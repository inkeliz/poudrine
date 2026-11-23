package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/constanttime"
	"github.com/inkeliz/poudrine/internal/public"
)

type supervisedWeb struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // set before closing done
}

func (w *supervisedWeb) stop() {
	w.cancel()
	<-w.done
}

// The worker owns the build tree and its pipes. Only the HTTP child and its
// broker connection change while a build is held for a service restart.
func startSupervisedWeb(lifetime, startup context.Context, exe string, o config.Options, cred *syscall.Credential, authority *broker.Authority) (*supervisedWeb, error) {
	key, err := authority.PublicKey()
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(o.ConfigSettings)
	if err != nil {
		return nil, err
	}
	parent, child, err := broker.Pair()
	if err != nil {
		return nil, err
	}
	defer child.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		parent.Close()
		return nil, err
	}
	defer reader.Close()
	deathReader, deathWriter, err := os.Pipe()
	if err != nil {
		writer.Close()
		parent.Close()
		return nil, err
	}
	defer deathReader.Close()
	ctx, cancel := context.WithCancel(lifetime)
	web := config.ChildProcess(ctx, exe, cred, "http-child", o.ConfigPath)
	web.Env = append(web.Env, "POUDRINE_SUPERVISED=1", "POUDRINE_SUPERVISOR_FD=5", "POUDRINE_CONFIG_SNAPSHOT="+string(snapshot),
		"POUDRINE_ADMIN_KEY="+constanttime.RawURLEncoding.EncodeToString(key))
	web.Stdout = os.Stdout
	web.ExtraFiles = []*os.File{writer, child, deathReader}
	stats, err := public.OpenManagedStats()
	if err != nil {
		log.Printf("public stats unavailable: %v", err)
	} else {
		web.ExtraFiles = append(web.ExtraFiles, stats)
		web.Env = append(web.Env, "POUDRINE_STATS_FD=6")
		defer stats.Close()
	}
	err = web.Start()
	writer.Close()
	child.Close()
	if err != nil {
		cancel()
		deathWriter.Close()
		parent.Close()
		return nil, fmt.Errorf("launch unprivileged web service: %w", err)
	}
	brokerDone := make(chan struct{})
	go func() {
		defer close(brokerDone)
		authority.Serve(ctx, parent)
	}()
	w := &supervisedWeb{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer deathWriter.Close()
		w.err = web.Wait()
		cancel()
		<-brokerDone
		close(w.done)
	}()
	readyCtx, end := context.WithTimeout(startup, time.Minute)
	defer end()
	stopWait := context.AfterFunc(readyCtx, func() { reader.Close() })
	defer stopWait()
	var ready [1]byte
	if _, err := io.ReadFull(reader, ready[:]); err != nil || ready[0] != 1 {
		w.stop()
		if readyCtx.Err() != nil {
			return nil, readyCtx.Err()
		}
		if w.err != nil {
			return nil, fmt.Errorf("web startup: %w", w.err)
		}
		return nil, errors.New("web service stopped before becoming ready")
	}
	return w, nil
}

type serviceRestartRequest struct {
	ctx    context.Context
	result chan error
}

func runServiceGenerations(parent context.Context, exe string, o config.Options, cred *syscall.Credential, authority *broker.Authority, repoPolicy <-chan config.PackageOverlays) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	startWeb := func(startup context.Context) (*supervisedWeb, error) {
		return startSupervisedWeb(ctx, startup, exe, o, cred, authority)
	}
	web, err := startWeb(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if web != nil {
			web.stop()
		}
	}()
	b := authority.Builder
	requests := make(chan serviceRestartRequest)
	if path := os.Getenv("POUDRINE_RESTART_SOCKET"); path != "" {
		listener, err := listenServiceRestarts(ctx, path, func(c context.Context) error {
			req := serviceRestartRequest{ctx: c, result: make(chan error, 1)}
			select {
			case requests <- req:
			case <-c.Done():
				return c.Err()
			}
			select {
			case err := <-req.result:
				return err
			case <-c.Done():
				select {
				case err := <-req.result:
					return err
				default:
					return c.Err()
				}
			}
		})
		if err != nil {
			return err
		}
		defer listener.Close()
	}

	stopped := make(chan struct{})
	var workerErr error
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			upgrading := b.UpgradeReady()
			if upgrading {
				workerErr = builder.ErrWorkerReexec
				return
			}
			if err := ensureTools(ctx); err != nil {
				log.Printf("install worker prerequisites: %v; retrying in one minute", err)
				select {
				case <-ctx.Done():
				case <-b.Wakeups():
				case <-time.After(time.Minute):
				}
				continue
			}
			workerErr = b.Run(ctx)
			return
		}
	}()
	defer func() { cancel(); <-stopped }()

	restart := func(c context.Context) error {
		// Verify the installed executable and settings before disturbing the
		// current HTTP service. The existing worker still owns its generation.
		check := exec.CommandContext(c, exe, "service-check", "-config", o.ConfigPath)
		if msg, err := check.CombinedOutput(); err != nil {
			return fmt.Errorf("validate updated Poudrine: %w %s", err, strings.TrimSpace(string(msg)))
		}
		release, err := b.HoldForRestart(c)
		if err != nil {
			return err
		}
		defer release()
		if !b.Status().Active {
			b.ReexecAfterBuild()
			return errWaitForService
		}
		if web != nil {
			web.stop()
		}
		web, err = startWeb(c)
		if err != nil {
			return err
		}
		b.ReexecAfterBuild()
		if !b.Status().Active {
			return errWaitForService
		}
		return nil
	}
	for {
		var exited <-chan struct{}
		var retry <-chan time.Time
		if web != nil {
			exited = web.done
		} else {
			retry = time.After(5 * time.Second)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopped:
			if errors.Is(workerErr, builder.ErrWorkerReexec) && ctx.Err() == nil {
				return &serviceUpgrade{exe: exe, worker: b}
			}
			return workerErr
		case req := <-requests:
			req.result <- restart(req.ctx)
		case overlays := <-repoPolicy:
			o.ConfigSettings.PackageOverlays = overlays
			o.Builder.PackageOverlays = overlays
			if web != nil {
				web.stop()
			}
			web, err = startWeb(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("apply package overlays to repository: %v; retrying in five seconds", err)
			}
		case <-exited:
			log.Printf("web service exited: %v; restarting it in five seconds", web.err)
			web.stop()
			web = nil
		case <-retry:
			var err error
			web, err = startWeb(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("restart web service: %v", err)
			}
		}
	}
}

// The supervisor alone holds the write end, with close-on-exec set. EOF also
// detects SIGKILL or a crash, without relying on platform-specific death signals
// or racing a parent PID check. Cancellation closes the reader and joins it.
func watchSupervisor(parent context.Context, fd int) (context.Context, context.CancelFunc, error) {
	syscall.CloseOnExec(fd)
	// ExtraFiles descriptors arrive in blocking mode. Register this pipe with
	// Go's poller so closing it also interrupts a read during normal shutdown.
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, nil, fmt.Errorf("open supervisor lifetime pipe: %w", err)
	}
	pipe := os.NewFile(uintptr(fd), "supervisor-lifetime")
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		_, _ = io.Copy(io.Discard, pipe)
	}()
	closeOnCancel := context.AfterFunc(ctx, func() { pipe.Close() })
	return ctx, func() {
		cancel()
		pipe.Close()
		closeOnCancel()
		<-done
	}, nil
}
