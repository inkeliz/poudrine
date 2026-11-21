package poudrine

import (
	"context"
	"encoding/base64"
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
func startSupervisedWeb(lifetime, startup context.Context, exe string, o options, cred *syscall.Credential, authority *configAuthority) (*supervisedWeb, error) {
	key, err := authority.publicKey()
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(o.configSettings)
	if err != nil {
		return nil, err
	}
	parent, child, err := configBrokerPair()
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
	ctx, cancel := context.WithCancel(lifetime)
	web := childProcess(ctx, exe, cred, "http-child", o.configPath)
	web.Env = append(web.Env, "POUDRINE_SUPERVISED=1", "POUDRINE_CONFIG_SNAPSHOT="+string(snapshot),
		"POUDRINE_ADMIN_KEY="+base64.RawURLEncoding.EncodeToString(key))
	web.Stdout = os.Stdout
	web.ExtraFiles = []*os.File{writer, child}
	stats, err := openManagedStats()
	if err != nil {
		log.Printf("public stats unavailable: %v", err)
	} else {
		web.ExtraFiles = append(web.ExtraFiles, stats)
		web.Env = append(web.Env, "POUDRINE_STATS_FD=5")
		defer stats.Close()
	}
	err = web.Start()
	writer.Close()
	child.Close()
	if err != nil {
		cancel()
		parent.Close()
		return nil, fmt.Errorf("launch unprivileged web service: %w", err)
	}
	brokerDone := make(chan struct{})
	go func() {
		defer close(brokerDone)
		authority.serve(parent)
	}()
	w := &supervisedWeb{cancel: cancel, done: make(chan struct{})}
	go func() {
		w.err = web.Wait()
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

func runServiceGenerations(parent context.Context, exe string, o options, cred *syscall.Credential, authority *configAuthority) error {
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
	b := authority.builder
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
			b.mu.Lock()
			upgrading := b.reexecPending && b.restartHold == nil
			b.mu.Unlock()
			if upgrading {
				workerErr = errWorkerReexec
				return
			}
			if err := ensureTools(ctx); err != nil {
				log.Printf("install worker prerequisites: %v; retrying in one minute", err)
				select {
				case <-ctx.Done():
				case <-b.wake:
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
		check := exec.CommandContext(c, exe, "service-check", "-config", o.configPath)
		if msg, err := check.CombinedOutput(); err != nil {
			return fmt.Errorf("validate updated Poudrine: %w %s", err, strings.TrimSpace(string(msg)))
		}
		release, err := b.holdForRestart(c)
		if err != nil {
			return err
		}
		defer release()
		if !b.Status().Active {
			b.reexecAfterBuild()
			return errWaitForService
		}
		if web != nil {
			web.stop()
		}
		web, err = startWeb(c)
		if err != nil {
			return err
		}
		b.reexecAfterBuild()
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
			if errors.Is(workerErr, errWorkerReexec) && ctx.Err() == nil {
				return &serviceUpgrade{exe: exe, worker: b}
			}
			return workerErr
		case req := <-requests:
			req.result <- restart(req.ctx)
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
