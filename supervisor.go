package poudrine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// Run starts Poudrine or executes a subcommand using the arguments after the executable name.
func Run(ctx context.Context, args []string) error {
	mode := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		mode, args = args[0], args[1:]
	}
	if mode == "migrate-config" && len(args) >= 1 {
		return migrateConfiguration(args[0], args[1:])
	}
	if (mode == "inventory-origins" || mode == "inventory-key") && len(args) == 1 {
		return inventoryQuery(ctx, mode, args[0])
	}
	if mode == "inventory-build" && len(args) == 1 {
		return importBuildRecord(ctx, args[0], os.Stdin)
	}
	if mode == "http-child" && len(args) == 1 && os.Getenv("POUDRINE_SUPERVISED") == "1" {
		o, err := childSettings(args[0])
		if err != nil {
			return err
		}
		return serve(ctx, o)
	}
	switch mode {
	case "service-check":
		o, err := parseOptions("run", args)
		if err != nil {
			return err
		}
		return preflightConfiguration(ctx, "run", o)
	case "service-restart":
		if len(args) != 1 {
			return errors.New("expected a local service restart socket")
		}
		return requestServiceRestart(args[0])
	case "poudriere-command":
		return runPoudriereCommand(ctx, args)
	case "client-setup":
		o, err := parseOptions(mode, args)
		if err != nil {
			return err
		}
		return printClientSetup(o, os.Stdout)
	case "check-config":
		o, err := parseOptions(mode, args)
		if err != nil {
			return err
		}
		if err := validateWebTLS(ctx, o, false); err != nil {
			return err
		}
		fmt.Printf("Valid configuration: %s\n", o.configPath)
		return nil
	case "check-tls-pair":
		// The supervisor forwards this diagnostic to the settings form.
		log.SetFlags(0)
		if len(args) != 2 {
			return errors.New("expected a certificate and key path")
		}
		_, err := loadWebsiteKeyPair(args[0], args[1])
		return err
	case "set-tls":
		return setTLS(args)
	case "run", "worker", "serve":
		o, err := parseOptions(mode, args)
		if err != nil {
			return err
		}
		return runConfigured(ctx, mode, o)
	default:
		return fmt.Errorf("unknown subcommand %q (see -help)", mode)
	}
}

func ensureTools(ctx context.Context) error {
	var pkgs []string
	for _, dep := range []struct{ command, pkg string }{{"poudriere", "poudriere"}, {"git", "git-lite"}} {
		if _, err := exec.LookPath(dep.command); err != nil {
			pkgs = append(pkgs, dep.pkg)
		}
	}
	if len(pkgs) == 0 {
		return nil
	}
	log.Printf("installing missing worker dependencies %v", pkgs)
	cmd := exec.CommandContext(ctx, "/usr/sbin/pkg", append([]string{"install", "-y"}, pkgs...)...)
	cmd.Env = append(os.Environ(), "ASSUME_ALWAYS_YES=yes")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func webIdentity(ctx context.Context, name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) && name == "_poudrine" {
		cmd := exec.CommandContext(ctx, "/usr/sbin/pw", "useradd", name, "-g", "nogroup", "-d", "/nonexistent", "-s", "/usr/sbin/nologin", "-w", "no", "-c", "Poudrine repository service")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("create service user: %w", err)
		}
		u, err = user.Lookup(name)
	}
	if err != nil {
		return nil, err
	}
	return serviceCredential(u)
}

func serviceCredential(u *user.User) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return nil, errors.New("service user must have a nonzero uid")
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil || gid == 0 {
		return nil, errors.New("service user must have a nonzero primary gid")
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

func prepareState(dir string, c *syscall.Credential) error {
	// New parent directories stay root-owned. Only a new state directory is
	// assigned to the HTTP account.
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return err
	}
	err := os.Mkdir(dir, 0700)
	if err == nil {
		return os.Chown(dir, int(c.Uid), int(c.Gid))
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	s, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !s.IsDir() || s.Sys().(*syscall.Stat_t).Uid != c.Uid || s.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s must be a private directory owned by the service user", dir)
	}
	return nil
}

func childProcess(ctx context.Context, exe string, cred *syscall.Credential, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Env = []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	cmd.Dir = "/"
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

func supervise(ctx context.Context, mode string, o options, registered chan<- *Builder, restore *workerCheckpoint) error {
	if runtime.GOOS != "freebsd" || os.Geteuid() != 0 {
		return errors.New("Poudriere setup requires root on FreeBSD. Run doas poudrine, or use poudrine serve for the HTTP services alone")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// rc services may inherit a PATH containing only base-system tools.
	if err := os.Setenv("PATH", os.Getenv("PATH")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"); err != nil {
		return err
	}
	if err := o.checkWorkerStorage(mode); err != nil {
		return err
	}
	cred, err := webIdentity(ctx, o.serviceUser)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	if path := os.Getenv("POUDRINE_SERVICE_BIN"); mode == "run" && os.Getenv("POUDRINE_RESTART_SOCKET") != "" && path != "" {
		if _, exists, err := inspectRootPath(path, false, true); err != nil || !exists {
			return fmt.Errorf("invalid installed service binary %s: %w", path, errors.Join(err, os.ErrInvalid))
		}
		// Keep the configured name across atomic install or symlink updates.
		exe = path
	}
	origins := func(ctx context.Context) ([]string, error) {
		// Read the web-owned database as the web user, including its SQLite WAL.
		cmd := childProcess(ctx, exe, cred, "inventory-origins", o.state)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("read tracked ports: %w", err)
		}
		var list []string
		err = json.Unmarshal(out, &list)
		return list, err
	}
	b, err := NewBuilder(o.builder, origins, nil)
	if err != nil {
		return err
	}
	b.executable = exe
	if restore != nil {
		b.restoreCheckpoint(restore)
	}
	registered <- b
	// Collect package outcomes from Poudriere's root-managed logs. Only the
	// unprivileged inventory owner opens SQLite and its WAL.
	history := packageLogHistory{cfg: o.builder, save: func(ctx context.Context, records []BuildRecord) error {
		data, err := json.Marshal(records)
		if err != nil {
			return err
		}
		recordCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := childProcess(recordCtx, exe, cred, "inventory-build", o.state)
		cmd.Stdin = bytes.NewReader(data)
		return cmd.Run()
	}}
	historyWake := make(chan struct{}, 1)
	b.monitorHistory = func(ctx context.Context) { history.monitor(ctx, b.Status, historyWake) }
	b.onBuildFinished = func(_ context.Context, _ BuildStatus) {
		select {
		case historyWake <- struct{}{}:
		default:
		}
	}
	if mode == "worker" {
		if err := ensureTools(ctx); err != nil {
			return fmt.Errorf("install worker prerequisites: %w", err)
		}
		return b.Run(ctx)
	}
	if err := prepareState(o.state, cred); err != nil {
		return err
	}
	var migrated ed25519.PublicKey
	if _, err := os.Stat(o.configPath + ".admin.pub"); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(o.configPath + ".setup-token"); errors.Is(err, os.ErrNotExist) {
			out, err := childProcess(ctx, exe, cred, "inventory-key", o.state).Output()
			if err != nil {
				return fmt.Errorf("read existing enrollment: %w", err)
			}
			if err = json.Unmarshal(out, &migrated); err != nil {
				return err
			}
		}
	}
	authority, err := newConfigAuthority(o.configPath, migrated)
	if err != nil {
		return err
	}
	authority.builder = b
	return runServiceGenerations(ctx, exe, o, cred, authority)
}
