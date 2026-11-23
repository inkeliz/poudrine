package builder

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestRetainedBuildIssues(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	master := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports)
	start := time.Now().UTC().Add(-time.Hour)
	older, clean := start.Add(-time.Minute), start.Add(time.Minute)
	for _, stamp := range []time.Time{start, older, clean} {
		if err := os.MkdirAll(filepath.Join(master, stamp.Format(BuildNameFormat)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(stamp time.Time, name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(master, stamp.Format(BuildNameFormat), ".poudriere.ports."+name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	const abseil = "abseil-20250127.1_3"
	write(start, "failed", strings.Repeat("devel/abseil "+abseil+" fetch fetch_timeout 2\n", 2)+"misc/other other-1 configure  0\n")
	write(start, "skipped", "misc/codex codex-0.170.0 "+abseil+"\n"+"devel/protobuf protobuf-29.6_1,1 "+abseil+"\n"+
		"misc/codex codex-0.170.0 "+abseil+"\n"+"misc/codex codex-0.170.0 other-1\n"+"devel/partial partial-1 "+abseil)
	write(older, "failed", "misc/crashed crashed-1 pkgbuild crashed\n")
	latest := filepath.Join(master, "latest")
	if err := os.Symlink(clean.Format(BuildNameFormat), latest); err != nil {
		t.Fatal(err)
	}
	view, err := ReadBuildIssues(t.Context(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(view.Runs, []time.Time{start, older}) || !start.Equal(view.Started) || len(view.Failed) != 2 || len(view.Skipped) != 3 {
		t.Fatalf("latest issues: %+v", view)
	}
	if issue := view.Failed[0]; issue.Package != abseil || issue.Origin != "devel/abseil" || issue.Phase != "fetch" || issue.Reason != "fetch_timeout" {
		t.Fatalf("missing root failure: %+v", issue)
	}
	if view.Failed[1].Reason != "" || view.Skipped[0].BlockedBy != abseil || view.Skipped[1].Package != "protobuf-29.6_1,1" || view.Skipped[2].BlockedBy != "other-1" {
		t.Fatalf("incorrect failure details/dependency chain: %+v", view)
	}
	if view.Failed[0].BuildSeconds == nil || *view.Failed[0].BuildSeconds != 2 || view.Failed[1].BuildSeconds == nil || *view.Failed[1].BuildSeconds != 0 || view.Skipped[0].BuildSeconds != nil {
		t.Fatalf("incorrect durations for failed and skipped packages: %+v", view)
	}
	view, err = ReadBuildIssues(t.Context(), cfg, clean)
	if err != nil || view.Started != clean || len(view.Failed)+len(view.Skipped) != 0 {
		t.Fatalf("clean run: %+v, %v", view, err)
	}
	view, err = ReadBuildIssues(t.Context(), cfg, older)
	if err != nil || len(view.Failed) != 1 || view.Failed[0].Reason != "crashed" || view.Failed[0].BuildSeconds != nil {
		t.Fatalf("older failure: %+v, %v", view, err)
	}
	if _, err := ReadBuildLog(t.Context(), cfg, start, abseil, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing log for retained failure: %v", err)
	}
	if _, err := ReadBuildIssues(t.Context(), cfg, start.Add(time.Hour)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing run: %v", err)
	}
}

func TestRetainedBuildLogPages(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	start := time.Now().UTC()
	bulk := filepath.Join(cfg.BaseFS, "data", "logs", "bulk")
	master := cfg.Jail + "-" + cfg.Ports
	run := filepath.Join(bulk, master, start.Format(BuildNameFormat))
	const pkg = "protobuf-29.6_1,1"
	path := filepath.Join(run, "logs", pkg+".log")
	aliases := []string{filepath.Join(bulk, master, "latest-per-pkg", pkg+".log"), filepath.Join(bulk, "latest-per-pkg", "protobuf", "29.6_1,1", master+".log")}
	for _, p := range append([]string{path}, aliases...) {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	raw := "Start\n€" + strings.Repeat("x", buildLogPageBytes-1)
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	for _, p := range aliases {
		if err := os.Link(path, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(run, ".poudriere.ports.failed"), []byte("devel/protobuf "+pkg+" build ??? 7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	end, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if end.Start <= 0 || end.Size != int64(len(raw)) || end.End != end.Size || end.End-end.Start > buildLogPageBytes {
		t.Fatalf("invalid tail offsets %d:%d/%d", end.Start, end.End, end.Size)
	}
	first, err := ReadBuildLog(t.Context(), cfg, start, pkg, &end.Start)
	if err != nil || first.Start != 0 || first.End != end.Start || first.Text+end.Text != raw {
		t.Fatalf("paged log failed to preserve the original text: %v", err)
	}
	for _, before := range []int64{-1, end.Size + 1} {
		if _, err := ReadBuildLog(t.Context(), cfg, start, pkg, &before); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid offset %d: %v", before, err)
		}
	}
	if err := os.WriteFile(filepath.Join(run, "logs", "other-1.log"), []byte("unlisted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBuildLog(t.Context(), cfg, start, "other-1", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unlisted package log: %v", err)
	}
	if _, err := ReadBuildLog(t.Context(), cfg, start, "../protobuf-29", nil); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("non-package path: %v", err)
	}

	// Only the two known Poudriere hardlinks count. Replacing either alias
	// with an unrelated link makes the same link count insufficient.
	if err := os.Remove(aliases[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil); err != nil {
		t.Fatalf("a retained log can lose one latest-per-pkg alias: %v", err)
	}
	if err := os.Link(path, filepath.Join(t.TempDir(), "unrelated")); err != nil {
		t.Fatal(err)
	}
	if p, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil); err == nil || p.Text != "" {
		t.Fatal("unrelated hardlink should not be displayed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aliases[0], path); err != nil {
		t.Fatal(err)
	}
	if p, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil); err == nil || p.Text != "" {
		t.Fatal("native package log must not follow symlinks")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil); err == nil {
		t.Fatal("non-regular log accepted")
	}
}

func TestIssueReaderEmptyTree(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	view, err := ReadBuildIssues(t.Context(), cfg, time.Time{})
	if err != nil || !view.Started.IsZero() || view.Runs == nil || view.Failed == nil || view.Skipped == nil {
		t.Fatalf("empty tree: %+v, %v", view, err)
	}
	history, err := ReadBuildResults(t.Context(), cfg, time.Time{}, false)
	if err != nil || !history.Started.IsZero() || history.Runs == nil || history.Succeeded == nil || history.Failed == nil || history.Skipped == nil {
		t.Fatalf("empty build history: %+v, %v", history, err)
	}
}

func TestRetainedLogForRunningAndBuiltPackages(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	// BASEFS may be a root-managed symlink to the Poudriere volume.
	realBase := t.TempDir()
	if err := os.Symlink(realBase, cfg.BaseFS); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	master := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports)
	run := filepath.Join(master, start.Format(BuildNameFormat))
	if err := os.MkdirAll(filepath.Join(run, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(run, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	const pkg = "poudriere-3.4.8"
	write("logs/"+pkg+".log", "compiling\n")
	check := func(present bool) {
		t.Helper()
		p, err := ReadBuildLog(t.Context(), cfg, start, pkg, nil)
		if present && (err != nil || p.Text != "compiling\n") || !present && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("log availability %t: %q, %v", present, p.Text, err)
		}
	}
	write(".poudriere.status.02", "build:ports-mgmt/poudriere:"+pkg+":110:120\n")
	write(".poudriere.builders", "01\n")
	check(false)
	write(".poudriere.builders", "01 02\n")
	check(true)
	write(".poudriere.status.02", "idle:\n")
	check(false)
	write(".poudriere.ports.skipped", "ports-mgmt/poudriere "+pkg+" old-1\n")
	check(false)
	write(".poudriere.ports.built", "ports-mgmt/poudriere "+pkg+" 42\n")
	check(true)

	// An unsafe result file in a different run should not hide this one.
	broken := start.Add(time.Minute)
	brokenDir := filepath.Join(master, broken.Format(BuildNameFormat))
	if err := os.Mkdir(brokenDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(run, ".poudriere.ports.skipped"), filepath.Join(brokenDir, ".poudriere.ports.failed")); err != nil {
		t.Fatal(err)
	}
	view, err := ReadBuildIssues(t.Context(), cfg, start)
	if err != nil || len(view.Skipped) != 1 || view.Started != start {
		t.Fatalf("could not inspect the retained run: %+v %v", view, err)
	}
	if _, err := ReadBuildIssues(t.Context(), cfg, broken); err == nil {
		t.Fatal("unexpectedly used a result file symlink")
	}
	if _, err := ReadBuildResults(t.Context(), cfg, time.Time{}, false); err == nil {
		t.Fatal("history hid the unreadable latest run")
	}
	results, err := ReadBuildResults(t.Context(), cfg, start, false)
	if err != nil || !results.Started.Equal(start) || len(results.Succeeded) != 1 {
		t.Fatalf("older results unavailable after a newer run was unreadable: %+v, %v", results, err)
	}
}
