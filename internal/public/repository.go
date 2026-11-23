package public

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	catalogpkg "github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/requestauth"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/weblocale"
	"golang.org/x/text/message"
)

const (
	maxReportBytes = 8 << 20
	maxReportLines = 65536
)

var errReportRows = errors.New("too many inventory entries")

var reportOriginProtection = http.NewCrossOriginProtection()

type inventoryReceipt struct {
	Reported  int `json:"reported"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
}

func parseReport(body io.Reader, catalog *catalogpkg.Snapshot, printer *message.Printer) (map[string]database.InstalledPackage, inventoryReceipt, error) {
	packages := make(map[string]database.InstalledPackage)
	var result inventoryReceipt
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 1024)
	line := 0
	for scanner.Scan() {
		line++
		if line > maxReportLines {
			return nil, result, errReportRows
		}
		fields := strings.FieldsFunc(scanner.Text(), func(r rune) bool { return r == ' ' || r == '\t' })
		if len(fields) != 3 {
			return nil, result, errors.New(printer.Sprintf("line %d: expected package name, installed version and origin", line))
		}
		name, version, origin := fields[0], fields[1], fields[2]
		if !packageinfo.ValidInventoryPackage(name, version, origin) {
			return nil, result, errors.New(printer.Sprintf("line %d: invalid package name, version or origin", line))
		}
		if old, ok := packages[name]; ok {
			if old.Version != version || old.Origin != origin {
				return nil, result, errors.New(printer.Sprintf("line %d: conflicting entries for the same package", line))
			}
			continue
		}
		installed := database.InstalledPackage{Version: version, Origin: origin}
		// Only the current catalog supplies a Poudriere build identifier.
		// Unknown packages and pkgbase entries still belong in the client list.
		if pkg, ok := catalog.ByPath[catalog.PathByName[name]]; ok && pkg.Origin == origin && !strings.HasPrefix(origin, "base/") {
			if len(pkg.BuildOrigin) <= 255 && packageinfo.BuildOrigin.MatchString(pkg.BuildOrigin) {
				installed.BuildOrigin = pkg.BuildOrigin
				result.Matched++
			}
		}
		packages[name] = installed
	}
	result.Reported = len(packages)
	result.Unmatched = result.Reported - result.Matched
	return packages, result, scanner.Err()
}

func (s *Repository) reportInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RequestURI() != routes.ReportInventory.Path {
		http.Error(w, "use the inventory path without URL parameters", http.StatusBadRequest)
		return
	}
	if r.Method != routes.ReportInventory.Method {
		w.Header().Set("Allow", routes.ReportInventory.Method)
		http.Error(w, "use POST to report installed packages", http.StatusMethodNotAllowed)
		return
	}
	if reportOriginProtection.Check(r) != nil {
		http.Error(w, "cross-origin inventory reports are forbidden", http.StatusForbidden)
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "text/plain" {
		http.Error(w, "send pkg query output as text/plain", http.StatusUnsupportedMediaType)
		return
	}
	key := requestauth.PublicKey(r.Header.Get("X-Poudrine-Key"))
	if key == nil || r.Header.Get("X-Poudrine-Signature") == "" {
		http.Error(w, "a signed Ed25519 inventory report is required", http.StatusUnauthorized)
		return
	}
	printer := message.NewPrinter(weblocale.ForRequest(r))
	reportTooLarge := func() {
		http.Error(w, printer.Sprintf("inventory exceeds %d MiB", maxReportBytes>>20), http.StatusRequestEntityTooLarge)
	}
	if r.ContentLength > maxReportBytes {
		reportTooLarge()
		return
	}
	ip := s.clientIP(r)
	release, admitted := s.admitReport(ip)
	if !admitted {
		// Do not drain an untrusted oversized or streaming body after rejection.
		r.Close = true
		w.Header().Set("Connection", "close")
		w.Header().Set("Retry-After", "10")
		http.Error(w, "inventory reports are busy; retry shortly", http.StatusTooManyRequests)
		return
	}
	defer release()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReportBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			reportTooLarge()
		} else {
			http.Error(w, "cannot read the inventory report", http.StatusBadRequest)
		}
		return
	}
	nonce, stamp, valid := requestauth.Verify(key, r.Method, routes.ReportInventory.Path, r.Header.Get("X-Poudrine-Time"),
		r.Header.Get("X-Poudrine-Nonce"), r.Header.Get("X-Poudrine-Signature"), body)
	if !valid {
		http.Error(w, "invalid inventory report signature", http.StatusUnauthorized)
		return
	}
	snapshot := s.Catalog.Current.Load()
	if snapshot == nil {
		http.Error(w, "repository catalog is initializing; retry the report when it is ready", http.StatusServiceUnavailable)
		return
	}
	packages, result, err := parseReport(bytes.NewReader(body), snapshot, printer)
	if err != nil {
		if errors.Is(err, errReportRows) {
			http.Error(w, printer.Sprintf("inventory exceeds %d packages", maxReportLines), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	if err := s.Store.ReplaceReport(r.Context(), ip, requestauth.ReportProof{Key: key, Nonce: nonce, Stamp: stamp}, packages); err != nil {
		switch {
		case errors.Is(err, database.ErrReportKey):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, database.ErrReportReplay):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, database.ErrReportExpired):
			http.Error(w, err.Error(), http.StatusUnauthorized)
		default:
			log.Printf("record installed packages: %v", err)
			http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("inventory report response: %v", err)
	}
}

type Repository struct {
	Catalog   *catalogpkg.Catalog `json:"-"`
	Store     database.DB         `json:"-"`
	Cache     *catalogpkg.Cache   `json:"-"`
	LocalDir  string              `json:"-"`
	Proxy     *net.IPNet          `json:"-"`
	activity  chan clientActivity
	reportMu  sync.Mutex
	reporting map[string]struct{}
}

func (s *Repository) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.Proxy != nil && s.Proxy.Contains(net.ParseIP(host)) {
		if ip := net.ParseIP(r.Header.Get("X-Real-IP")); ip != nil {
			return ip.String()
		}
	}
	return host
}

func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == routes.ReportInventory.Path {
		s.reportInventory(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if !filepath.IsLocal(rel) || rel != path.Clean(rel) || rel == "." {
		http.NotFound(w, r)
		return
	}
	ip := s.clientIP(r)
	if s.Catalog.Metadata(w, r, rel) {
		s.recordActivity(ip, nil)
		return
	}
	p, ok := s.Catalog.Package(rel)
	var f *os.File
	if !ok {
		// Only previously advertised immutable URLs survive a catalog change.
		sum := catalogpkg.LiveDownloadDigest(rel)
		if sum == "" {
			http.NotFound(w, r)
			return
		}
		var err error
		f, err = s.Catalog.OpenLiveDownload(sum)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		m, err := catalogpkg.ReadLiveManifest(f, sum)
		if err != nil {
			http.Error(w, "package unavailable", http.StatusBadGateway)
			return
		}
		p = m.Pkg
		// A retained URL can outlive a configuration change or service
		// restart. Its archive carries the settings used for that build.
		snapshot := s.Catalog.Current.Load()
		if snapshot == nil && len(s.Catalog.Overlays) > 0 {
			// Until the current catalog resolves names to origins, we cannot
			// distinguish a port move from a different provider of an overlay.
			http.Error(w, "repository catalog is initializing", http.StatusServiceUnavailable)
			return
		}
		verifiedOverlayBuild := strings.HasPrefix(filepath.Base(f.Name()), catalogpkg.LiveOverlayPrefix)
		if !snapshot.PermitsLocal(p, !verifiedOverlayBuild) {
			http.NotFound(w, r)
			return
		}
	}
	isSignature := catalogpkg.BootstrapSignaturePath(rel)
	size := p.Size
	if isSignature {
		size = int64(s.Catalog.Key.Size())
	} else {
		w.Header().Set("ETag", fmt.Sprintf("%q", p.Sum))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == "HEAD" {
		s.recordActivity(ip, nil)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}
	var err error
	if p.Local {
		if !filepath.IsLocal(p.Path) {
			http.Error(w, "invalid local path", 502)
			return
		}
		if f == nil {
			if p.Live {
				f, err = s.Catalog.OpenLiveDownload(catalogpkg.LiveDownloadDigest(p.Path))
			} else {
				f, err = os.Open(filepath.Join(s.LocalDir, filepath.FromSlash(p.Path)))
			}
			if err == nil {
				defer f.Close()
			}
		}
		if err == nil {
			err = catalogpkg.VerifyPackage(f, io.Discard, p)
			if err == nil {
				_, err = f.Seek(0, io.SeekStart)
			}
		}
	} else {
		f, err = s.Cache.Open(r.Context(), p)
		if err == nil {
			defer f.Close()
		}
	}
	if err != nil {
		log.Printf("serve %q: %v", p.Path, err)
		http.Error(w, "package unavailable", 502)
		return
	}
	var content io.ReadSeeker = f
	tracked := &p
	if isSignature {
		sig, err := s.Catalog.BootstrapSignature(f, p)
		if err != nil {
			log.Printf("sign bootstrap package: %v", err)
			http.Error(w, "signature unavailable", 502)
			return
		}
		content = bytes.NewReader(sig)
		tracked = nil
	}
	s.recordActivity(ip, tracked)
	http.ServeContent(w, r, path.Base(rel), time.Time{}, content)
}
