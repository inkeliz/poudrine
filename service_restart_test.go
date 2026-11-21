package poudrine

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestServiceRestartHandshake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc.sock")
	var calls atomic.Int32
	entered := make(chan struct{})
	proceed := make(chan struct{})
	callback := func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		}
		return errors.New("cannot pause the current command")
	}
	service, err := listenServiceRestarts(t.Context(), path, callback)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		t.Fatalf("private service socket: %v, %v", info, err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "unknown\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if err != nil || !strings.HasPrefix(line, "ERROR") || calls.Load() != 0 {
		t.Fatalf("unexpected command ran a restart: %q, %v", line, err)
	}
	results := make(chan error, 2)
	go func() { results <- requestServiceRestart(path) }()
	<-entered
	second, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := io.WriteString(second, "restart\n"); err != nil {
		t.Fatal(err)
	}
	// The second client must be waiting for completion of the first.
	second.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	peek := make([]byte, 1)
	if _, err := second.Read(peek); err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	close(proceed)
	line, err = bufio.NewReader(second).ReadString('\n')
	if err != nil || line != "OK\n" || <-results != nil || calls.Load() != 1 {
		t.Fatalf("duplicate restart: calls %d, reply %q, error %v", calls.Load(), line, err)
	}
	if err := requestServiceRestart(path); err == nil || err.Error() != "cannot pause the current command" {
		t.Fatalf("missing supervisor error: %v", err)
	}
}

func TestServiceRestartSocketOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc.sock")
	noop := func(context.Context) error { return nil }
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	wantRefusal := func() {
		t.Helper()
		service, err := listenServiceRestarts(t.Context(), path, noop)
		if service != nil || err == nil {
			t.Fatal("listener replaced an occupied path")
		}
	}
	wantRefusal()
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("changed existing file: %s %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	other.SetUnlinkOnClose(false)
	defer other.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	wantRefusal()
	other.Close() // the on-disk socket now belongs to a dead listener
	s, err := listenServiceRestarts(t.Context(), path, noop)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wantRefusal()
	if err := requestServiceRestart(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service socket survived clean exit: %v", err)
	}
	if err := requestServiceRestart(path); err == nil || !strings.Contains(err.Error(), "let the active build finish") {
		t.Fatalf("missing migration guidance: %v", err)
	}
}

func TestServiceRestartAcknowledgesBeforeReexec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc.sock")
	ready := make(chan struct{})
	s, err := listenServiceRestarts(t.Context(), path, func(ctx context.Context) error {
		close(ready)
		// The generation received a positive response and is now leaving.
		<-ctx.Done()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result := make(chan error, 1)
	go func() { result <- requestServiceRestart(path) }()
	<-ready
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestIdleServiceRestartWaitsForNewGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc.sock")
	var calls atomic.Int32
	called := make(chan struct{})
	old, err := listenServiceRestarts(t.Context(), path, func(context.Context) error {
		if calls.Add(1) == 1 {
			close(called)
		}
		return errWaitForService
	})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	result := make(chan error, 1)
	go func() { result <- requestServiceRestart(path) }()
	<-called
	waiting := func() {
		t.Helper()
		select {
		case err := <-result:
			t.Fatalf("restart returned before replacement was ready: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
	}
	waiting() // The outgoing listener can acknowledge only its own generation.
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waiting() // Replacement hasn't published its control socket.
	fresh, err := listenServiceRestarts(t.Context(), path, func(context.Context) error {
		calls.Add(1)
		return errors.New("unexpected second restart")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	select {
	case err := <-result:
		if err != nil || calls.Load() != 1 {
			t.Fatalf("restart count %d, error %v", calls.Load(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ready service was not acknowledged")
	}
}

func TestLongServiceRestartSocket(t *testing.T) {
	root := t.TempDir()
	const name = "a.restart"
	n := len(syscall.RawSockaddrUnix{}.Path) - 1 - len(root) - len(name) - 2
	if n < 1 {
		t.Skip("test directory already fills the Unix socket address")
	}
	dir := filepath.Join(root, strings.Repeat("d", n))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	service, err := listenServiceRestarts(t.Context(), path, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := requestServiceRestart(path); err != nil {
		t.Fatal(err)
	}
}
