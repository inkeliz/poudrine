package admin

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	catalogpkg "github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/weblocale"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

func defaultPushContact(repositoryURL string) (string, error) {
	repository, err := url.Parse(repositoryURL)
	if err != nil {
		return "", err
	}
	host := repository.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.WithZone("").Unmap()
		host = "[" + ip.String() + "]"
		if ip.Is6() {
			host = "[IPv6:" + ip.String() + "]"
		}
	}
	return "postmaster@" + host, nil
}

func (a *Admin) initPush() error {
	a.pushClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a.pushContact = strings.TrimPrefix(a.cfg.PushContact, "mailto:")
	if a.pushContact == "" {
		contact, err := defaultPushContact(a.cfg.RepositoryURL)
		if err != nil {
			return err
		}
		a.pushContact = url.PathEscape(contact)
	}
	load := func() error {
		var err error
		a.pushPrivate, a.pushPublic, err = a.cfg.DB.PushKeys()
		return err
	}
	if err := load(); !errors.Is(err, database.ErrNotFound) {
		return err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	if err = a.cfg.DB.SavePushKeys(priv, pub); err != nil {
		return err
	}
	return load()
}

func (a *Admin) subscribe(w http.ResponseWriter, r *http.Request, body []byte) {
	var sub webpush.Subscription
	if err := json.Unmarshal(body, &sub); err != nil {
		http.Error(w, "Invalid subscription", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(sub.Endpoint)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || len(sub.Endpoint) > 4096 {
		http.Error(w, "Expected an HTTPS push endpoint", http.StatusBadRequest)
		return
	}
	auth, authErr := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
	raw, keyErr := base64.RawURLEncoding.DecodeString(sub.Keys.P256dh)
	_, curveErr := ecdh.P256().NewPublicKey(raw)
	if authErr != nil || len(auth) != 16 || keyErr != nil || curveErr != nil {
		http.Error(w, "Invalid push encryption keys", http.StatusBadRequest)
		return
	}
	err = a.cfg.DB.SubscribePush(r.Context(), sub.Endpoint, sub.Keys.Auth, sub.Keys.P256dh, weblocale.ForRequest(r).String())
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) unsubscribe(w http.ResponseWriter, r *http.Request, body []byte) {
	var sub struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(body, &sub); err != nil || sub.Endpoint == "" {
		http.Error(w, "Expected a subscription endpoint", http.StatusBadRequest)
		return
	}
	if err := a.cfg.DB.UnsubscribePush(r.Context(), sub.Endpoint); err != nil {
		adminInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type packageAlert struct {
	Body string `json:"body"`
	Tag  string `json:"tag"`
}

// A pending result without jail metadata can use the local catalog after the
// bulk is published. A known jail ABI also lets a long bulk report each
// successful package before Poudriere switches its atomic repository.
func buildAlert(p database.PushBuild, snapshot *catalogpkg.Snapshot) (alert *packageAlert, wait bool) {
	name, version := packageinfo.SplitBuildPackage(p.Pkg)
	if name == "" {
		return nil, false
	}
	upstream, ok := snapshot.FreeBSDPackage(name)
	if !ok || catalogpkg.ComparePkgVersions(version, upstream.Version) <= 0 {
		return nil, false
	}
	origin, abi := p.Origin, p.ABI
	if abi == "" {
		local, ok := snapshot.Compiled[name]
		if !ok || local.Version != version {
			return nil, true
		}
		abi = local.ABI
		if origin == "" {
			origin = local.BuildOrigin
		}
	}
	if !packageinfo.CompatibleABI(abi, upstream.ABI) || origin != "" && origin != upstream.BuildOrigin && origin != upstream.Origin {
		return nil, false
	}
	id := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", p.Started, p.Pkg)))
	printer := message.NewPrinter(language.Make(p.Language))
	return &packageAlert{Body: printer.Sprintf("%s built locally. FreeBSD has %s.", p.Pkg, upstream.Version), Tag: base64.RawURLEncoding.EncodeToString(id[:18])}, false
}

// retry distinguishes a temporary transport/service failure from an invalid
// delivery. A removed subscription also removes its remaining queued work.
func (a *Admin) sendBuildAlert(ctx context.Context, sub *webpush.Subscription, alert *packageAlert) (retry bool, err error) {
	payload, err := json.Marshal(alert)
	if err != nil {
		return false, err
	}
	response, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
		Subscriber: a.pushContact, VAPIDPublicKey: a.pushPublic, VAPIDPrivateKey: a.pushPrivate,
		HTTPClient: a.pushClient, TTL: 3600, Topic: alert.Tag,
	})
	if err != nil {
		// Push endpoint paths contain bearer tokens. Keep them out of logs.
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			return true, wrapped.Err
		}
		return true, err
	}
	response.Body.Close()
	code := response.StatusCode
	switch {
	case code == http.StatusNotFound || code == http.StatusGone:
		err = a.cfg.DB.RemovePushSubscription(ctx, sub.Endpoint, sub.Keys.Auth, sub.Keys.P256dh)
		return err != nil, err
	case code/100 == 2:
		return false, nil
	default:
		return code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code == http.StatusTooEarly, fmt.Errorf("push service returned HTTP %d", code)
	}
}

// Only the web process holding service.lock dispatches. Successful endpoints
// leave the queue individually. If a crash follows network acceptance, Web Push
// Topic and the browser notification tag coalesce a retry of that same build.
func (a *Admin) deliverBuildPush(ctx context.Context, snapshot *catalogpkg.Snapshot) error {
	// Keep temporary push outages, missing manifests and offline upstreams
	// bounded even if the catalog never becomes available.
	if err := a.cfg.DB.ExpireBuildPush(ctx, time.Now()); err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}
	batch, err := a.cfg.DB.PendingBuildPush(ctx, time.Now())
	if err != nil {
		return err
	}
	for _, p := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		alert, wait := buildAlert(p, snapshot)
		if alert != nil {
			// The user may have unsubscribed (or an earlier response removed this
			// endpoint) while we were sending the other builds in this batch.
			sub := webpush.Subscription{Endpoint: p.Endpoint}
			sub.Keys.Auth, sub.Keys.P256dh, err = a.cfg.DB.BuildPushSubscription(ctx, p)
			if errors.Is(err, database.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			wait, err = a.sendBuildAlert(ctx, &sub, alert)
			if err != nil && ctx.Err() == nil {
				log.Printf("push for %s: %v", p.Pkg, err)
			}
		}
		err = a.cfg.DB.FinishBuildPush(ctx, p, wait, time.Now())
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Admin) DispatchBuildPush(ctx context.Context, catalog *catalogpkg.Catalog) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := a.deliverBuildPush(ctx, catalog.Current.Load()); err != nil && ctx.Err() == nil {
			log.Printf("dispatch package notifications: %v", err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
		}
	}
}
