package poudrine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A compiled archive can replace several cached versions. Hash its bytes
// once, and reuse the result while the file and declared checksum are unchanged.
type compiledArchive struct {
	path     string
	info     os.FileInfo
	mu       sync.Mutex
	verified bool
}

func sameArchive(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func fetchedByPoudriere(dir, path string, archive os.FileInfo) (bool, error) {
	base := filepath.Base(path)
	names := []string{base}
	// pkg repo may move the hardlink into All/Hashed/name-version~digest.pkg;
	// Poudriere originally linked it from .pkg-cache/name-version.pkg.
	if filepath.Base(filepath.Dir(path)) == "Hashed" {
		if split := strings.LastIndexByte(base, '~'); split > 0 {
			names = append(names, base[:split]+filepath.Ext(base))
		}
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, ".pkg-cache", name))
		if err == nil && os.SameFile(archive, info) {
			return true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (a *compiledArchive) verify(p CatalogPackage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !sameArchive(a.info, info) {
		return errUntrustedPackage
	}
	if !a.verified {
		if p.overlayDigest != "" {
			// A native repository index can outlive or mislabel the archive.
			// Check the provenance that was written into the package itself.
			raw, err := liveCompactJSON(f)
			if err != nil {
				return err
			}
			var built struct {
				Name, Origin, Version string
				Annotations           packageAnnotations
			}
			if err := json.Unmarshal(raw, &built); err != nil {
				return err
			}
			if built.Annotations.OverlayDigest != p.overlayDigest || built.Name != p.Name || built.Version != p.Version ||
				packageBuildOrigin(built.Origin, built.Annotations.Flavor, built.Annotations.Subpackage) != p.BuildOrigin {
				return errUntrustedPackage
			}
		}
		if err := verifyPackage(f, io.Discard, p); err != nil {
			return err
		}
		a.verified = true
	}
	return nil
}

// HTTP validators belong to one verified upstream archive. The signed digest
// also identifies an unchanged catalog on mirrors that always return HTTP 200.
type catalogRevision struct {
	archive, resource, etag, modified string
	fingerprint, signature            string
}

func (c *Catalog) openLocalIndex() (*os.File, os.FileInfo, error) {
	if c.localDir != "" {
		for _, name := range []string{"data.pkg", "data.tzst", "data.txz", "packagesite.pkg", "packagesite.txz", "packagesite.tzst"} {
			f, err := os.Open(filepath.Join(c.localDir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			return f, info, nil
		}
	}
	return nil, nil, nil
}

func sameLocalIndex(old, index os.FileInfo) bool {
	if index == nil || old == nil {
		return index == nil && old == nil
	}
	return sameArchive(old, index)
}

// A native commit is a local publication event even when there were no
// captured packages, or the next upstream HTTP request has not completed.
func (c *Catalog) publicationInputsChanged() bool {
	f, info, err := c.openLocalIndex()
	if err != nil {
		return true // PublishLive reports the read error to Management.
	}
	if f != nil {
		f.Close()
	}
	c.publish.Lock()
	defer c.publish.Unlock()
	return !c.samePublicationInputs(info)
}

func (c *Catalog) samePublicationInputs(index os.FileInfo) bool {
	return sameLocalIndex(c.localIndex, index) &&
		(len(c.live) == 0 || maps.Equal(c.baseLibraries, jailBaseLibraries(c.jailRoot)))
}

func (c *Catalog) localInputsUnchanged(index os.FileInfo) bool {
	if !c.samePublicationInputs(index) {
		return false
	}
	// Even with the same index, a previously missing local archive might
	// have appeared, or an archive selected for publication disappeared.
	for path, old := range c.localArchives {
		fi, err := os.Stat(filepath.Join(c.localDir, path))
		if old == nil && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || old == nil || !sameArchive(fi, old) {
			return false
		}
	}
	return true
}

// A nil result means the existing merged snapshot still applies. Only send
// validators if both that snapshot's local inputs and its signing trust hold.
func (c *Catalog) fetchCatalog(ctx context.Context, previous catalogRevision) (*repoContent, catalogRevision, error) {
	for _, spec := range []struct{ archive, member string }{{"data.pkg", "data"}, {"packagesite.pkg", "packagesite.yaml"}, {"packagesite.txz", "packagesite.yaml"}, {"packagesite.tzst", "packagesite.yaml"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.upstream.JoinPath(spec.archive).String(), nil)
		if err != nil {
			return nil, catalogRevision{}, err
		}
		req.Header.Set("Cache-Control", "no-cache")
		conditional := previous.archive == spec.archive && (previous.etag != "" || previous.modified != "") &&
			checkUpstreamFingerprint(c.fingerprints, previous.fingerprint) == nil
		if conditional {
			if previous.etag != "" {
				req.Header.Set("If-None-Match", previous.etag)
			}
			if previous.modified != "" {
				req.Header.Set("If-Modified-Since", previous.modified)
			}
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, catalogRevision{}, err
		}
		// A mirror redirect can change between polls. A different resource
		// may use the same opaque ETag or modification date for other data.
		if conditional && resp.StatusCode == http.StatusNotModified && resp.Request.URL.String() != previous.resource {
			resp.Body.Close()
			req.Header.Del("If-None-Match")
			req.Header.Del("If-Modified-Since")
			conditional = false
			resp, err = c.client.Do(req)
			if err != nil {
				return nil, catalogRevision{}, err
			}
		}
		switch resp.StatusCode {
		case http.StatusNotFound:
			resp.Body.Close()
			continue
		case http.StatusNotModified:
			resp.Body.Close()
			if !conditional {
				return nil, catalogRevision{}, fmt.Errorf("upstream %s: unsolicited HTTP 304", spec.archive)
			}
			// Trust can change while the request is in flight.
			if err := checkUpstreamFingerprint(c.fingerprints, previous.fingerprint); err != nil {
				return nil, catalogRevision{}, err
			}
			return nil, previous, nil
		case http.StatusOK:
		default:
			resp.Body.Close()
			return nil, catalogRevision{}, fmt.Errorf("upstream %s: HTTP %d", spec.archive, resp.StatusCode)
		}

		m, err := unpackCatalog(io.LimitReader(resp.Body, maxCatalogBytes), spec.member)
		resp.Body.Close()
		if err != nil {
			return nil, catalogRevision{}, err
		}
		if err = verifyCatalog(m, spec.member, c.fingerprints); err != nil {
			return nil, catalogRevision{}, fmt.Errorf("authenticate %s: %w", spec.archive, err)
		}
		revision := catalogRevision{
			archive: spec.archive, resource: resp.Request.URL.String(), etag: resp.Header.Get("ETag"),
			fingerprint: fmt.Sprintf("%x", sha256.Sum256(m[spec.member+".pub"])),
			signature:   string(m[spec.member+".sig"]),
		}
		// "*" is a request wildcard, never an entity tag for this catalog.
		if revision.etag == "*" {
			revision.etag = ""
		}
		// Last-Modified has one-second resolution. A fresh upload can still
		// change within that second. Wait for an older response before using
		// the date as a validator (RFC 9110 section 8.8.2.2).
		date, dateErr := http.ParseTime(resp.Header.Get("Date"))
		modified, modifiedErr := http.ParseTime(resp.Header.Get("Last-Modified"))
		if dateErr == nil && modifiedErr == nil && date.Sub(modified) >= time.Minute {
			revision.modified = resp.Header.Get("Last-Modified")
		}
		if previous.archive == revision.archive && previous.fingerprint == revision.fingerprint && previous.signature == revision.signature {
			return nil, revision, nil
		}
		repo, err := parseCatalog(m[spec.member], spec.member, false)
		return &repo, revision, err
	}
	return nil, catalogRevision{}, errors.New("no upstream catalog")
}
