package status

import (
	"bytes"
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

	"github.com/inkeliz/poudrine/internal/statusproto"
	"golang.org/x/sys/unix"
)

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
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	for _, raw := range []string{"", "bad secret data", `null`, `{}`, `{"packages":null,"ahead":0}`, `{"packages":0}`, `{"packages":0,"ahead":1}`, `{"packages":-1,"ahead":0}`, `{"packages":2,"ahead":null}`, `{"packages":1,"ahead":0}[]`, strings.Repeat(" ", 1024) + `{"packages":1,"ahead":0}`} {
		set(raw)
		w := reply("GET", "/status.json", "", http.StatusServiceUnavailable)
		if w.Body.String() != "Statistics unavailable\n" {
			t.Fatal("invalid snapshot appeared in public response")
		}
	}
	set(`{"packages":0,"ahead":1,"extra":"sensitive snapshot content"}`)
	unavailable := reply("GET", "/", "", http.StatusServiceUnavailable).Body.String()
	if !strings.Contains(unavailable, "Statistics unavailable") || strings.Count(unavailable, ">—</dd>") != 2 || strings.Contains(unavailable, "sensitive snapshot content") {
		t.Fatal("HTML must show unavailable counts without exposing invalid snapshot contents")
	}
	// Unmarshal ignores unknown fields and uses the last duplicate value.
	for _, raw := range []string{`{"packages":2,"ahead":1,"ahead":0}`, `{"packages":2,"ahead":0,"private":"secret"}`} {
		set(raw)
		w := reply("GET", "/status.json", "", http.StatusOK)
		if w.Body.String() != "{\"packages\":2,\"ahead\":0}\n" {
			t.Fatalf("unexpected public counts: %s", w.Body.String())
		}
	}
	for _, want := range []statusproto.Counts{{}, {Packages: 38755, Ahead: 1234}} {
		set(fmt.Sprintf(`{"ahead":%20d,"packages":%20d}`, want.Ahead, want.Packages))
		w := reply("GET", "/status.json", "application/json", http.StatusOK)
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
		jsonPage := reply("GET", "/status.json", "text/html", http.StatusOK)
		if !bytes.Equal(w.Body.Bytes(), jsonPage.Body.Bytes()) {
			t.Fatal("explicit JSON endpoint must always return JSON")
		}
		jsonHead := reply("HEAD", "/status.json", "", http.StatusOK)
		if jsonHead.Body.Len() != 0 || jsonHead.Header().Get("Content-Length") != strconv.Itoa(jsonPage.Body.Len()) || jsonHead.Header().Get("Content-Type") != "application/json" {
			t.Fatal("incorrect JSON HEAD response")
		}
	}
	reply("GET", "/status.json?", "", http.StatusBadRequest)
	reply("GET", "/status.json?format=json", "", http.StatusBadRequest)
	for _, path := range []string{"/", "/index.html", "/status.json"} {
		w := reply("QUERY", path, "", http.StatusMethodNotAllowed)
		if w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("expected ordinary read methods only")
		}
	}
	reply("POST", "/status.json", "", http.StatusMethodNotAllowed)
	reply("POST", "/index.html", "", http.StatusMethodNotAllowed)
	reply("GET", "/repository.key", "", http.StatusNotFound)
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
		if got, ok := readCounts(fd); !ok || got != (statusproto.Counts{Packages: 8, Ahead: 2}) {
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
