package public

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	catalogpkg "github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// ServeContent selects the response after evaluating ranges and preconditions.
// pkg already has the signed archive checksum. Stream immediately, hash complete
// disk responses as they pass, and let pkg verify a file assembled from ranges.
type archiveResponse struct {
	http.ResponseWriter
	checksum  *catalogpkg.PackageChecksum
	status    int
	remaining int64
	last      byte
	pending   bool
	err       error
}

func (w *archiveResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status == http.StatusPartialContent {
		fullSize := w.remaining
		w.remaining, w.err = strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
		if w.Header().Get("Content-Range") == "" || w.remaining != fullSize {
			// Multipart framing and partial archive data cannot be compared
			// with a digest of the complete file.
			w.checksum = nil
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *archiveResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status != http.StatusOK && w.status != http.StatusPartialContent {
		return w.ResponseWriter.Write(p)
	}
	if w.err != nil {
		return 0, w.err
	}
	size := len(p)
	if int64(size) > w.remaining {
		w.err = errors.New("response exceeds declared length")
		return 0, w.err
	}
	if w.checksum != nil {
		w.checksum.Write(p)
	}
	w.remaining -= int64(size)
	if w.remaining == 0 && size > 0 {
		w.last, w.pending = p[size-1], true
		p = p[:size-1]
	}
	if len(p) > 0 {
		n, err := w.ResponseWriter.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.err = err
			return n, err
		}
	}
	return size, nil
}

func (s *Repository) serveArchive(w http.ResponseWriter, r *http.Request, rel string, p packageinfo.Package, f *os.File, stream *catalogpkg.ArchiveReader) {
	response := &archiveResponse{ResponseWriter: w, remaining: p.Size}
	var src io.ReadSeeker
	var verify func() error
	if f == nil {
		src = stream
		verify = stream.Verify
	} else {
		sum, err := catalogpkg.NewPackageChecksum(p.Sum)
		if err != nil {
			http.Error(w, "invalid archive checksum", http.StatusBadGateway)
			return
		}
		response.checksum = sum
		src = io.NewSectionReader(f, 0, p.Size)
		verify = func() error {
			if response.checksum == nil || response.checksum.Matches() {
				return nil
			}
			err := errors.New("archive checksum differs from catalog")
			if !p.Local {
				err = errors.Join(err, s.Cache.DiscardFile(p, f))
			}
			return err
		}
	}
	// A zero-length response is complete when the headers reach the client.
	if p.Size == 0 {
		if err := verify(); err != nil {
			log.Printf("stream %q: %v", rel, err)
			http.Error(w, "package unavailable", http.StatusBadGateway)
			return
		}
	}
	http.ServeContent(response, r, path.Base(rel), time.Time{}, src)
	if response.status != http.StatusOK && response.status != http.StatusPartialContent {
		return
	}
	if response.err != nil || response.remaining != 0 {
		panic(http.ErrAbortHandler)
	}
	if p.Size != 0 {
		if err := verify(); err != nil {
			log.Printf("stream %q: %v", rel, err)
			panic(http.ErrAbortHandler)
		}
	}
	if response.pending {
		if n, err := w.Write([]byte{response.last}); err != nil || n != 1 {
			panic(http.ErrAbortHandler)
		}
	}
}
