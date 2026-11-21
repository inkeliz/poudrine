// Poudrine's public HTTP reader holds a descriptor for one snapshot.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
	"golang.org/x/net/netutil"
	"golang.org/x/sys/unix"
)

type counts struct {
	Packages int64 `json:"packages"`
	Ahead    int64 `json:"ahead"`
}

// A single pread pairs with the publisher's single fixed-width pwrite.
// Return only validated integers. Never serve bytes or errors from the file.
func readCounts(fd int) (counts, bool) {
	var buf [1025]byte
	n, err := unix.Pread(fd, buf[:], 0)
	if err != nil || n == 0 || n == len(buf) {
		return counts{}, false
	}
	d := json.NewDecoder(bytes.NewReader(buf[:n]))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return counts{}, false
	}
	result := counts{Packages: -1, Ahead: -1}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return counts{}, false
		}
		var value *int64
		switch key {
		case "packages":
			value = &result.Packages
		case "ahead":
			value = &result.Ahead
		default:
			return counts{}, false
		}
		if *value >= 0 || d.Decode(value) != nil || *value < 0 {
			return counts{}, false
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return counts{}, false
	}
	_, err = d.Token()
	return result, err == io.EOF && result.Ahead >= 0 && result.Ahead <= result.Packages
}

type statusHandler struct {
	fd     int
	nonces statusNonces
}

func (h *statusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/" && r.URL.Path != "/status.json" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		http.Error(w, "URL parameters are unsupported", http.StatusBadRequest)
		return
	}
	page := r.URL.Path != "/status.json" && !(r.URL.Path == "/" && r.Method == requestauth.Query)
	if !page {
		if r.Method != requestauth.Query {
			w.Header().Set("Allow", requestauth.Query)
			http.Error(w, "Use QUERY for statistics", http.StatusMethodNotAllowed)
			return
		}
		if !h.authorizeQuery(w, r) {
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		allow := "GET, HEAD"
		if r.URL.Path == "/" {
			allow += ", " + requestauth.Query
		}
		w.Header().Set("Allow", allow)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s, available := readCounts(h.fd)
	code := http.StatusOK
	if !available {
		code = http.StatusServiceUnavailable
	}
	var body []byte
	switch {
	case page:
		var err error
		body, err = renderStatusPage(s, available)
		if err != nil {
			http.Error(w, "Cannot render statistics", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	case available:
		body, _ = json.Marshal(s) // Two integers always marshal.
		body = append(body, '\n')
		w.Header().Set("Content-Type", "application/json")
	default:
		body = []byte("Statistics unavailable\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func openSnapshot(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err == nil && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1) {
		err = errors.New("status file must be a regular file with a single link")
	}
	if err == nil {
		rights, e := unix.CapRightsInit([]uint64{unix.CAP_PREAD})
		err = e
		if err == nil {
			err = unix.CapRightsLimit(uintptr(fd), rights)
		}
	}
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// The rc service inherits only standard I/O. Also discard extra descriptors
// inherited from an interactive launcher, especially any directory handles.
// Go opens its own runtime descriptors with close-on-exec.
func discardInheritedFiles() error {
	limit := unix.Getdtablesize()
	// A launcher can lower its descriptor limit after opening a file.
	// The Go runtime cannot allocate its own descriptors above this limit.
	if _, _, errno := unix.Syscall(unix.SYS_CLOSE_RANGE, uintptr(limit), ^uintptr(0), 0); errno != 0 {
		return errno
	}
	for fd := 3; fd < limit; fd++ {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err == unix.EBADF {
			continue
		}
		if err != nil {
			return err
		}
		if flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if err := unix.Close(fd); err != nil {
			return err
		}
	}
	// Standard descriptors may themselves be files or directories. Permit
	// diagnostics only on output pipes, sockets or terminals.
	for fd := 0; fd <= 2; fd++ {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err == unix.EBADF {
			continue
		} else if err != nil {
			return err
		}
		var caps []uint64
		kind := st.Mode & unix.S_IFMT
		if fd > 0 && (kind == unix.S_IFIFO || kind == unix.S_IFSOCK || kind == unix.S_IFCHR) {
			caps = []uint64{unix.CAP_WRITE}
		}
		rights, err := unix.CapRightsInit(caps)
		if err != nil {
			return err
		}
		if err := unix.CapRightsLimit(uintptr(fd), rights); err != nil {
			return err
		}
	}
	return nil
}

func run(path, address string) error {
	if os.Geteuid() == 0 || os.Getuid() == 0 {
		return errors.New("run as an unprivileged user")
	}
	addr, err := netip.ParseAddrPort(address)
	if err != nil || addr.Addr().Zone() != "" {
		return errors.New("listen address must be a numeric IP address and port")
	}
	if err := discardInheritedFiles(); err != nil {
		return fmt.Errorf("restrict inherited descriptors: %w", err)
	}
	fd, err := openSnapshot(path)
	if err != nil {
		return fmt.Errorf("open status file: %w", err)
	}
	defer unix.Close(fd)
	family := "tcp6"
	if addr.Addr().Is4() {
		family = "tcp4"
	}
	listener, err := net.ListenTCP(family, net.TCPAddrFromAddrPort(addr))
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := unix.CapEnter(); err != nil {
		return fmt.Errorf("enter Capsicum: %w", err)
	}
	server := http.Server{
		Handler: &statusHandler{fd: fd}, DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	return server.Serve(netutil.LimitListener(listener, 128))
}

func main() {
	addr := flag.String("listen", "[::]:8282", "numeric HTTP listen address and port")
	file := flag.String("status-file", "/var/run/poudrine-status/status.json", "catalog count snapshot")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*file, *addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
