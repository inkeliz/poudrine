package service

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

type readinessPipe struct {
	io.Writer
	closed bool
}

func (p *readinessPipe) Close() error { p.closed = true; return nil }

type failedReadyWrite struct{ err error }

func (w failedReadyWrite) Write([]byte) (int, error) { return 0, w.err }

func TestSupervisorReadyWrite(t *testing.T) {
	var received bytes.Buffer
	for _, test := range []struct {
		writer io.Writer
		want   error
	}{
		{&received, nil},
		{failedReadyWrite{}, io.ErrShortWrite},
		{failedReadyWrite{io.ErrClosedPipe}, io.ErrClosedPipe},
	} {
		pipe := &readinessPipe{Writer: test.writer}
		if err := signalReady(pipe); !errors.Is(err, test.want) || !pipe.closed {
			t.Fatalf("readiness write: got %v (closed %v), want %v", err, pipe.closed, test.want)
		}
	}
	if !bytes.Equal(received.Bytes(), []byte{1}) {
		t.Fatalf("readiness signal %v", received.Bytes())
	}
}

func TestDownloadWriteDeadline(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	conn := downloadConn{raw, time.Second}
	received := make(chan error, 1)
	go func() {
		var chunk [1]byte
		_, err := io.ReadFull(peer, chunk[:])
		received <- err
	}()
	// A cache miss can take longer than the initial request deadline.
	if err := raw.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("a")); err != nil {
		t.Fatal("expired initial deadline interrupted a progressing download:", err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	conn.idle = 10 * time.Millisecond
	if _, err := conn.Write([]byte("b")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write to stalled peer: %v", err)
	}
}
