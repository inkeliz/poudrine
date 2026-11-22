package service

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
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/public"
)

// Run starts Poudrine or executes a subcommand using the arguments after the executable name.
func Run(ctx context.Context, args []string) error {
	mode := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		mode, args = args[0], args[1:]
	}
	if mode == "migrate-config" && len(args) >= 1 {
		return config.MigrateConfiguration(args[0], args[1:])
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
		o, err := config.ParseOptions("run", args)
		if err != nil {
			return err
		}
		return config.PreflightConfiguration(ctx, "run", o)
	case "service-restart":
		if len(args) != 1 {
			return errors.New("expected a local service restart socket")
		}
		return requestServiceRestart(args[0])
	case "poudriere-command":
		return builder.RunPoudriereCommand(ctx, args)
	case "client-setup":
		o, err := config.ParseOptions(mode, args)
		if err != nil {
			return err
		}
		return public.PrintClientSetup(o, os.Stdout)
	case "check-config":
		o, err := config.ParseOptions(mode, args)
		if err != nil {
			return err
		}
		if err := config.ValidateWebTLS(ctx, o, false); err != nil {
			return err
		}
		fmt.Printf("Valid configuration: %s\n", o.ConfigPath)
		return nil
	case "check-tls-pair":
		// The supervisor forwards this diagnostic to the settings form.
		log.SetFlags(0)
		if len(args) != 2 {
			return errors.New("expected a certificate and key path")
		}
		_, err := config.LoadWebsiteKeyPair(args[0], args[1])
		return err
	case "set-tls":
		return config.SetTLS(args)
	case "run", "worker", "serve":
		o, err := config.ParseOptions(mode, args)
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
	return config.ServiceCredential(u)
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

func supervise(ctx context.Context, mode string, o config.Options, registered chan<- *builder.Builder, restore *builder.WorkerCheckpoint, repoPolicy <-chan config.PackageOverlays) error {
	if os.Geteuid() != 0 {
		return errors.New("Poudriere setup requires root on FreeBSD. Run doas poudrine, or use poudrine serve for the HTTP services alone")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// rc services may inherit a PATH containing only base-system tools.
	if err := os.Setenv("PATH", os.Getenv("PATH")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"); err != nil {
		return err
	}
	if err := o.CheckWorkerStorage(mode); err != nil {
		return err
	}
	cred, err := webIdentity(ctx, o.ServiceUser)
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
		if _, exists, err := config.InspectRootPath(path, false, true); err != nil || !exists {
			return fmt.Errorf("invalid installed service binary %s: %w", path, errors.Join(err, os.ErrInvalid))
		}
		// Keep the configured name across atomic install or symlink updates.
		exe = path
	}
	origins := func(ctx context.Context) ([]string, error) {
		// Read the web-owned database as the web user, including its SQLite WAL.
		cmd := config.ChildProcess(ctx, exe, cred, "inventory-origins", o.State)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("read tracked ports: %w", err)
		}
		var list []string
		err = json.Unmarshal(out, &list)
		return list, err
	}
	b, err := builder.NewBuilder(o.Builder, origins, nil)
	if err != nil {
		return err
	}
	b.Executable = exe
	if restore != nil {
		b.RestoreCheckpoint(restore)
	}
	registered <- b
	// Collect package outcomes from Poudriere's root-managed logs. Only the
	// unprivileged inventory owner opens SQLite and its WAL.
	history := builder.PackageLogHistory{Cfg: o.Builder, Save: func(ctx context.Context, records []database.BuildRecord) error {
		data, err := json.Marshal(records)
		if err != nil {
			return err
		}
		recordCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := config.ChildProcess(recordCtx, exe, cred, "inventory-build", o.State)
		cmd.Stdin = bytes.NewReader(data)
		return cmd.Run()
	}}
	historyWake := make(chan struct{}, 1)
	b.MonitorHistory = func(ctx context.Context) { history.Monitor(ctx, b.Status, historyWake) }
	b.OnBuildFinished = func(_ context.Context, _ builder.BuildStatus) {
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
	if err := prepareState(o.State, cred); err != nil {
		return err
	}
	var migrated ed25519.PublicKey
	if _, err := os.Stat(o.ConfigPath + config.AdminKeySuffix); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(o.ConfigPath + config.SetupTokenSuffix); errors.Is(err, os.ErrNotExist) {
			out, err := config.ChildProcess(ctx, exe, cred, "inventory-key", o.State).Output()
			if err != nil {
				return fmt.Errorf("read existing enrollment: %w", err)
			}
			if err = json.Unmarshal(out, &migrated); err != nil {
				return err
			}
		}
	}
	authority, err := broker.NewAuthority(o.ConfigPath, migrated)
	if err != nil {
		return err
	}
	authority.Builder = b
	return runServiceGenerations(ctx, exe, o, cred, authority, repoPolicy)
}
