package service

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"golang.org/x/sys/unix"
)

const serviceRestartTimeout = 2 * time.Minute

// An idle supervisor can exec immediately. Its caller must wait for the
// successor to open its control socket after the new web service is ready.
var errWaitForService = errors.New("waiting for updated service generation")

type restartFlight struct {
	done chan struct{}
	err  error
}

type serviceRestarts struct {
	ctx      context.Context
	cancel   context.CancelFunc
	restart  func(context.Context) error
	listener *net.UnixListener
	path     string
	inode    os.FileInfo
	lock     *os.File
	mu       sync.Mutex
	flight   *restartFlight
	close    sync.Once
	closed   chan struct{}
	token    string
}

func restartDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", errors.New("restart socket requires a clean absolute path")
	}
	if len(path) >= len(syscall.RawSockaddrUnix{}.Path) {
		return "", errors.New("restart socket path exceeds the system limit")
	}
	dir := filepath.Dir(path)
	if os.Geteuid() == 0 {
		_, exists, err := config.InspectRootPath(dir, true, true)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", os.ErrNotExist
		}
	} else {
		// Permit an isolated unprivileged instance to use its own directory.
		info, err := os.Stat(dir)
		if err != nil {
			return "", err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st == nil || !info.IsDir() || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
			return "", errors.New("restart socket needs a protected service-owned directory")
		}
	}
	return dir, nil
}

func privateRestartDirectory(dir string) (string, error) {
	// Leave room for "/.<random>/s" and the sockaddr_un NUL even when a
	// custom pidfile is close to the kernel's socket path length limit.
	n := min(10, len(syscall.RawSockaddrUnix{}.Path)-len(dir)-5)
	if n < 1 {
		return "", errors.New("restart socket directory path exceeds the system limit")
	}
	for range 32 {
		private := filepath.Join(dir, "."+rand.Text()[:n])
		err := os.Mkdir(private, 0700)
		if !errors.Is(err, os.ErrExist) {
			return private, err
		}
	}
	return "", errors.New("could not reserve a private restart socket directory")
}

// Retain the lock inode across restarts. Its flock distinguishes a dead
// generation from a live one even if both start or probe at the same time.
func listenServiceRestarts(parent context.Context, path string, restart func(context.Context) error) (io.Closer, error) {
	dir, err := restartDirectory(path)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	info, err := lock.Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
			err = errors.New("unsafe restart socket lock")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock restart socket: %w", err)
	}
	info, err = os.Lstat(path)
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st == nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() {
			err = errors.New("refusing to replace an unexpected file at the restart socket")
			return nil, err
		}
		// Never take over an unrelated live listener at this location.
		conn, probeErr := net.DialTimeout("unix", path, time.Second)
		if probeErr == nil {
			conn.Close()
			err = errors.New("restart socket is already in use")
			return nil, err
		}
		if !errors.Is(probeErr, syscall.ECONNREFUSED) && !errors.Is(probeErr, os.ErrNotExist) {
			err = probeErr
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Bind inside a private directory, set the final mode, then publish
	// atomically. No connection can be queued under the process's umask.
	private, err := privateRestartDirectory(dir)
	if err != nil {
		return nil, err
	}
	defer os.Remove(private)
	tmp := filepath.Join(private, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	defer func() {
		if err != nil {
			listener.Close()
			os.Remove(tmp)
		}
	}()
	if err = os.Chmod(tmp, 0600); err != nil {
		return nil, err
	}
	info, err = os.Lstat(tmp)
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &serviceRestarts{ctx: ctx, cancel: cancel, restart: restart, listener: listener, path: path, inode: info, lock: lock, closed: make(chan struct{}), token: rand.Text()}
	go s.serve()
	go func() {
		select {
		case <-parent.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	return s, nil
}

func (s *serviceRestarts) serve() {
	var clients sync.WaitGroup
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			break
		}
		clients.Go(func() {
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			request, err := bufio.NewReaderSize(conn, 128).ReadSlice('\n')
			if err != nil {
				return
			}
			reply := "OK"
			switch {
			case strings.HasPrefix(string(request), "ready "):
				if string(request) == "ready "+s.token+"\n" {
					reply = "WAIT " + s.token
				}
			case string(request) != "restart\n":
				reply = "ERROR unknown service command"
			case s.ctx.Err() != nil:
				reply = "ERROR service is stopping"
			default:
				if err := s.run(); errors.Is(err, errWaitForService) {
					reply = "WAIT " + s.token
				} else if err != nil {
					message := strings.Join(strings.Fields(err.Error()), " ")
					reply = "ERROR " + strings.ToValidUTF8(message[:min(len(message), 2048)], "")
				}
			}
			conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			fmt.Fprintln(conn, reply)
		})
	}
	clients.Wait() // Complete acknowledgements before permitting re-exec.
	close(s.closed)
}

func (s *serviceRestarts) run() error {
	s.mu.Lock()
	if f := s.flight; f != nil {
		s.mu.Unlock()
		<-f.done
		return f.err
	}
	f := &restartFlight{done: make(chan struct{})}
	s.flight = f
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, serviceRestartTimeout)
	f.err = s.restart(ctx)
	cancel()
	s.mu.Lock()
	close(f.done)
	s.flight = nil
	s.mu.Unlock()
	return f.err
}

func (s *serviceRestarts) Close() error {
	s.close.Do(func() {
		s.listener.Close()
		s.cancel()
		<-s.closed
		if info, err := os.Lstat(s.path); err == nil && os.SameFile(s.inode, info) {
			os.Remove(s.path)
		}
		s.lock.Close()
	})
	return nil
}

func exchangeServiceRestart(path, command string, deadline time.Time) (string, error) {
	conn, err := net.DialTimeout("unix", path, min(3*time.Second, time.Until(deadline)))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(deadline)
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		return "", err
	}
	reply, err := bufio.NewReaderSize(conn, 4096).ReadSlice('\n')
	if err != nil {
		return "", fmt.Errorf("restart acknowledgement: %w", err)
	}
	line := strings.TrimSuffix(string(reply), "\n")
	if message, ok := strings.CutPrefix(line, "ERROR "); ok {
		return "", errors.New(message)
	}
	return line, nil
}

func requestServiceRestart(path string) error {
	deadline := time.Now().Add(serviceRestartTimeout + 10*time.Second)
	reply, err := exchangeServiceRestart(path, "restart", deadline)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("restart control unavailable at %s. If the service is starting, try again shortly. For the first upgrade from an older release, let the active build finish, then run service poudrine stop and service poudrine start", path)
	}
	if err != nil {
		return err
	}
	if reply == "OK" {
		return nil
	}
	token, wait := strings.CutPrefix(reply, "WAIT ")
	if !wait || token == "" || len(token) > 80 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("unexpected restart acknowledgement")
	}
	// A released socket inode may be reused immediately after exec. Check
	// the generation token instead, using a read-only probe on each retry.
	for time.Now().Before(deadline) {
		next, err := exchangeServiceRestart(path, "ready "+token, deadline)
		switch {
		case err == nil && next == "OK":
			return nil
		case err == nil && next != reply:
			return errors.New("unexpected service readiness response")
		case err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) &&
			!errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) && !errors.Is(err, io.EOF):
			return fmt.Errorf("waiting for updated service: %w", err)
		}
		time.Sleep(min(100*time.Millisecond, time.Until(deadline)))
	}
	return errors.New("timed out waiting for the updated service to become ready")
}

// The daemon still supervises the same PID across exec. Transfer the idle
// worker's queue and deadlines through a private, unlinked file descriptor.
type serviceUpgrade struct {
	exe    string
	worker *builder.Builder
}

func (*serviceUpgrade) Error() string { return "service binary ready for replacement" }

const checkpointFDEnv = "POUDRINE_RESTART_FD"

type serviceCheckpoint struct {
	Config, State, PoudriereConfig, BaseFS, Jail, Ports, PackagesFile string
	Worker                                                            *builder.WorkerCheckpoint
}

func checkpointIdentity(o config.Options) serviceCheckpoint {
	return serviceCheckpoint{Config: o.ConfigPath, State: o.State, PoudriereConfig: o.Builder.ConfigDir,
		BaseFS: o.Builder.BaseFS, Jail: o.Builder.Jail, Ports: o.Builder.Ports, PackagesFile: o.Builder.PackagesFile}
}

func checkpointFile(o config.Options, cp *builder.WorkerCheckpoint) (*os.File, error) {
	// The restart socket already has a protected directory, even if the
	// worker is still installing prerequisites. A regular file permits queues
	// larger than a pipe or the exec argument/environment size limits.
	dir := o.Builder.ConfigDir
	if socket := os.Getenv("POUDRINE_RESTART_SOCKET"); socket != "" {
		dir = filepath.Dir(socket)
	}
	f, err := os.CreateTemp(dir, ".poudrine-restart-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	state := checkpointIdentity(o)
	state.Worker = cp
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readServiceCheckpoint(o config.Options) (*builder.WorkerCheckpoint, error) {
	value, ok := os.LookupEnv(checkpointFDEnv)
	if !ok {
		return nil, nil
	}
	os.Unsetenv(checkpointFDEnv)
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return nil, errors.New("invalid inherited service checkpoint descriptor")
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "service-checkpoint")
	if f == nil {
		return nil, errors.New("missing service checkpoint")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() || st.Nlink != 0 {
		return nil, errors.New("unexpected inherited service checkpoint")
	}
	var state serviceCheckpoint
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return nil, fmt.Errorf("read service checkpoint: %w", err)
	}
	cp := state.Worker
	state.Worker = nil
	if state != checkpointIdentity(o) {
		log.Print("worker paths or jail changed; initializing the new worker schedule")
		return nil, nil
	}
	return cp, nil
}

func (u *serviceUpgrade) exec(o config.Options, cp *builder.WorkerCheckpoint) error {
	f, err := checkpointFile(o, cp)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := f.Fd()
	if _, err := unix.FcntlInt(fd, unix.F_SETFD, 0); err != nil {
		return err
	}
	env := append(os.Environ(), checkpointFDEnv+"="+strconv.Itoa(int(fd)))
	log.Printf("starting updated service from %s", u.exe)
	return syscall.Exec(u.exe, []string{u.exe, "run", "-config", o.ConfigPath}, env)
}
