package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/fsutil"
	"github.com/inkeliz/poudrine/internal/testutil"
)

// Opt-in system test on a dedicated FreeBSD host. Use an absolute path to the
// built command in POUDRINE_RC_TEST_BIN and run this test as root. All service
// paths are isolated, including the rc supervisor PID and private listeners.
func TestServiceRestartPreservesBuild(t *testing.T) {
	bin := os.Getenv("POUDRINE_RC_TEST_BIN")
	if bin == "" || os.Geteuid() != 0 {
		t.Skip("requires the built POUDRINE_RC_TEST_BIN and root on FreeBSD")
	}
	// The optional public status publisher uses a system-wide location.
	// Reserve its name exclusively so the fixture can never change an
	// existing snapshot, and remove our reservation on exit.
	guard, err := os.OpenFile(config.PublicStatsDir, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		t.Skip("the host already has a Poudrine status publisher")
	}
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
	t.Cleanup(func() { os.Remove(config.PublicStatsDir) })

	root, err := os.MkdirTemp("/tmp", "poudrine-rc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	path := func(name string) string { return filepath.Join(root, name) }
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		if err := fsutil.WriteAtomicFile(path(name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		data, _ := os.ReadFile(path(name))
		return strings.TrimSpace(string(data))
	}
	install := func(from, to string) {
		t.Helper()
		if out, err := exec.Command("/usr/bin/install", "-S", "-m", "0555", from, path(to)).CombinedOutput(); err != nil {
			t.Fatalf("install: %v: %s", err, out)
		}
	}
	install(bin, "poudrine")
	install(bin, "updated-poudrine")
	if err := os.Mkdir(path("rc.d"), 0755); err != nil {
		t.Fatal(err)
	}
	install("../../rc.d/poudrine", "rc.d/poudrine")
	write("poudriere", `#!/bin/sh
case " $* " in
*" bulk "*) ;;
*) exit 0 ;;
esac
cd "$POUDRINE_RC_FIXTURE" || exit 1
echo "$$" >> bulks
exec /usr/local/libexec/poudriere/setsid -w /bin/sh ./compiler
`, 0555)
	write("compiler", `#!/bin/sh
echo "$$" >> compilers
while [ ! -f finish ]; do
	echo work >> progress
	sleep .05
done
`, 0555)
	write("origins", "www/firefox\n", 0644)
	worker := testutil.WorkerConfig(root)
	cfg := config.DefaultConfiguration()
	webPort, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RepoListen, cfg.WebListen = "127.0.0.1:0", webPort.Addr().String()
	webPort.Close()
	cfg.WebTLS, cfg.Upstream = false, "http://127.0.0.1:9"
	cfg.State, cfg.ServiceUser = path("state"), "nobody"
	cfg.PoudriereConfig, cfg.BaseFS, cfg.Distfiles = worker.ConfigDir, worker.BaseFS, worker.Distfiles
	cfg.PackagesFile, cfg.BuildKey, cfg.BuildStatus = worker.PackagesFile, worker.SigningKey, worker.StatusFile
	cfg.Release, cfg.BuildInterval = worker.Release, "24h"
	configure := func(hours string) {
		t.Helper()
		cfg.WorkingHours = hours
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		write("config.json", string(data), 0600)
	}
	configure("")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	rc := func(ctx context.Context, action string) error {
		args := []string{"-E", "_rc_conf_loaded=true", "-E", "local_startup=" + path("rc.d"),
			"-E", "poudrine_enable=YES", "-E", "poudrine_bin=" + path("poudrine"), "-E", "poudrine_config=" + path("config.json"),
			"-E", "poudrine_pidfile=" + path("service.pid"), "-E", "POUDRINE_RC_FIXTURE=" + root,
			"-E", "PATH=" + root + ":/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin", "poudrine", action}
		cmd := exec.CommandContext(ctx, "/usr/sbin/service", args...)
		cmd.Env = append(os.Environ(), "_rc_conf_loaded=true", "local_startup="+path("rc.d"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", action, err, out)
		}
		return nil
	}
	await := func(label string, fn func() bool) {
		t.Helper()
		waitCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		if err := testutil.Wait(waitCtx, fn); err != nil {
			t.Fatalf("waiting for %s: %v; build status: %s", label, err, read("status.json"))
		}
	}
	if err := rc(ctx, "start"); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			os.WriteFile(path("release-web"), nil, 0644)
			os.WriteFile(path("release-exec"), nil, 0644)
			os.WriteFile(path("finish"), nil, 0644)
			end, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			if err := rc(end, "stop"); err != nil {
				t.Error(err)
			}
		}
	})
	await("running compiler", func() bool { return len(read("progress")) > 10 })
	bulkPID, compilerPID := read("bulks"), read("compilers")
	status := func() BuildStatus { s, _ := ReadBuildStatus(worker.StatusFile); return s }
	first := status()
	process := func(parent, arg string) string {
		out, _ := exec.Command("/bin/pgrep", "-P", parent, "-f", arg).Output()
		return strings.TrimSpace(string(out))
	}
	vnode := func(pid string) string {
		out, _ := exec.Command("/usr/bin/fstat", "-p", pid).Output()
		for line := range strings.SplitSeq(string(out), "\n") {
			if f := strings.Fields(line); len(f) > 5 && f[3] == "text" {
				return f[5]
			}
		}
		return ""
	}
	image, err := os.Stat(path("updated-poudrine"))
	if err != nil {
		t.Fatal(err)
	}
	inode := strconv.FormatUint(image.Sys().(*syscall.Stat_t).Ino, 10)
	web := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	available := func() bool {
		resp, err := web.Get("http://" + cfg.WebListen + "/admin")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	supervisor := process(read("service.pid"), " run -config ")
	webPID := process(supervisor, " http-child ")
	if supervisor == "" || webPID == "" || !first.Active {
		t.Fatalf("missing service processes: supervisor=%q http=%q build=%+v", supervisor, webPID, first)
	}
	// Delay only the new HTTP child's readiness to observe the compiler while
	// the service is restarting. Activation ends in the real updated binary.
	write("launcher", fmt.Sprintf(`#!/bin/sh
if [ "$1" = http-child ]; then
	echo "$$" > '%s'
	while [ ! -e '%s' ]; do sleep .05; done
	if [ -e '%s' ]; then
		: > '%s'
		while [ ! -e '%s' ]; do sleep .05; done
	fi
elif [ "$1" = run ] && [ -e '%s' ]; then
	: > '%s'
fi
exec '%s' "$@"
`, path("state/new-http"), path("release-web"), path("exec-held"), path("state/final-web-held"), path("release-exec"),
		path("gate-exec"), path("exec-held"), path("updated-poudrine")), 0555)
	install(path("launcher"), "poudrine")
	restartDone := make(chan error, 1)
	go func() { restartDone <- rc(ctx, "restart") }()
	await("replacement HTTP child", func() bool { return read("state/new-http") != "" })
	await("paused compiler", func() bool { return status().Paused })
	progress := read("progress")
	await("compiler stopped by the kernel", func() bool { return stoppedCompiler(ctx, path("compilers")) })
	if read("progress") != progress || read("compilers") != compilerPID {
		t.Fatal("compiler advanced or restarted while the updated HTTP child was starting")
	}
	write("release-web", "", 0644)
	if err := <-restartDone; err != nil {
		t.Fatal(err)
	}
	await("same build continuing", func() bool { return !status().Paused && read("progress") != progress })
	newWeb := process(supervisor, " http-child ")
	if !status().Started.Equal(first.Started) || read("compilers") != compilerPID || read("bulks") != bulkPID ||
		newWeb == webPID || vnode(newWeb) != inode || !available() {
		t.Fatal("restart lost the active build or kept the old web service")
	}
	// Exercise another restart while the existing process tree is already
	// paused by working hours. Replacing the web child cannot unpause it.
	hour := (time.Now().Hour() + 4) % 24
	configure(fmt.Sprintf("%02d:00-%02d:01", hour, hour))
	await("working-hours pause", func() bool { s := status(); return s.Paused && !s.WindowOpens.IsZero() })
	progress = read("progress")
	if err := rc(ctx, "restart"); err != nil {
		t.Fatal(err)
	}
	if read("progress") != progress || !status().Paused {
		t.Fatal("restart resumed the build outside working hours")
	}
	configure("")
	await("working hours reopened", func() bool { return read("progress") != progress })
	write("finish", "", 0644)
	await("updated supervisor and Management", func() bool { return vnode(supervisor) == inode && available() })
	if read("compilers") != compilerPID || read("bulks") != bulkPID {
		t.Fatal("more than one bulk or compiler ran")
	}
	write("gate-exec", "", 0644)
	go func() { restartDone <- rc(ctx, "restart") }()
	await("final Management child after idle re-exec", func() bool { _, err := os.Stat(path("state/final-web-held")); return err == nil })
	select {
	case err := <-restartDone:
		t.Fatalf("idle restart returned while the replacement supervisor was still starting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	write("release-exec", "", 0644)
	if err := <-restartDone; err != nil || !available() {
		t.Fatalf("idle restart did not return with Management ready: %v", err)
	}
	// A normal stop must retain its shutdown semantics even after an upgrade.
	if err := os.Remove(path("finish")); err != nil {
		t.Fatal(err)
	}
	write("origins", "www/firefox\nmisc/test\n", 0644)
	hour = time.Now().Hour()
	configure(fmt.Sprintf("%02d:00-%02d:00", (hour+23)%24, (hour+3)%24))
	await("next build", func() bool { return len(strings.Fields(read("compilers"))) == 2 })
	next, err := strconv.Atoi(strings.Fields(read("compilers"))[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := rc(ctx, "stop"); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if _, err := os.Stat(path("service.pid")); !os.IsNotExist(err) {
		t.Fatalf("daemon pidfile after stop: %v", err)
	}
	await("compiler to exit after ordinary stop", func() bool { return syscall.Kill(next, 0) == syscall.ESRCH })
}
