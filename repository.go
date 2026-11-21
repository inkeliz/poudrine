package poudrine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type repository struct {
	catalog  *Catalog
	store    inventory
	cache    *packageCache
	localDir string
	proxy    *net.IPNet
}

func (s *repository) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.proxy != nil && s.proxy.Contains(net.ParseIP(host)) {
		if ip := net.ParseIP(r.Header.Get("X-Real-IP")); ip != nil {
			return ip.String()
		}
	}
	return host
}

func (s *repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == reportPath {
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
	if s.catalog.Metadata(w, r, rel) {
		// The response has already been sent, so the client can disconnect
		// before SQLite commits this visit.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if err := s.store.record(ctx, ip, nil); err != nil {
			log.Printf("record metadata request: %v", err)
		}
		return
	}
	p, ok := s.catalog.Package(rel)
	var f *os.File
	if !ok {
		// Only previously advertised immutable URLs survive a catalog change.
		sum := liveDownloadDigest(rel)
		if sum == "" {
			http.NotFound(w, r)
			return
		}
		var err error
		f, err = s.catalog.openLiveDownload(sum)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		m, err := readLiveManifest(f, sum)
		if err != nil {
			http.Error(w, "package unavailable", http.StatusBadGateway)
			return
		}
		p = m.pkg
		// A retained URL can outlive a configuration change or service
		// restart. Its archive carries the settings used for that build.
		snapshot := s.catalog.current.Load()
		if snapshot == nil && len(s.catalog.overlays) > 0 {
			// Until the current catalog resolves names to origins, we cannot
			// distinguish a port move from a different provider of an overlay.
			http.Error(w, "repository catalog is initializing", http.StatusServiceUnavailable)
			return
		}
		verifiedOverlayBuild := strings.HasPrefix(filepath.Base(f.Name()), liveOverlayPrefix)
		if !snapshot.permitsLocal(p, !verifiedOverlayBuild) {
			http.NotFound(w, r)
			return
		}
	}
	isSignature := bootstrapSignaturePath(rel)
	size := p.Size
	if isSignature {
		size = int64(s.catalog.key.Size())
	} else {
		w.Header().Set("ETag", fmt.Sprintf("%q", p.Sum))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == "HEAD" {
		if err := s.store.record(r.Context(), ip, nil); err != nil {
			http.Error(w, "inventory unavailable", 503)
			return
		}
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
			if p.live {
				f, err = s.catalog.openLiveDownload(liveDownloadDigest(p.Path))
			} else {
				f, err = os.Open(filepath.Join(s.localDir, filepath.FromSlash(p.Path)))
			}
			if err == nil {
				defer f.Close()
			}
		}
		if err == nil {
			err = verifyPackage(f, io.Discard, p)
			if err == nil {
				_, err = f.Seek(0, io.SeekStart)
			}
		}
	} else {
		f, err = s.cache.open(r.Context(), p)
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
		sig, err := s.catalog.bootstrapSignature(f, p)
		if err != nil {
			log.Printf("sign bootstrap package: %v", err)
			http.Error(w, "signature unavailable", 502)
			return
		}
		content = bytes.NewReader(sig)
		tracked = nil
	}
	if err := s.store.record(r.Context(), ip, tracked); err != nil {
		log.Printf("record package request: %v", err)
		http.Error(w, "inventory unavailable", 503)
		return
	}
	http.ServeContent(w, r, path.Base(rel), time.Time{}, content)
}
