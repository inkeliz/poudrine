package poudrine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServiceCheckpointTransfer(t *testing.T) {
	dir := t.TempDir()
	o := options{configPath: filepath.Join(dir, "poudrine.json"), builder: workerTestConfig(dir)}
	t.Setenv("POUDRINE_RESTART_SOCKET", filepath.Join(dir, "rc.sock"))
	cp := &workerCheckpoint{Status: BuildStatus{Stage: "idle", ManualPending: true}, RetryOrigins: []string{"www/firefox"},
		LastOrigins: "www/firefox\n", Maintenance: time.Now().UTC().Add(time.Hour)}
	for _, changed := range []bool{false, true} {
		f, err := checkpointFile(o, cp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
			t.Fatal("restart state was left in the filesystem")
		}
		// A duplicate stands in for the descriptor inherited across exec.
		fd, err := unix.Dup(int(f.Fd()))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(checkpointFDEnv, strconv.Itoa(fd))
		newOptions := o
		newOptions.websiteURL = "https://updated.example"
		if changed {
			newOptions.builder.Jail = "another"
		}
		got, err := readServiceCheckpoint(newOptions)
		if err != nil || !changed && !reflect.DeepEqual(got, cp) || changed && got != nil {
			t.Fatalf("different jail=%v: checkpoint %+v, err=%v", changed, got, err)
		}
		if _, ok := os.LookupEnv(checkpointFDEnv); ok {
			t.Fatal("checkpoint is still available to child processes")
		}
	}
}

func TestConfigurationEditWaitsForActiveServiceBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poudrine.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"web_tls":false}`)
	cfg, rev, err := loadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := cfg.options(path)
	if err != nil {
		t.Fatal(err)
	}
	opts.configRevision = rev
	worker := &Builder{status: BuildStatus{Active: true}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	registered, ticks := make(chan *Builder), make(chan time.Time)
	repoPolicy := make(chan PackageOverlays)
	done := make(chan configTransition, 1)
	go func() { done <- watchConfiguration(ctx, "run", opts, registered, ticks, repoPolicy) }()
	registered <- worker
	write(`{"web_tls":false,"jobs":2,"package_overlays":{"databases/mydumper":{}}}`)
	ticks <- time.Now()
	select {
	case overlays := <-repoPolicy:
		if _, ok := overlays["databases/mydumper"]; !ok {
			t.Fatal("repository still allows the stock package")
		}
	case <-done:
		t.Fatal("overlay interrupted an active build")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	write(`{"web_tls":false,"jobs":2}`)
	ticks <- time.Now()
	select {
	case overlays := <-repoPolicy:
		if len(overlays) != 0 {
			t.Fatal("removing an overlay left the repository restricted")
		}
	case <-done:
		t.Fatal("removing the overlay interrupted an active build")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case ticks <- time.Now(): // First check left the active generation intact.
	case change := <-done:
		t.Fatalf("edit interrupted an active build: %+v", change)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	worker.mu.Lock()
	worker.status.Active = false
	worker.mu.Unlock()
	select {
	case ticks <- time.Now():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case change := <-done:
		if change.next != nil && change.next.builder.Jobs == 2 {
			return
		}
		t.Fatalf("unexpected config change: %+v", change)
	}
	change := <-done
	if change.next == nil || change.next.builder.Jobs != 2 {
		t.Fatal("idle supervisor did not apply the pending configuration")
	}
}
