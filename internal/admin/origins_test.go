package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestConfiguredBuildOrigins(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdmin(t)
			key := enrollAdmin(t, a)
			cfg := testutil.WorkerConfig(t.TempDir())
			if mode == "supervised" {
				b, err := builder.NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
				if err != nil {
					t.Fatal(err)
				}
				var supervisor *broker.Authority
				a, supervisor = supervisedTestAdmin(t, a.cfg, key, b)
				unsigned := broker.ForwardedConfigRequest(httptest.NewRequest(routes.Origins.Method, routes.Origins.Path, nil), nil)
				if reply := supervisor.Handle(t.Context(), unsigned); reply.Status != 401 {
					t.Fatalf("unsigned supervisor origins request: %+v", reply)
				}
			} else {
				a.cfg.ActiveSettings.PackagesFile = cfg.PackagesFile
			}
			adminResponse(t, a, httptest.NewRequest(routes.Origins.Method, routes.Origins.Path, nil), 401)
			check := func(want []string) {
				t.Helper()
				req := testutil.SignedRequest(t, key, routes.Origins.Method, routes.Origins.Path, []byte("{}"), time.Now())
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
			for _, count := range []int{broker.OriginsPerFrame, 3 * broker.OriginsPerFrame} {
				ports := make([]string, count)
				for i := range ports {
					ports[i] = fmt.Sprintf("devel/%0240d", i)
				}
				write(strings.Join(ports, "\n"))
				check(ports)
			}
			// The last snapshot spans more than one full broker frame. A
			// subsequent authenticated request must read its own response.
			req := testutil.SignedRequest(t, key, routes.BuildStatus.Method, routes.BuildStatus.Path, []byte("{}"), time.Now())
			body := adminResponse(t, a, req, 200).Body.Bytes()
			var view buildView
			if err := json.Unmarshal(body, &view); err != nil || view.Stage != "waiting" || view.CanForce != (mode == "supervised") {
				t.Fatalf("unexpected status after reading large inventory: %s", body)
			}
		})
	}
}

func TestOverlayVisibleBeforeFirstBuild(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdmin(t)
			key := enrollAdmin(t, a)
			const origin = "databases/mydumper"
			cfg := testutil.WorkerConfig(t.TempDir())
			cfg.PackageOverlays = config.PackageOverlays{origin: {}}
			if mode == "standalone" {
				a.cfg.ActiveSettings.PackagesFile = cfg.PackagesFile
				a.cfg.ActiveSettings.PackageOverlays = cfg.PackageOverlays
			} else {
				b, err := builder.NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
				if err != nil {
					t.Fatal(err)
				}
				a, _ = supervisedTestAdmin(t, a.cfg, key, b)
			}
			a.cfg.Packages = func(_ context.Context, origins []string) (catalog.PackageInventoryView, error) {
				return catalog.ManagementPackages(nil, nil, origins, nil, nil, nil), nil
			}
			get := func(path string, dst any) {
				t.Helper()
				req := testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now())
				reply := adminResponse(t, a, req, 200)
				if err := json.Unmarshal(reply.Body.Bytes(), dst); err != nil {
					t.Fatal(err)
				}
			}
			var roots struct{ Origins []string }
			get(routes.Origins.Path, &roots)
			if !reflect.DeepEqual(roots.Origins, []string{origin}) {
				t.Fatalf("missing overlay origin: %v", roots.Origins)
			}
			var view catalog.PackageInventoryView
			get(routes.Packages.Path, &view)
			if view.TrackedOrigins != 1 || len(view.Packages) != 1 || !view.Packages[0].Tracked || view.Packages[0].Origin != origin {
				t.Fatalf("missing overlay in inventory: %+v", view)
			}
		})
	}
}
