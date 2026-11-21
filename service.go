package poudrine

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type options struct {
	webTLS                                                                                                                  bool
	tlsCert, tlsKey                                                                                                         string
	configRevision                                                                                                          string
	configSettings                                                                                                          Configuration
	configPath                                                                                                              string
	builder                                                                                                                 BuilderConfig
	state, repoListen, webListen, publicURL, websiteURL, upstream, fingerprints, localRepo, proxy, pushContact, serviceUser string
	cacheGB                                                                                                                 float64
	refresh                                                                                                                 time.Duration
}

func webURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("invalid repository base URL %q", raw)
	}
	return u, nil
}

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
	return net.Listen(network, address)
}

func hostUserlandRelease(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "freebsd-version", "-u").Output()
	return strings.TrimSpace(string(out)), err
}

func hostPackageABI(ctx context.Context) (string, error) {
	// The base-system pkg shim may prompt to bootstrap on a new host.
	if out, err := exec.CommandContext(ctx, "/usr/local/sbin/pkg", "config", "ABI").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	release, err := hostUserlandRelease(ctx)
	if err != nil {
		return "", err
	}
	major, _, ok := strings.Cut(release, ".")
	if !ok {
		return "", fmt.Errorf("unknown FreeBSD release %q", release)
	}
	arch, err := exec.CommandContext(ctx, "uname", "-p").Output()
	if err != nil {
		return "", err
	}
	return "FreeBSD:" + major + ":" + strings.TrimSpace(string(arch)), nil
}

func defaultUpstream(ctx context.Context) (string, error) {
	abi, err := hostPackageABI(ctx)
	if err != nil {
		return "", err
	}
	return "https://pkg.freebsd.org/" + abi + "/latest", nil
}

func serve(ctx context.Context, o options) error {
	var broker *configClient
	var pinned ed25519.PublicKey
	var publicStats *os.File
	if os.Getenv("POUDRINE_SUPERVISED") == "1" {
		syscall.CloseOnExec(3)
		syscall.CloseOnExec(4)
		f := os.NewFile(4, "config-broker")
		defer f.Close()
		broker = newConfigClient(f)
		if os.Getenv("POUDRINE_STATS_FD") == "5" {
			syscall.CloseOnExec(5)
			publicStats = os.NewFile(5, publicStatsFile)
			defer publicStats.Close()
		}
		var err error
		pinned, err = base64.RawURLEncoding.DecodeString(os.Getenv("POUDRINE_ADMIN_KEY"))
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(o.state, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(o.state, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock web service state: %w", err)
	}
	if broker == nil {
		publicStats, err = openStatsWriter(filepath.Join(o.state, standaloneStatsName))
		if err != nil {
			log.Printf("public stats unavailable: %v", err)
		} else {
			defer publicStats.Close()
		}
	}
	if o.upstream == "" {
		o.upstream, err = defaultUpstream(ctx)
		if err != nil {
			return fmt.Errorf("detect upstream ABI (or configure upstream): %w", err)
		}
	}
	up, err := webURL(o.upstream)
	if err != nil {
		return err
	}
	repoListener, err := listenHTTP(o.repoListen)
	if err != nil {
		return err
	}
	defer repoListener.Close()
	webListener, err := listenHTTP(o.webListen)
	if err != nil {
		return err
	}
	defer webListener.Close()
	o.publicURL, err = o.advertisedRepository(repoListener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	if _, err := webURL(o.publicURL); err != nil {
		return err
	}
	var proxy *net.IPNet
	if o.proxy != "" {
		_, proxy, err = net.ParseCIDR(o.proxy)
		if err != nil {
			return err
		}
	}
	db, err := openInventory(o.state, false)
	if err != nil {
		return err
	}
	defer db.Close()
	store := inventory{db}
	client := &http.Client{Timeout: 30 * time.Minute}
	cat, err := NewCatalog(o.state, up.String(), o.fingerprints, o.localRepo, client)
	if err != nil {
		return err
	}
	cache, err := newPackageCache(filepath.Join(o.state, "cache"), int64(o.cacheGB*1e9), up, client)
	if err != nil {
		return err
	}
	packages := func(ctx context.Context, origins []string) (packageInventoryView, error) {
		usage, err := store.trackedPackages(ctx)
		if err != nil {
			return packageInventoryView{}, err
		}
		builds, err := store.recentInventoryBuilds(ctx)
		if err != nil {
			return packageInventoryView{}, err
		}
		snapshot := cat.current.Load()
		view := managementPackages(snapshot, usage, origins, cache.inventory(snapshot), resolvedPorts(o.builder), builds)
		view.CacheLimit = cache.limit
		return view, store.loadBuildTimes(ctx, snapshot, view.Packages)
	}
	api, err := NewAdmin(AdminConfig{ConfigPath: o.configPath, ActiveSettings: o.configSettings, builder: o.builder, broker: broker, pinnedKey: pinned, DB: db, DataDir: o.state, RepositoryURL: o.publicURL, WebsiteURL: o.websiteURL,
		RepositoryPort: repoListener.Addr().(*net.TCPAddr).Port, UpstreamURL: o.upstream,
		PushContact: o.pushContact, PublicKey: func() string { return string(cat.PublicKey) }, Stats: cat.Stats, Clients: store.clients, Tracked: store.tracked, Packages: packages, Build: func() (BuildStatus, error) {
			s, err := ReadBuildStatus(o.builder.StatusFile)
			if errors.Is(err, os.ErrNotExist) {
				return BuildStatus{Stage: "waiting", Message: "Waiting for the build worker"}, nil
			}
			return s, err
		}})
	if err != nil {
		return err
	}
	repo := &repository{catalog: cat, store: store, cache: cache, localDir: o.localRepo, proxy: proxy}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	}
	repoServer, webServer := newServer(repo), newServer(api.Handler())
	// End live Management streams promptly when this service generation stops.
	webServer.BaseContext = func(net.Listener) context.Context { return runCtx }
	webScheme := "http"
	if o.webTLS {
		webServer.TLSConfig, err = webTLSConfig(o.state, o.webListen, o.websiteURL, o.tlsCert, o.tlsKey)
		if err != nil {
			return err
		}
		webScheme = "https"
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- repoServer.Serve(repoListener) })
	wg.Go(func() {
		if o.webTLS {
			results <- webServer.ServeTLS(webListener, "", "")
		} else {
			results <- webServer.Serve(webListener)
		}
	})
	wg.Go(func() { monitor(runCtx, o, cat, cache, publicStats) })
	wg.Go(func() { api.dispatchBuildPush(runCtx, cat) })
	log.Printf("repository http://%s, website %s://%s, state %s", repoListener.Addr(), webScheme, webListener.Addr(), o.state)
	if os.Getenv("POUDRINE_SUPERVISED") == "1" {
		f := os.NewFile(3, "supervisor-ready")
		if f != nil {
			_, _ = f.Write([]byte{1})
			f.Close()
		}
	}
	select {
	case <-ctx.Done():
	case err = <-results:
	}
	cancel()
	end, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for _, s := range []*http.Server{repoServer, webServer} {
		if e := s.Shutdown(end); e != nil {
			_ = s.Close()
		}
	}
	wg.Wait()
	return err
}

func monitor(ctx context.Context, o options, cat *Catalog, cache *packageCache, publicStats *os.File) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var next, last time.Time
	if s, err := ReadBuildStatus(o.builder.StatusFile); err == nil {
		last = s.Finished
	}
	for ctx.Err() == nil {
		if s, err := ReadBuildStatus(o.builder.StatusFile); err == nil && s.Finished.After(last) {
			last = s.Finished
			next = time.Time{}
		}
		if !time.Now().Before(next) {
			if err := cat.Refresh(ctx); err != nil {
				log.Printf("refresh catalog: %v", err)
				next = time.Now().Add(min(o.refresh, time.Minute))
			} else {
				if err := cache.pruneCompiled(cat.current.Load()); err != nil {
					log.Printf("prune compiled packages from download cache: %v", err)
				}
				stats := cat.Stats()
				if publicStats != nil {
					if err := writePublicStats(publicStats, stats); err != nil {
						log.Printf("write public stats: %v", err)
					}
				}
				log.Printf("catalog checked: %d packages, %d ahead", stats.Packages, stats.Ahead)
				next = time.Now().Add(o.refresh)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
