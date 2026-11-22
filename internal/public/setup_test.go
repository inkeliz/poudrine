package public

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestCopiedReportCommand(t *testing.T) {
	for _, binary := range []string{"curl", "openssl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip("requires " + binary)
		}
	}
	const prefix = "/network/o'neil"
	repo, _ := inventoryReportFixture(t)
	received := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix+routes.ReportInventory.Path || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		received <- string(body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.URL.Path, r.URL.RawPath = routes.ReportInventory.Path, ""
		repo.ServeHTTP(w, r)
	}))
	defer server.Close()
	data, err := MakeClientInstructions(server.URL+prefix, testutil.RepositoryPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	bin, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pkg"), []byte("#!/bin/sh\nprintf '%s' \"$TEST_ROWS\"\nexit \"$TEST_PKG_STATUS\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var public []byte
	for _, tc := range []struct{ name, rows, status string }{
		{"installed", "pkg 1.2_3,1 ports-mgmt/pkg\ncurl 8.0 ftp/curl\n", "0"},
		{"empty", "", "0"},
		{"failed-query", "partial", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROWS="+tc.rows, "TEST_PKG_STATUS="+tc.status)
			cmd.Stdin = strings.NewReader(strings.ReplaceAll(data.ReportCommand, "/var/db/poudrine-client", state))
			out, err := cmd.CombinedOutput()
			if tc.status != "0" {
				if err == nil || len(received) != 0 {
					t.Fatal("failed pkg query submitted a report")
				}
				return
			}
			if err != nil {
				t.Fatalf("copied signed report: %v: %s", err, out)
			}
			if got := <-received; got != tc.rows {
				t.Fatalf("report body %q, want %q", got, tc.rows)
			}
			var pin []byte
			if err := repo.Store.DB.QueryRow("SELECT public_key FROM client_report_keys").Scan(&pin); err != nil {
				t.Fatal(err)
			}
			if public != nil && !bytes.Equal(pin, public) {
				t.Fatal("second report did not reuse the client key")
			}
			public = pin
			if st, err := os.Stat(filepath.Join(state, "inventory.key")); err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("expected private reporting key with mode 0600", err)
			}
		})
	}
}
