package poudrine

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"database/sql"
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
		return a.cfg.DB.QueryRow("SELECT private_key,public_key FROM push_identity WHERE id=1").Scan(&a.pushPrivate, &a.pushPublic)
	}
	if err := load(); !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	if _, err = a.cfg.DB.Exec("INSERT INTO push_identity(id,private_key,public_key) VALUES(1,?,?) ON CONFLICT DO NOTHING", priv, pub); err != nil {
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
	_, err = a.cfg.DB.ExecContext(r.Context(), `INSERT INTO push_subscriptions(endpoint,auth,p256dh) VALUES(?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET auth=excluded.auth,p256dh=excluded.p256dh`, sub.Endpoint, sub.Keys.Auth, sub.Keys.P256dh)
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
	if _, err := a.cfg.DB.ExecContext(r.Context(), "DELETE FROM push_subscriptions WHERE endpoint=?", sub.Endpoint); err != nil {
		adminInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type packageAlert struct {
	Body string `json:"body"`
	Tag  string `json:"tag"`
}

type pushBuild struct {
	endpoint         string
	started          int64
	pkg, origin, abi string
}

// A pending result without jail metadata can use the local catalog after the
// bulk is published. A known jail ABI also lets a long bulk report each
// successful package before Poudriere switches its atomic repository.
func (p pushBuild) alert(snapshot *catalogSnapshot) (alert *packageAlert, wait bool) {
	i := strings.LastIndexByte(p.pkg, '-')
	if i < 1 || i == len(p.pkg)-1 {
		return nil, false
	}
	name, version := p.pkg[:i], p.pkg[i+1:]
	upstream, ok := snapshot.freeBSDPackage(name)
	if !ok || comparePkgVersions(version, upstream.Version) <= 0 {
		return nil, false
	}
	origin, abi := p.origin, p.abi
	if abi == "" {
		local, ok := snapshot.compiled[name]
		if !ok || local.Version != version {
			return nil, true
		}
		abi = local.ABI
		if origin == "" {
			origin = local.BuildOrigin
		}
	}
	if !compatibleABI(abi, upstream.ABI) || origin != "" && origin != upstream.BuildOrigin && origin != upstream.Origin {
		return nil, false
	}
	id := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", p.started, p.pkg)))
	return &packageAlert{Body: fmt.Sprintf("%s built locally. FreeBSD has %s.", p.pkg, upstream.Version), Tag: base64.RawURLEncoding.EncodeToString(id[:18])}, false
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
		_, err = a.cfg.DB.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint=? AND auth=? AND p256dh=?", sub.Endpoint, sub.Keys.Auth, sub.Keys.P256dh)
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
func (a *Admin) deliverBuildPush(ctx context.Context, snapshot *catalogSnapshot) error {
	// Keep temporary push outages, missing manifests and offline upstreams
	// bounded even if the catalog never becomes available.
	if _, err := a.cfg.DB.ExecContext(ctx, `DELETE FROM push_build_queue WHERE finished_ns < ?`, time.Now().Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}
	rows, err := a.cfg.DB.QueryContext(ctx, `SELECT endpoint,started_ns,package,origin,abi
	 FROM push_build_queue WHERE next_try <= ? ORDER BY next_try,finished_ns,package LIMIT 64`, time.Now().Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	var batch []pushBuild
	for rows.Next() {
		var p pushBuild
		err = rows.Scan(&p.endpoint, &p.started, &p.pkg, &p.origin, &p.abi)
		if err != nil {
			return err
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		alert, wait := p.alert(snapshot)
		if alert != nil {
			// The user may have unsubscribed (or an earlier response removed this
			// endpoint) while we were sending the other builds in this batch.
			sub := webpush.Subscription{Endpoint: p.endpoint}
			err = a.cfg.DB.QueryRowContext(ctx, `SELECT s.auth,s.p256dh FROM push_subscriptions s
			  JOIN push_build_queue q USING(endpoint) WHERE q.endpoint=? AND q.started_ns=? AND q.package=?`,
				p.endpoint, p.started, p.pkg).Scan(&sub.Keys.Auth, &sub.Keys.P256dh)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			wait, err = a.sendBuildAlert(ctx, &sub, alert)
			if err != nil && ctx.Err() == nil {
				log.Printf("push for %s: %v", p.pkg, err)
			}
		}
		if wait {
			_, err = a.cfg.DB.ExecContext(ctx, `UPDATE push_build_queue SET next_try=?
			WHERE endpoint=? AND started_ns=? AND package=?`, time.Now().Add(time.Minute).Unix(), p.endpoint, p.started, p.pkg)
		} else {
			_, err = a.cfg.DB.ExecContext(ctx, `DELETE FROM push_build_queue WHERE endpoint=? AND started_ns=? AND package=?`, p.endpoint, p.started, p.pkg)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Admin) dispatchBuildPush(ctx context.Context, catalog *Catalog) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := a.deliverBuildPush(ctx, catalog.current.Load()); err != nil && ctx.Err() == nil {
			log.Printf("dispatch package notifications: %v", err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
		}
	}
}
