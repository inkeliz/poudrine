package service

import (
	"io"
	"net"
	"testing"
	"time"
)

type queuedListener struct{ conns chan net.Conn }

func (l *queuedListener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *queuedListener) Close() error   { close(l.conns); return nil }
func (l *queuedListener) Addr() net.Addr { return &net.TCPAddr{} }

type addressedConn struct {
	net.Conn
	remote net.Addr
}

func (c addressedConn) RemoteAddr() net.Addr { return c.remote }

func TestRepositoryPeerLimitsAndLifetime(t *testing.T) {
	queue := &queuedListener{make(chan net.Conn, 4)}
	limited := limitRepositoryPeers(queue)
	limited.limit = 1
	limited.lifetime = time.Second
	add := func(ip string) net.Conn {
		server, client := net.Pipe()
		queue.conns <- addressedConn{server, &net.TCPAddr{IP: net.ParseIP(ip), Port: 123}}
		t.Cleanup(func() { server.Close(); client.Close() })
		return client
	}
	add("192.0.2.1")
	first, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	rejected := add("::ffff:192.0.2.1")
	add("192.0.2.2")
	second, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	rejected.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := rejected.Read(b[:]); err != io.EOF {
		t.Fatalf("excess connection not closed: %v", err)
	}
	first.Close()
	first.Close()
	limited.lifetime = 10 * time.Millisecond
	peer := add("192.0.2.1")
	next, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatalf("connection lifetime not bounded: %v", err)
	}
}
