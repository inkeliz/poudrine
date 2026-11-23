package public

import (
	"io"
	"log"
	"net/http"
	"path"
	"time"

	catalogpkg "github.com/inkeliz/poudrine/internal/catalog"
)

// Keep one response byte until verification. This also covers short and
// multipart ranges: no successful HTTP body can complete on unverified data.
// Earlier bytes may already be on the wire; pkg must still verify its catalog
// checksum. A late upstream failure aborts the connection, not a second status.
type verifiedResponse struct {
	http.ResponseWriter
	status  int
	last    byte
	pending bool
	err     error
}

func (w *verifiedResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *verifiedResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status != http.StatusOK && w.status != http.StatusPartialContent {
		return w.ResponseWriter.Write(p)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if w.pending {
		n, err := w.ResponseWriter.Write([]byte{w.last})
		if err == nil && n != 1 {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.err = err
			return 0, err
		}
	}
	n, err := w.ResponseWriter.Write(p[:len(p)-1])
	if err == nil && n != len(p)-1 {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.pending = false
		w.err = err
		return n, err
	}
	w.last, w.pending = p[len(p)-1], true
	return len(p), nil
}

func serveStream(w http.ResponseWriter, r *http.Request, rel string, content *catalogpkg.ArchiveReader) {
	response := &verifiedResponse{ResponseWriter: w}
	http.ServeContent(response, r, path.Base(rel), time.Time{}, content)
	if response.status != http.StatusOK && response.status != http.StatusPartialContent {
		return
	}
	if response.err != nil {
		panic(http.ErrAbortHandler)
	}
	if err := content.Verify(); err != nil {
		log.Printf("stream %q: %v", rel, err)
		panic(http.ErrAbortHandler)
	}
	if response.pending {
		if _, err := w.Write([]byte{response.last}); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
