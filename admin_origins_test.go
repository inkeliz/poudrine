package poudrine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfiguredBuildOrigins(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdmin(t)
			key := enrollAdmin(t, a)
			cfg := workerTestConfig(t.TempDir())
			if mode == "supervised" {
				b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
				if err != nil {
					t.Fatal(err)
				}
				var supervisor *configAuthority
				a, supervisor = supervisedTestAdmin(t, a.cfg, key, b)
				unsigned := forwardedConfigRequest(httptest.NewRequest(methodQuery, originsPath, nil), nil)
				if reply := supervisor.handle(unsigned); reply.Status != 401 {
					t.Fatalf("unsigned supervisor origins request: %+v", reply)
				}
			} else {
				a.cfg.ActiveSettings.PackagesFile = cfg.PackagesFile
			}
			adminResponse(t, a, httptest.NewRequest(methodQuery, originsPath, nil), 401)
			check := func(want []string) {
				t.Helper()
				req := signedTestRequest(t, key, methodQuery, originsPath, []byte("{}"), time.Now())
				raw := adminResponse(t, a, req, 200).Body.Bytes()
				var result struct {
					Origins json.RawMessage `json:"origins"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				var got []string
				if err := json.Unmarshal(result.Origins, &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("received %d origins, wanted %d (null=%t, want %t): %v", len(got), len(want), got == nil, want == nil, err)
				}
			}
			write := func(body string) {
				t.Helper()
				if err := os.WriteFile(cfg.PackagesFile, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}

			check(nil) // The worker has not created the list yet.
			write("# comment only\n")
			check([]string{})
			write(" devel/py-example@py311 # manual entry\nftp/curl\nftp/curl\n")
			check([]string{"devel/py-example@py311", "ftp/curl"})
			write("bad origin\n")
			check(nil)
			for _, count := range []int{originsPerFrame, 3 * originsPerFrame} {
				ports := make([]string, count)
				for i := range ports {
					ports[i] = fmt.Sprintf("devel/%0240d", i)
				}
				write(strings.Join(ports, "\n"))
				check(ports)
			}
			// The last snapshot spans more than one full broker frame. A
			// subsequent authenticated request must read its own response.
			req := signedTestRequest(t, key, methodQuery, buildStatusPath, []byte("{}"), time.Now())
			body := adminResponse(t, a, req, 200).Body.Bytes()
			var view buildView
			if err := json.Unmarshal(body, &view); err != nil || view.Stage != "waiting" || view.CanForce != (mode == "supervised") {
				t.Fatalf("unexpected status after reading large inventory: %s", body)
			}
		})
	}
}

func TestConfiguredOriginsRequiresRegularFile(t *testing.T) {
	if origins := configuredOrigins(t.TempDir()); origins != nil {
		t.Fatalf("directory reported as an inventory: %v", origins)
	}
	dir := t.TempDir()
	list := filepath.Join(dir, "packages.txt")
	if err := os.WriteFile(list, []byte("ftp/curl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "packages-link")
	if err := os.Symlink(list, link); err != nil {
		t.Fatal(err)
	}
	if origins := configuredOrigins(link); !reflect.DeepEqual(origins, []string{"ftp/curl"}) {
		t.Fatalf("configured symlink: %v", origins)
	}
}
