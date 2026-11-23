package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"repo_listen":"127.0.0.1:8000","web_listen":"8182","repo_url":"https://repo.example/packages","state":"data","refresh":"25m","cache_gb":0,"jobs":2}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := ParseOptions("serve", []string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if o.WebListen != "[::]:8182" || o.State != filepath.Join(dir, "data") || o.PublicURL != "https://repo.example/packages" || o.Refresh != 25*time.Minute || o.CacheGB != 0 || o.Builder.Jobs != 2 || o.Builder.Interval != 15*time.Minute {
		t.Fatalf("bad settings: %+v", o)
	}
	if o.ConfigRevision != ConfigRevision([]byte(content)) || o.ConfigSettings.State != "data" {
		t.Fatalf("invalid startup snapshot: %+v", o)
	}
}

func TestConfigurationPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	o, err := ParseOptions("serve", []string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if o.RepoListen != "[::]:8080" || o.WebListen != "[::]:8181" {
		t.Fatalf("incorrect defaults: %+v", o)
	}
	if _, err := SaveConfiguration(path, o.ConfigRevision, json.RawMessage(`{"jobs":0}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected invalid jobs, got %v", err)
	}
	orig := o.ConfigRevision
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, setting := range []string{`{"jobs":2}`, `{"jobs":3}`} {
		wg.Go(func() {
			_, err := SaveConfiguration(path, orig, json.RawMessage(setting))
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrConfigConflict) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("concurrent saves: %d successes", wins.Load())
	}
	_, revision, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	manual := []byte("{\n \"jobs\": 5\n}\n")
	if err := os.WriteFile(path, manual, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveConfiguration(path, revision, json.RawMessage("{}")); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale edit after manual change: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(manual) {
		t.Fatal("overwrote manual edit", err)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, s := range []string{`null`, `{}` + "{}", `{"jobs":1.5}`, `{"typo":1}`, `{"trusted_proxy":":::"}`, `{"repo_url":"http://[::]:8080"}`, `{"service_user":"root"}`, `{"build_interval":"0s"}`, `{"refresh":"-1s"}`, `{"repo_listen":"::1:80"}`, `{"repo_listen":":99999"}`, `{"packages_file":"/tmp/../etc/ports"}`, `{"jail":"-bad"}`} {
		if _, err := DecodeConfiguration([]byte(s), ""); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("accepted invalid config %q: %v", s, err)
		}
	}
	for _, value := range []string{"https://[::]:8181", "http://0.0.0.0", "http://2001:db8::1:8080", "https://example.org:99999", "https://user@example.org", "https://example.org/path?"} {
		if _, err := publicHTTPURL(value); err == nil {
			t.Errorf("accepted invalid public URL %q", value)
		}
	}
}

func TestMigrateLegacySettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root config paths must reside in root-controlled locations")
	}
	t.Setenv("POUDRINE_REPO_URL", "https://old.example")
	t.Setenv("POUDRINE_WEB_URL", "https://site.example/ui/")
	path := filepath.Join(t.TempDir(), "poudrine.json")
	args := []string{"-web-tls=false", "-state", "srv/previous", "-cache-gb", "0.5", "-jobs=3", "-repo-url", "https://custom.example", "-build-interval", "45m"}
	if err := MigrateConfiguration(path, args); err != nil {
		t.Fatal(err)
	}
	c, rev, err := LoadConfiguration(path)
	if err != nil || c.WebTLS || c.State != "/srv/previous" || c.CacheGB != 0.5 || c.Jobs != 3 || c.RepoURL != "https://custom.example" || c.WebURL != "https://site.example/ui/" || c.BuildInterval != "45m" {
		t.Fatalf("bad migration: %+v %v", c, err)
	}
	if err := MigrateConfiguration(path, []string{"-state", "/different"}); err != nil {
		t.Fatal(err)
	}
	if _, newRev, err := LoadConfiguration(path); err != nil || newRev != rev {
		t.Fatalf("existing file changed: %v", err)
	}
}
