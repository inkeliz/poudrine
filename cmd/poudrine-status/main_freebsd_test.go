package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func signedStatusRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("QUERY", path, strings.NewReader(body))
	now := strconv.FormatInt(time.Now().Unix(), 10)
	var nonce [16]byte
	rand.Read(nonce[:])
	encode := base64.RawURLEncoding.EncodeToString
	nonceText := encode(nonce[:])
	prefix := strings.Join([]string{"poudrine-v2", "QUERY", path, now, nonceText, ""}, "\n")
	r.Header.Set("X-Poudrine-Time", now)
	r.Header.Set("X-Poudrine-Nonce", nonceText)
	r.Header.Set("X-Poudrine-Key", encode(pub))
	r.Header.Set("X-Poudrine-Signature", encode(ed25519.Sign(key, []byte(prefix+body))))
	return r
}

func TestStatusReplies(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "status.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := openSnapshot(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	set := func(data string) {
		t.Helper()
		if err := file.Truncate(0); err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte(data), 0); err != nil {
			t.Fatal(err)
		}
	}
	h := &statusHandler{fd: fd}
	reply := func(method, path, accept string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		if method == "QUERY" {
			r = signedStatusRequest(t, path, "{}")
		}
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	for _, raw := range []string{"", "bad secret data", `{"packages":0}`, `{"packages":0,"ahead":1}`, `{"packages":-1,"ahead":0}`, `{"packages":2,"ahead":null}`, `{"packages":2,"ahead":1,"ahead":0}`, `{"packages":1,"ahead":0,"private":"secret"}`, `{"packages":1,"ahead":0}[]`, strings.Repeat(" ", 1024) + `{"packages":1,"ahead":0}`} {
		set(raw)
		w := reply("QUERY", "/status.json", "", http.StatusServiceUnavailable)
		if w.Body.String() != "Statistics unavailable\n" {
			t.Fatal("invalid snapshot appeared in public response")
		}
	}
	set(`{"packages":7,"ahead":1,"extra":"sensitive snapshot content"}`)
	unavailable := reply("GET", "/", "", http.StatusServiceUnavailable).Body.String()
	if !strings.Contains(unavailable, "Statistics unavailable") || strings.Count(unavailable, ">—</dd>") != 2 || strings.Contains(unavailable, "sensitive snapshot content") {
		t.Fatal("HTML must show unavailable counts without exposing invalid snapshot contents")
	}
	for _, want := range []counts{{0, 0}, {38755, 1234}} {
		set(fmt.Sprintf(`{"ahead":%20d,"packages":%20d}`, want.Ahead, want.Packages))
		w := reply("QUERY", "/", "application/json", http.StatusOK)
		b, _ := json.Marshal(want)
		if !bytes.Equal(w.Body.Bytes(), append(b, '\n')) || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected public response %q", w.Body.Bytes())
		}
		page := reply("GET", "/", "text/html,application/xhtml+xml,*/*;q=0.8", http.StatusOK)
		if page.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatal("browser did not receive the HTML representation")
		}
		for _, accept := range []string{"", "*/*", "application/json"} {
			got := reply("GET", "/", accept, http.StatusOK)
			if !bytes.Equal(got.Body.Bytes(), page.Body.Bytes()) || got.Header().Get("Content-Type") != page.Header().Get("Content-Type") {
				t.Fatalf("expected the HTML page for Accept %q", accept)
			}
		}
		numbers := []string{">38,755</dd>", ">1,234</dd>"}
		if want.Packages == 0 {
			if strings.Count(page.Body.String(), ">0</dd>") != 2 {
				t.Fatal("zero counts must be visible")
			}
		} else {
			for _, number := range numbers {
				if !strings.Contains(page.Body.String(), number) {
					t.Fatalf("missing count %s", number)
				}
			}
		}
		head := reply("HEAD", "/index.html", "application/json", http.StatusOK)
		if head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(page.Body.Len()) {
			t.Fatal("incorrect HTML HEAD response")
		}
		jsonPage := reply("QUERY", "/status.json", "text/html", http.StatusOK)
		if !bytes.Equal(w.Body.Bytes(), jsonPage.Body.Bytes()) {
			t.Fatal("explicit JSON endpoint must always return JSON")
		}
	}
	reply("GET", "/status.json", "", http.StatusMethodNotAllowed)
	reply("HEAD", "/status.json", "", http.StatusMethodNotAllowed)
	reply("QUERY", "/status.json?", "", http.StatusBadRequest)
	reply("QUERY", "/status.json?format=json", "", http.StatusBadRequest)
	reply("POST", "/status.json", "", http.StatusMethodNotAllowed)
	reply("POST", "/index.html", "", http.StatusMethodNotAllowed)
	reply("GET", "/repository.key", "", http.StatusNotFound)
}

func TestStatusQueryProof(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(`{"packages":3,"ahead":2}`); err != nil {
		t.Fatal(err)
	}
	h := &statusHandler{fd: int(file.Fd())}
	check := func(r *http.Request, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got %d, want %d", r.Method, r.URL, w.Code, want)
		}
	}
	const path = "/status.json"
	check(httptest.NewRequest("QUERY", path, strings.NewReader("{}")), http.StatusUnauthorized)
	valid := signedStatusRequest(t, path, "{}")
	withBody := func(body string) *http.Request {
		r := httptest.NewRequest("QUERY", path, strings.NewReader(body))
		r.Header = valid.Header.Clone()
		return r
	}
	check(withBody("{ }"), http.StatusUnauthorized)
	check(signedStatusRequest(t, path, `{"unknown":1}`), http.StatusBadRequest)
	check(valid, http.StatusOK)
	check(withBody("{}"), http.StatusConflict)
}

func TestReaderRejectsLinksAndPipes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, path, pipe, dir} {
		if fd, err := openSnapshot(p); err == nil {
			unix.Close(fd)
			t.Fatal("accepted unsuitable status file", p)
		}
	}
}

func TestConfinement(t *testing.T) {
	if path := os.Getenv("POUDRINE_TEST_CAPABILITY_FILE"); path != "" {
		if err := discardInheritedFiles(); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(3, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatal("inherited directory descriptor was retained")
		}
		fd, err := openSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if err := unix.CapEnter(); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.Open(path, unix.O_RDONLY, 0); !errors.Is(err, unix.ECAPMODE) {
			t.Fatalf("look up path in capability mode: %v", err)
		}
		if _, err := unix.Pwrite(fd, []byte("x"), 0); !errors.Is(err, unix.ENOTCAPABLE) {
			t.Fatalf("write using snapshot descriptor: %v", err)
		}
		if err := unix.Fstat(fd, &unix.Stat_t{}); !errors.Is(err, unix.ENOTCAPABLE) {
			t.Fatalf("stat using snapshot descriptor: %v", err)
		}
		if got, ok := readCounts(fd); !ok || got != (counts{8, 2}) {
			t.Fatal("confined snapshot read", got)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte(`{"packages":8,"ahead":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfinement$")
	cmd.Env = append(os.Environ(), "POUDRINE_TEST_CAPABILITY_FILE="+path)
	cmd.ExtraFiles = []*os.File{d}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("confined reader: %v\n%s", err, output)
	}
}
