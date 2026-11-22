package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/inkeliz/poudrine/internal/fsutil"
	"golang.org/x/sys/unix"
)

// FreeBSD procctl(2), <sys/procctl.h>. idtype P_PID and id 0 select only self.
// id_t is 64 bits, including on the 32-bit syscall ABI.
func ownProcctl(op uintptr, data unsafe.Pointer) error {
	var errno syscall.Errno
	switch runtime.GOARCH {
	case "arm": // EABI aligns id_t to an even pair of argument slots.
		_, _, errno = syscall.Syscall6(syscall.SYS_PROCCTL, 0, 0, 0, 0, op, uintptr(data))
	case "386":
		_, _, errno = syscall.Syscall6(syscall.SYS_PROCCTL, 0, 0, 0, op, uintptr(data), 0)
	default:
		_, _, errno = syscall.Syscall6(syscall.SYS_PROCCTL, 0, 0, op, uintptr(data), 0, 0)
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// Called exclusively by a helper which has successfully acquired its own
// reaper and has only Poudriere as its child. REAP_KILL walks the entire
// kernel subtree, including nested reapers, orphans and new jail sessions.
func signalWorkTree(sig syscall.Signal) error {
	k := struct {
		Signal    int32
		Flags     uint32
		Subtree   int32
		Killed    uint32
		FailedPID int32
		Padding   [15]uint32
	}{Signal: int32(sig)}
	err := ownProcctl(6, unsafe.Pointer(&k)) // PROC_REAP_KILL
	if errors.Is(err, syscall.ESRCH) && k.FailedPID == -1 {
		return nil // The command just exited.
	}
	if err != nil {
		return err
	}
	if k.FailedPID != -1 {
		return fmt.Errorf("signal %s failed for descendant %d", sig, k.FailedPID)
	}
	return nil
}

// The parent controls this dedicated reaper via an inherited socket. A lost
// parent closes the socket, which thaws a paused tree before normal teardown.
func RunPoudriereCommand(ctx context.Context, args []string) error {
	control := os.NewFile(3, "build control")
	if control == nil {
		return errors.New("missing build control socket")
	}
	defer control.Close()
	unix.CloseOnExec(3)
	if _, err := unix.Getpeername(3); err != nil {
		return fmt.Errorf("build control socket: %w", err)
	}
	if err := ownProcctl(2, nil); err != nil { // PROC_REAP_ACQUIRE
		return fmt.Errorf("acquire Poudriere reaper: %w", err)
	}
	// Startup of a new process may straddle a closing boundary. Wait for
	// the worker to recheck its live schedule before launching Poudriere.
	if _, err := control.Write([]byte{'A'}); err != nil {
		return err
	}
	var start [1]byte
	if _, err := io.ReadFull(control, start[:]); err != nil {
		return err
	}
	if start[0] != 'R' {
		return errors.New("invalid build start request")
	}
	// Poudriere normally tears down its jobs before returning. On shutdown,
	// prevent a stray helper from outliving the job and holding its pipes open.
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	mightBePaused := false
	defer func() {
		if life.Err() != nil {
			if err := signalWorkTree(syscall.SIGKILL); err != nil {
				fmt.Fprintln(os.Stderr, "releasing Poudriere process tree:", err)
			}
		} else if mightBePaused {
			if err := signalWorkTree(syscall.SIGCONT); err != nil {
				fmt.Fprintln(os.Stderr, "releasing paused Poudriere helpers:", err)
			}
		}
	}()
	cmd := exec.CommandContext(life, "poudriere", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error {
		if err := signalWorkTree(syscall.SIGCONT); err != nil {
			fmt.Fprintln(os.Stderr, "resume Poudriere for shutdown:", err)
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Minute
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	requests := make(chan byte)
	stopReader := make(chan struct{})
	defer close(stopReader)
	go func() {
		defer close(requests)
		var request [1]byte
		for {
			if _, err := io.ReadFull(control, request[:]); err != nil {
				return
			}
			select {
			case requests <- request[0]:
			case <-stopReader:
				return
			}
		}
	}()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			return <-done
		case request, open := <-requests:
			if !open {
				cancel()
				return <-done
			}
			sig := syscall.SIGSTOP
			if request == 'C' {
				sig = syscall.SIGCONT
			} else if request != 'S' {
				cancel()
				<-done
				return errors.New("invalid build control request")
			}
			ack := byte('Y')
			if sig == syscall.SIGSTOP {
				mightBePaused = true
			}
			if err := signalWorkTree(sig); err != nil {
				fmt.Fprintln(os.Stderr, "Poudriere process tree:", err)
				ack = 'E'
			} else if sig == syscall.SIGCONT {
				mightBePaused = false
			}
			if _, err := control.Write([]byte{ack}); err != nil {
				cancel()
				return <-done
			}
		}
	}
}

func (b *Builder) runWorkCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return err
	}
	control := os.NewFile(uintptr(pair[0]), "worker control")
	defer control.Close()
	stopClose := context.AfterFunc(ctx, func() { control.Close() })
	defer stopClose()
	child := os.NewFile(uintptr(pair[1]), "build control")
	cmd := exec.Command(b.Executable, append([]string{"poudriere-command", "-N", "-e", b.Cfg.ConfigDir}, args...)...)
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Minute
	err = cmd.Start()
	child.Close()
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var ready [1]byte
	_, err = io.ReadFull(control, ready[:])
	if err == nil && ready[0] != 'A' {
		err = errors.New("invalid build helper response")
	}
	if err == nil {
		err = b.startWork(ctx)
		if err == nil {
			defer b.endWork()
		}
	}
	if err == nil {
		_, err = control.Write([]byte{'R'})
	}
	if err != nil {
		control.Close()
		waitErr := <-done
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(err, waitErr)
	}
	var journal []byte
	var bulkName string
	if len(args) >= 3 && args[0] == "bulk" && args[len(args)-2] == "-B" {
		bulkName = args[len(args)-1]
	}
	path := filepath.Join(b.Cfg.ConfigDir, ".poudrine-pauses")
	defer func() {
		b.windowStatus(false, time.Time{}, nil)
		if len(journal) > 0 {
			os.Remove(path)
		}
	}()
	// A partially signalled tree must also receive CONT when its window
	// opens. Keep the starting time until both the journal and CONT succeed.
	var since time.Time
	paused := false
	for {
		b.mu.Lock()
		inWindow, next := b.hours.State(time.Now())
		hold := b.restartHold
		b.mu.Unlock()
		allowed := inWindow && hold == nil
		var changeErr error
		if !allowed && !paused || allowed && !since.IsZero() {
			req := byte('S')
			updated := journal
			if allowed {
				req = 'C'
				if bulkName != "" {
					updated = fmt.Appendf(journal, "%s %d %d\n", bulkName, since.Unix(), time.Now().Unix())
					changeErr = fsutil.WriteAtomicFile(path, updated, 0600)
				}
			} else if since.IsZero() {
				since = time.Now()
			}
			if changeErr == nil {
				var ack [1]byte
				_, err := control.Write([]byte{req})
				if err == nil {
					_, err = io.ReadFull(control, ack[:])
				}
				if err != nil {
					control.Close()
					waitErr := <-done
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return waitErr
				}
				if ack[0] != 'Y' {
					changeErr = errors.New("changing Poudriere process state failed; retrying")
				} else {
					paused = !allowed
					if allowed {
						since = time.Time{}
						journal = updated
					}
				}
			}
		}
		opens := time.Time{}
		if !inWindow {
			opens = next
		}
		b.windowStatus(paused, opens, changeErr)
		if paused || changeErr != nil {
			hold.acknowledge(changeErr)
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if changeErr != nil {
			timer = time.NewTimer(time.Second)
		} else if !next.IsZero() {
			timer = time.NewTimer(boundaryDelay(next))
		}
		if timer != nil {
			tick = timer.C
		}
		select {
		case err := <-done:
			if timer != nil {
				timer.Stop()
			}
			return err
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			control.Close()
			<-done
			return ctx.Err()
		case <-b.hoursChanged:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
