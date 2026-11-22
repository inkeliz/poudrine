package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// The website can resolve an ephemeral listener. On the host console, use
// the configured port or the full address supplied by the operator.
func (o Options) AdvertisedRepository(boundPort int) (string, error) {
	if o.PublicURL != "" {
		return o.PublicURL, nil
	}
	host, port, err := net.SplitHostPort(o.RepoListen)
	if err != nil {
		return "", err
	}
	if boundPort != 0 {
		port = strconv.Itoa(boundPort)
	} else if port == "0" {
		return "", errors.New("set repo_url or a fixed repo_listen port to generate client setup offline")
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host, err = os.Hostname()
		if err != nil {
			return "", err
		}
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String(), nil
}

// Check what can be verified before interrupting a running build.
func PreflightConfiguration(ctx context.Context, mode string, o Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode != "serve" && os.Geteuid() == 0 {
		if err := o.CheckWorkerStorage(mode); err != nil {
			return err
		}
	}
	if mode == "worker" {
		return nil
	}
	return ValidateWebTLS(ctx, o, mode == "run")
}

// Working hours are applied directly to an active worker. Overlay builds
// require a new worker generation, even when HTTP already enforces the policy.
func (c Configuration) SameGeneration(other Configuration) bool {
	c.WorkingHours = other.WorkingHours
	overlays := c.PackageOverlays
	c.PackageOverlays = other.PackageOverlays
	return reflect.DeepEqual(c, other) && overlays.Equal(other.PackageOverlays)
}

type Options struct {
	WebTLS                                                                                                                  bool          `json:"-"`
	TLSCert, TLSKey                                                                                                         string        `json:"-"`
	ConfigRevision                                                                                                          string        `json:"-"`
	ConfigSettings                                                                                                          Configuration `json:"-"`
	ConfigPath                                                                                                              string        `json:"-"`
	Builder                                                                                                                 Builder       `json:"-"`
	State, RepoListen, WebListen, PublicURL, WebsiteURL, Upstream, Fingerprints, LocalRepo, Proxy, PushContact, ServiceUser string        `json:"-"`
	CacheGB                                                                                                                 float64       `json:"-"`
	Refresh                                                                                                                 time.Duration `json:"-"`
}

func WebURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("invalid repository base URL %q", raw)
	}
	return u, nil
}

func HostUserlandRelease(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "freebsd-version", "-u").Output()
	return strings.TrimSpace(string(out)), err
}

func hostPackageABI(ctx context.Context) (string, error) {
	// The base-system pkg shim may prompt to bootstrap on a new host.
	if out, err := exec.CommandContext(ctx, "/usr/local/sbin/pkg", "config", "ABI").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	release, err := HostUserlandRelease(ctx)
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

func DefaultUpstream(ctx context.Context) (string, error) {
	abi, err := hostPackageABI(ctx)
	if err != nil {
		return "", err
	}
	return "https://pkg.freebsd.org/" + abi + "/latest", nil
}
