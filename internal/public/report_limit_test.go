package public

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

type waitingReportBody struct {
	entered chan struct{}
	finish  <-chan struct{}
}

func (b *waitingReportBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.finish
	return 0, fmt.Errorf("test read stopped")
}
func (b *waitingReportBody) Close() error { return nil }

type unreadReportBody struct{ t *testing.T }

func (b unreadReportBody) Read([]byte) (int, error) {
	b.t.Error("rejected report body was read")
	return 0, fmt.Errorf("unexpected read")
}
func (b unreadReportBody) Close() error { return nil }

func TestInventoryReportAdmissionBeforeBodyRead(t *testing.T) {
	repo, _ := inventoryReportFixture(t)
	key := testutil.ReportProof().Key
	// Headers pass the inexpensive key check; authentication would fail after
	// the body. This exercises the unauthenticated memory admission boundary.
	req := func(ip string) *http.Request {
		r := httptest.NewRequest("POST", routes.ReportInventory.Path, strings.NewReader(""))
		r.RemoteAddr = ip + ":1234"
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("X-Poudrine-Key", base64.RawURLEncoding.EncodeToString(key))
		r.Header.Set("X-Poudrine-Signature", "invalid")
		return r
	}
	finish := make(chan struct{})
	done := make(chan struct{}, maxConcurrentReports)
	launched := 0
	defer func() {
		close(finish)
		for range launched {
			<-done
		}
	}()
	for i := 0; i < maxConcurrentReports; i++ {
		r := req(fmt.Sprintf("192.0.2.%d", i+1))
		body := &waitingReportBody{make(chan struct{}), finish}
		r.Body = body
		launched++
		go func() { repo.ServeHTTP(httptest.NewRecorder(), r); done <- struct{}{} }()
		select {
		case <-body.entered:
		case <-time.After(time.Second):
			t.Fatal("admitted report did not read body")
		}
		if i == 0 {
			same := req("192.0.2.1")
			same.Body = unreadReportBody{t}
			w := httptest.NewRecorder()
			repo.ServeHTTP(w, same)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("same IP: %d", w.Code)
			}
		}
	}
	r := req("192.0.2.99")
	r.Body = unreadReportBody{t}
	w := httptest.NewRecorder()
	repo.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("full admission: %d", w.Code)
	}
}
