package service

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/admin"
	brokerpkg "github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/public"
)

func serve(ctx context.Context, o config.Options) error {
	var broker *brokerpkg.Client
	var pinned ed25519.PublicKey
	var publicStats *os.File
	if os.Getenv("POUDRINE_SUPERVISED") == "1" {
		if os.Getenv("POUDRINE_SUPERVISOR_FD") != "5" {
			return errors.New("missing supervisor lifetime pipe")
		}
		var stop context.CancelFunc
		var err error
		ctx, stop, err = watchSupervisor(ctx, 5)
		if err != nil {
			return err
		}
		defer stop()
		syscall.CloseOnExec(3)
		syscall.CloseOnExec(4)
		f := os.NewFile(4, "config-broker")
		defer f.Close()
		broker = brokerpkg.NewClient(f)
		if os.Getenv("POUDRINE_STATS_FD") == "6" {
			syscall.CloseOnExec(6)
			publicStats = os.NewFile(6, config.PublicStatsFile)
			defer publicStats.Close()
		}
		pinned, err = base64.RawURLEncoding.DecodeString(os.Getenv("POUDRINE_ADMIN_KEY"))
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(o.State, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(o.State, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock web service state: %w", err)
	}
	if broker == nil {
		publicStats, err = public.OpenStatsWriter(filepath.Join(o.State, config.StandaloneStatsName))
		if err != nil {
			log.Printf("public stats unavailable: %v", err)
		} else {
			defer publicStats.Close()
		}
	}
	if o.Upstream == "" {
		o.Upstream, err = config.DefaultUpstream(ctx)
		if err != nil {
			return fmt.Errorf("detect upstream ABI (or configure upstream): %w", err)
		}
	}
	up, err := config.WebURL(o.Upstream)
	if err != nil {
		return err
	}
	repoListener, err := listenHTTP(o.RepoListen)
	if err != nil {
		return err
	}
	defer repoListener.Close()
	webListener, err := listenHTTP(o.WebListen)
	if err != nil {
		return err
	}
	defer webListener.Close()
	o.PublicURL, err = o.AdvertisedRepository(repoListener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	if _, err := config.WebURL(o.PublicURL); err != nil {
		return err
	}
	var proxy *net.IPNet
	if o.Proxy != "" {
		_, proxy, err = net.ParseCIDR(o.Proxy)
		if err != nil {
			return err
		}
	}
	db, err := database.Open(o.State, false)
	if err != nil {
		return err
	}
	defer db.Close()
	store := *db
	client := &http.Client{Timeout: 30 * time.Minute}
	cat, err := catalog.New(o.State, up.String(), o.Fingerprints, o.LocalRepo, client)
	if err != nil {
		return err
	}
	cat.JailRoot = o.Builder.BuildJailRoot()
	cat.Overlays = o.Builder.PackageOverlays
	cache, err := catalog.NewCache(filepath.Join(o.State, "cache"), int64(o.CacheGB*1e9), up, client)
	if err != nil {
		return err
	}
	packages := func(ctx context.Context, origins []string) (catalog.PackageInventoryView, error) {
		usage, err := store.TrackedPackages(ctx)
		if err != nil {
			return catalog.PackageInventoryView{}, err
		}
		builds, err := store.RecentInventoryBuilds(ctx)
		if err != nil {
			return catalog.PackageInventoryView{}, err
		}
		snapshot := cat.Current.Load()
		view := catalog.ManagementPackages(snapshot, usage, origins, cache.Inventory(snapshot), builder.ResolvedPorts(o.Builder), builds)
		catalog.DescribeUnpublished(view.Packages, snapshot, cat.LiveScan.Load())
		view.CacheLimit = cache.Limit
		return view, catalog.LoadBuildTimes(ctx, store, snapshot, view.Packages)
	}
	api, err := admin.New(ctx, admin.Config{ConfigPath: o.ConfigPath, ActiveSettings: o.ConfigSettings, Builder: o.Builder, Broker: broker, PinnedKey: pinned, DB: db, DataDir: o.State, RepositoryURL: o.PublicURL, WebsiteURL: o.WebsiteURL,
		RepositoryPort: repoListener.Addr().(*net.TCPAddr).Port, UpstreamURL: o.Upstream,
		PushContact: o.PushContact, PublicKey: func() string { return string(cat.PublicKey) }, Stats: cat.Stats, Clients: store.Clients, Tracked: store.Tracked, Packages: packages, Build: func() (builder.BuildStatus, error) {
			s, err := builder.ReadBuildStatus(o.Builder.StatusFile)
			if errors.Is(err, os.ErrNotExist) {
				return builder.BuildStatus{Stage: "waiting", Message: "Waiting for the build worker"}, nil
			}
			return s, err
		}})
	if err != nil {
		return err
	}
	repo := &public.Repository{Catalog: cat, Store: store, Cache: cache, LocalDir: o.LocalRepo, Proxy: proxy}
	return serveHTTP(ctx, o, repoListener, webListener, repo, api, publicStats)
}

func monitor(ctx context.Context, o config.Options, cat *catalog.Catalog, cache *catalog.Cache, publicStats *os.File) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var next, last time.Time
	var reported *catalog.Snapshot
	if s, err := builder.ReadBuildStatus(o.Builder.StatusFile); err == nil {
		last = s.Finished
	}
	for ctx.Err() == nil {
		if s, err := builder.ReadBuildStatus(o.Builder.StatusFile); err == nil && s.Finished.After(last) {
			last = s.Finished
			next = time.Time{}
		}
		checked := false
		if !time.Now().Before(next) {
			if err := cat.Refresh(ctx); err != nil {
				log.Printf("refresh catalog: %v", err)
				next = time.Now().Add(min(o.Refresh, time.Minute))
			} else {
				checked = true
				next = time.Now().Add(o.Refresh)
			}
		}
		if snapshot := cat.Current.Load(); snapshot != nil && (checked || snapshot != reported) {
			reported = snapshot
			if err := cache.PruneCompiled(snapshot); err != nil {
				log.Printf("prune compiled packages from download cache: %v", err)
			}
			stats := cat.Stats()
			if publicStats != nil {
				if err := public.WritePublicStats(publicStats, stats); err != nil {
					log.Printf("write public stats: %v", err)
				}
			}
			log.Printf("catalog checked: %d packages, %d ahead", stats.Packages, stats.Ahead)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
