package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func inheritedPipe(t *testing.T, reader *os.File) int {
	t.Helper()
	// Fd switches the pipe to blocking mode, as exec's ExtraFiles does.
	fd, err := syscall.Dup(int(reader.Fd()))
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestSupervisorCrashReleasesListener(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	// Only this process keeps the pipe's write end after Start. SIGKILL closes
	// it without a cancellation signal or any supervisor cleanup running.
	owner := exec.Command("/bin/sh", "-c", "printf ready; exec sleep 30")
	owner.ExtraFiles = []*os.File{writer}
	ready, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { owner.Process.Kill(); owner.Wait() }()
	writer.Close()
	var signal [5]byte
	if _, err := io.ReadFull(ready, signal[:]); err != nil {
		t.Fatal(err)
	}
	ctx, stop, err := watchSupervisor(t.Context(), inheritedPipe(t, reader))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()
	server := &http.Server{}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	closeServer := context.AfterFunc(ctx, func() { server.Close() })
	defer closeServer()
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP listener survived supervisor death")
	}
	rebound, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("restart cannot bind %s: %v", address, err)
	}
	rebound.Close()
}

func TestSupervisorWatcherStopsWithWriterAlive(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx, stop, err := watchSupervisor(parent, inheritedPipe(t, reader))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cancel(); stop(); close(done) }()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("child context still active")
		}
	case <-time.After(time.Second):
		writer.Close()
		<-done
		t.Fatal("normal shutdown waited for supervisor to die")
	}
}
