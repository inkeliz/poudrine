package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/inkeliz/poudrine/internal/admin"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/public"
	"golang.org/x/net/netutil"
)

const httpWriteTimeout = 30 * time.Second

func listenHTTP(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	network := "tcp"
	if ip, err := netip.ParseAddr(host); err == nil {
		// Generic TCP can bind a dual-stack socket for the IPv6 wildcard.
		network = "tcp6"
		if ip.Unmap().Is4() {
			network = "tcp4"
		}
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	return netutil.LimitListener(listener, 128), nil
}

// Repository requests can fetch and verify an archive before sending the
// first byte. Renew the deadline on each socket write so a progressing HTTP
// download can outlive the server's write timeout.
type downloadListener struct{ net.Listener }

func (l downloadListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &downloadConn{Conn: conn, idle: httpWriteTimeout, rateWindow: 30 * time.Second}, nil
}

// Each response must deliver 16 KiB/s over a 30-second budget once sending
// starts. Idle time and cache-fill time between responses do not consume it.
const minimumDownloadRate = 16 << 10

type downloadConn struct {
	net.Conn
	idle       time.Duration
	mu         sync.Mutex
	rateWindow time.Duration
	rateEnd    time.Time
	sent       int64
}

func (c *downloadConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// net/http sets this at request start and clears it after the response.
	c.rateEnd, c.sent = time.Time{}, 0
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *downloadConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	deadline := now.Add(c.idle)
	if c.rateWindow > 0 {
		if c.rateEnd.IsZero() {
			c.rateEnd = now.Add(c.rateWindow)
		}
		if c.rateEnd.Before(deadline) {
			deadline = c.rateEnd
		}
	}
	if err := c.Conn.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	c.sent += int64(n)
	if c.rateWindow > 0 && c.sent >= max(1, int64(float64(minimumDownloadRate)*c.rateWindow.Seconds())) {
		c.rateEnd, c.sent = time.Now().Add(c.rateWindow), 0
	}
	return n, err
}

func signalReady(w io.WriteCloser) error {
	n, err := w.Write([]byte{1})
	if err == nil && n != 1 {
		err = io.ErrShortWrite
	}
	return errors.Join(err, w.Close())
}

func serveHTTP(ctx context.Context, o config.Options, repoListener, webListener net.Listener, repo *public.Repository, api *admin.Admin, publicStats *os.File) error {
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: httpWriteTimeout, IdleTimeout: 60 * time.Second,
		}
	}
	repoServer, webServer := newServer(repo), newServer(api.Handler())
	webScheme := "http"
	if o.WebTLS {
		tlsConfig, err := config.WebTLSConfig(o.State, o.WebListen, o.WebsiteURL, o.TLSCert, o.TLSKey)
		if err != nil {
			return err
		}
		webServer.TLSConfig = tlsConfig
		webScheme = "https"
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopRecording := repo.StartRecording(runCtx)
	// End live Management streams promptly when this service generation stops.
	webServer.BaseContext = func(net.Listener) context.Context { return runCtx }
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- repoServer.Serve(downloadListener{limitRepositoryPeers(repoListener)}) })
	wg.Go(func() {
		if o.WebTLS {
			results <- webServer.ServeTLS(webListener, "", "")
		} else {
			results <- webServer.Serve(webListener)
		}
	})
	wg.Go(func() { monitor(runCtx, o, repo.Catalog, repo.Cache) })
	if publicStats != nil {
		wg.Go(func() { monitorPublicStats(runCtx, repo.Catalog, repo.Store, publicStats) })
	}
	wg.Go(func() { catalog.MonitorLivePackages(runCtx, o, repo.Catalog) })
	wg.Go(func() { api.DispatchBuildPush(runCtx, repo.Catalog) })
	wg.Go(func() { api.CleanupNonces(runCtx) })
	wg.Go(func() { repo.CleanupClientActivity(runCtx) })
	defer func() {
		cancel()
		// Release both ports before waiting for active downloads to finish.
		repoListener.Close()
		webListener.Close()
		end, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, server := range []*http.Server{repoServer, webServer} {
			if err := server.Shutdown(end); err != nil {
				_ = server.Close()
			}
		}
		stopRecording()
		wg.Wait()
	}()

	log.Printf("repository http://%s, website %s://%s, state %s", repoListener.Addr(), webScheme, webListener.Addr(), o.State)
	if os.Getenv("POUDRINE_SUPERVISED") == "1" {
		f := os.NewFile(3, "supervisor-ready")
		if f == nil {
			return errors.New("missing supervisor readiness pipe")
		}
		if err := signalReady(f); err != nil {
			return fmt.Errorf("signal supervisor readiness: %w", err)
		}
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-results:
		return err
	}
}
