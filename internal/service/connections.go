package service

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

const repositoryConnectionsPerIP = 8
const repositoryConnectionLifetime = 30 * time.Minute

type peerListener struct {
	net.Listener
	mu       sync.Mutex
	active   map[netip.Addr]int
	limit    int
	lifetime time.Duration
}

func limitRepositoryPeers(listener net.Listener) *peerListener {
	return &peerListener{Listener: listener, active: make(map[netip.Addr]int), limit: repositoryConnectionsPerIP, lifetime: repositoryConnectionLifetime}
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		addr, err := netip.ParseAddrPort(conn.RemoteAddr().String())
		if err != nil {
			conn.Close()
			continue
		}
		ip := addr.Addr().Unmap().WithZone("")
		l.mu.Lock()
		if l.active[ip] >= l.limit {
			l.mu.Unlock()
			conn.Close()
			continue
		}
		l.active[ip]++
		c := &peerConn{Conn: conn, listener: l, ip: ip}
		c.timer = time.AfterFunc(l.lifetime, func() { c.Close() })
		l.mu.Unlock()
		return c, nil
	}
}

type peerConn struct {
	net.Conn
	listener *peerListener
	ip       netip.Addr
	once     sync.Once
	timer    *time.Timer // protected by listener.mu
	err      error
}

func (c *peerConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		l := c.listener
		l.mu.Lock()
		c.timer.Stop()
		l.active[c.ip]--
		if l.active[c.ip] == 0 {
			delete(l.active, c.ip)
		}
		l.mu.Unlock()
	})
	return c.err
}
