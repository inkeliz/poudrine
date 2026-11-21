package poudrine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
	"github.com/inkeliz/poudrine/web/poudrine"
)

type AdminConfig struct {
	ConfigPath                string
	ActiveSettings            Configuration
	builder                   BuilderConfig
	broker                    *configClient
	pinnedKey                 ed25519.PublicKey
	DB                        *sql.DB
	DataDir                   string
	RepositoryURL, WebsiteURL string
	RepositoryPort            int
	UpstreamURL               string
	PushContact               string
	PublicKey                 func() string
	Stats                     func() CatalogStats
	Clients, Tracked          func(context.Context) (any, error)
	Packages                  func(context.Context, []string) (packageInventoryView, error)
	Build                     func() (BuildStatus, error)
}

type Admin struct {
	cfg                                  AdminConfig
	pushPublic, pushPrivate, pushContact string
	pushClient                           *http.Client
}

const adminSchema = `
CREATE TABLE IF NOT EXISTS admin_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	public_key BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_nonces (
	nonce BLOB PRIMARY KEY,
	expires INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS admin_nonce_expiry ON admin_nonces(expires);
CREATE TABLE IF NOT EXISTS push_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	private_key TEXT NOT NULL,
	public_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS push_subscriptions (
	endpoint TEXT PRIMARY KEY,
	auth TEXT NOT NULL,
	p256dh TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS push_watch_new AFTER INSERT ON push_subscriptions BEGIN
 INSERT INTO push_build_watch VALUES(NEW.endpoint, CAST(unixepoch('subsec') * 1000000000 AS INTEGER));
END;
CREATE TRIGGER IF NOT EXISTS push_unwatch AFTER DELETE ON push_subscriptions BEGIN
 DELETE FROM push_build_queue WHERE endpoint=OLD.endpoint;
 DELETE FROM push_build_watch WHERE endpoint=OLD.endpoint;
END;
INSERT OR IGNORE INTO push_build_watch
 SELECT endpoint, CAST(unixepoch('subsec') * 1000000000 AS INTEGER) FROM push_subscriptions;`

func NewAdmin(cfg AdminConfig) (*Admin, error) {
	if _, err := cfg.DB.Exec(buildHistorySchema + adminSchema); err != nil {
		return nil, err
	}
	a := &Admin{cfg: cfg}
	if err := a.initPush(); err != nil {
		return nil, err
	}
	if len(cfg.pinnedKey) > 0 {
		if len(cfg.pinnedKey) != ed25519.PublicKeySize {
			return nil, errors.New("invalid supervisor admin identity")
		}
		_, err := cfg.DB.Exec("INSERT INTO admin_identity(id,public_key) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET public_key=excluded.public_key", []byte(cfg.pinnedKey))
		if err != nil {
			return nil, err
		}
	}
	var enrolled bool
	if err := cfg.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM admin_identity)").Scan(&enrolled); err != nil {
		return nil, err
	}
	if !enrolled && cfg.broker == nil {
		if err := a.prepareSetupToken(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *Admin) tokenPath() string { return filepath.Join(a.cfg.DataDir, "setup-token") }

func (a *Admin) prepareSetupToken() error {
	if err := os.MkdirAll(a.cfg.DataDir, 0700); err != nil {
		return err
	}
	// Link the completed private file into place without replacing an existing token.
	f, err := os.CreateTemp(a.cfg.DataDir, ".setup-token-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	var raw [32]byte
	rand.Read(raw[:])
	_, err = fmt.Fprintln(f, base64.RawURLEncoding.EncodeToString(raw[:]))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	err = os.Link(f.Name(), a.tokenPath())
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

const methodQuery = requestauth.Query

var managementPage = template.Must(template.ParseFS(poudrineweb.Files, "admin.html"))

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /admin", a.management)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, poudrineweb.Files, "style.css")
	})
	script := func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, poudrineweb.Files, "poudrine.js")
	}
	mux.HandleFunc("GET /poudrine.js", script)
	// Keep existing push subscriptions registered under their original worker URL.
	mux.HandleFunc("GET /sw.js", script)
	// Previously served pages load the application from one of these addresses.
	mux.HandleFunc("GET /admin.js", script)
	mux.HandleFunc("GET /status.js", script)
	// Older Management pages load these scripts before admin.js, which now includes both.
	emptyScript := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("GET /builds.js", emptyScript)
	mux.HandleFunc("GET /inventory.js", emptyScript)
	mux.HandleFunc("QUERY /api/status", a.signedEmptyQuery(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		adminJSON(w, struct {
			CatalogStats
			RepositoryURL string `json:"repository_url"`
			WebsiteURL    string `json:"website_url,omitempty"`
		}{a.cfg.Stats(), a.cfg.RepositoryURL, a.cfg.WebsiteURL})
	}))
	mux.HandleFunc("GET /setup-client.sh", a.clientSetup)
	mux.HandleFunc("GET /repository.pub", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="poudrine.pub"`)
		fmt.Fprint(w, a.cfg.PublicKey())
	})
	mux.HandleFunc("QUERY /api/setup", a.signedEmptyQuery(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		adminJSON(w, map[string]bool{"enrolled": true})
	}))
	mux.HandleFunc("POST /api/setup", a.enroll)
	mux.HandleFunc("POST /api/admin/login", a.signed(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("QUERY /api/admin/config", a.signedEmptyQuery(a.configuration))
	mux.HandleFunc("PUT /api/admin/config", a.signed(a.configuration))
	mux.HandleFunc("QUERY /api/admin/build", a.signedEmptyQuery(a.buildStatus))
	mux.HandleFunc("QUERY /api/admin/build/events", a.signedAs(a.buildEvents))
	mux.HandleFunc("QUERY /api/admin/origins", a.signedEmptyQuery(a.buildOrigins))
	mux.HandleFunc("QUERY /api/admin/packages", a.signedEmptyQuery(a.packageInventory))
	mux.HandleFunc("QUERY /api/admin/clients/summary", a.signedEmptyQuery(a.clientSummaries))
	mux.HandleFunc("QUERY /api/admin/clients/packages", a.signed(a.clientPackages))
	mux.HandleFunc("QUERY /api/admin/build/issues", a.signed(a.buildIssues))
	mux.HandleFunc("QUERY /api/admin/build/log", a.signed(a.packageLog))
	mux.HandleFunc("POST /api/admin/build/retry", a.signed(a.queueBuild))
	mux.HandleFunc("POST /api/admin/build/force", a.signed(a.queueBuild))
	mux.HandleFunc("QUERY /api/admin/build/history", a.signed(a.buildHistory))
	mux.HandleFunc("QUERY /api/admin/clients", a.signedEmptyQuery(listView(a.cfg.Clients)))
	mux.HandleFunc("QUERY /api/admin/tracked", a.signedEmptyQuery(listView(a.cfg.Tracked)))
	mux.HandleFunc("QUERY /api/admin/push-key", a.signedEmptyQuery(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		adminJSON(w, map[string]string{"public_key": a.pushPublic})
	}))
	mux.HandleFunc("POST /api/admin/subscription", a.signed(a.subscribe))
	mux.HandleFunc("DELETE /api/admin/subscription", a.signed(a.unsubscribe))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") && (r.URL.RawQuery != "" || r.URL.ForceQuery) {
			http.Error(w, "Send API parameters in the signed request body", http.StatusBadRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *Admin) enrolledKey(ctx context.Context) (ed25519.PublicKey, error) {
	if a.cfg.broker != nil {
		return a.cfg.broker.publicKey()
	}
	var key []byte
	err := a.cfg.DB.QueryRowContext(ctx, "SELECT public_key FROM admin_identity WHERE id=1").Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return key, err
}

func (a *Admin) management(w http.ResponseWriter, r *http.Request) {
	key, err := a.enrolledKey(r.Context())
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := managementPage.Execute(w, struct{ Enrolled bool }{len(key) != 0}); err != nil {
		log.Printf("management page: %v", err)
	}
}

func adminInternal(w http.ResponseWriter, err error) {
	log.Printf("management: %v", err)
	http.Error(w, "Management service unavailable", http.StatusServiceUnavailable)
}

func adminJSON(w http.ResponseWriter, v any) {
	adminJSONStatus(w, http.StatusOK, v)
}

func adminJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("management response: %v", err)
	}
}

// The signature covers the exact wire target and body. UTC timestamps allow
// five minutes of skew. A successful private request consumes its nonce in SQLite.
func adminProof(w http.ResponseWriter, r *http.Request, key ed25519.PublicKey) ([]byte, []byte, int64, bool) {
	limit := int64(16 << 10)
	if r.Method == http.MethodPut && r.URL.Path == "/api/admin/config" {
		limit = maxConfigBytes + 1024
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		http.Error(w, "Invalid or oversized body", http.StatusBadRequest)
		return nil, nil, 0, false
	}
	nonce, stamp, ok := requestauth.Verify(key, r.Method, r.URL.RequestURI(),
		r.Header.Get("X-Poudrine-Time"), r.Header.Get("X-Poudrine-Nonce"), r.Header.Get("X-Poudrine-Signature"), body)
	if !ok {
		http.Error(w, "Invalid request signature", http.StatusUnauthorized)
		return nil, nil, 0, false
	}
	return body, nonce, stamp, true
}

func (a *Admin) enroll(w http.ResponseWriter, r *http.Request) {
	key := requestauth.PublicKey(r.Header.Get("X-Poudrine-Key"))
	if key == nil {
		http.Error(w, "Invalid enrollment key", http.StatusBadRequest)
		return
	}
	body, _, _, ok := adminProof(w, r, key)
	if !ok {
		return
	}
	var registration struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &registration); err != nil {
		http.Error(w, "Invalid enrollment request", http.StatusBadRequest)
		return
	}
	if a.cfg.broker != nil {
		_, status, err := a.cfg.broker.submit(r, body)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
	} else {
		expected, err := os.ReadFile(a.tokenPath())
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "Enrollment closed", http.StatusConflict)
			return
		}
		if err != nil {
			adminInternal(w, err)
			return
		}
		if len(registration.Token) != 43 || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(expected))), []byte(registration.Token)) != 1 {
			http.Error(w, "Invalid setup token", http.StatusForbidden)
			return
		}
	}
	res, err := a.cfg.DB.ExecContext(r.Context(), "INSERT INTO admin_identity(id,public_key) VALUES(1,?) ON CONFLICT(id) DO NOTHING", key)
	if err != nil {
		adminInternal(w, err)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		adminInternal(w, err)
		return
	}
	if n == 0 {
		http.Error(w, "Enrollment closed", http.StatusConflict)
		return
	}
	if a.cfg.broker == nil {
		if err := os.Remove(a.tokenPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove consumed setup token: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Each QUERY has one JSON object. Its exact bytes have already been signed,
// including any whitespace. Refuse unknown fields and trailing values.
func validQuery(body []byte, args any) bool {
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(args) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func queryArgs(w http.ResponseWriter, body []byte, args any) bool {
	if !validQuery(body, args) {
		http.Error(w, "Invalid QUERY body", http.StatusBadRequest)
		return false
	}
	return true
}

func (a *Admin) signedEmptyQuery(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return a.signed(func(w http.ResponseWriter, r *http.Request, body []byte) {
		if queryArgs(w, body, &struct{}{}) {
			next(w, r, body)
		}
	})
}

func listView(load func(context.Context) (any, error)) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		result, err := load(r.Context())
		if err != nil {
			adminInternal(w, err)
			return
		}
		adminJSON(w, result)
	}
}

func (a *Admin) signed(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return a.signedAs(func(w http.ResponseWriter, r *http.Request, body []byte, _ ed25519.PublicKey) {
		next(w, r, body)
	})
}

func (a *Admin) signedAs(next func(http.ResponseWriter, *http.Request, []byte, ed25519.PublicKey)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := a.enrolledKey(r.Context())
		if err != nil {
			adminInternal(w, err)
			return
		}
		if len(key) == 0 {
			http.Error(w, "Administrator enrollment required", http.StatusUnauthorized)
			return
		}
		body, nonce, stamp, ok := adminProof(w, r, key)
		if !ok {
			return
		}
		// Expired nonces are pruned only after successful verification.
		tx, err := a.cfg.DB.BeginTx(r.Context(), nil)
		if err != nil {
			adminInternal(w, err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec("DELETE FROM admin_nonces WHERE expires < ?", time.Now().Unix()); err != nil {
			adminInternal(w, err)
			return
		}
		res, err := tx.Exec("INSERT INTO admin_nonces(nonce,expires) VALUES(?,?) ON CONFLICT DO NOTHING", nonce, stamp+requestauth.ClockSkewSeconds)
		if err != nil {
			adminInternal(w, err)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			adminInternal(w, err)
			return
		}
		if n == 0 {
			http.Error(w, "Request already processed", http.StatusConflict)
			return
		}
		if err := tx.Commit(); err != nil {
			adminInternal(w, err)
			return
		}
		next(w, r, body, key)
	}
}
