package admin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
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

	brokerpkg "github.com/inkeliz/poudrine/internal/broker"
	builderpkg "github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/fsutil"
	"github.com/inkeliz/poudrine/internal/requestauth"
	"github.com/inkeliz/poudrine/internal/routes"
	webtheme "github.com/inkeliz/poudrine/internal/web"
	poudrineweb "github.com/inkeliz/poudrine/internal/web/poudrine"
	"github.com/inkeliz/poudrine/internal/weblocale"
)

type Config struct {
	ConfigPath                string
	ActiveSettings            config.Configuration
	Builder                   config.Builder    `json:"-"`
	Broker                    *brokerpkg.Client `json:"-"`
	PinnedKey                 ed25519.PublicKey `json:"-"`
	DB                        *database.DB
	DataDir                   string
	RepositoryURL, WebsiteURL string
	RepositoryPort            int
	UpstreamURL               string
	PushContact               string
	PublicKey                 func() string
	Stats                     func() catalog.CatalogStats
	Clients                   func(context.Context) ([]database.ClientRecord, error)
	Tracked                   func(context.Context) ([]database.TrackedPackageRecord, error)
	Packages                  func(context.Context, []string) (catalog.PackageInventoryView, error)
	Build                     func() (builderpkg.BuildStatus, error)
}

type Admin struct {
	cfg                                  Config
	pushPublic, pushPrivate, pushContact string
	pushClient                           *http.Client
}

func New(cfg Config) (*Admin, error) {
	if err := cfg.DB.PrepareAdmin(); err != nil {
		return nil, err
	}
	a := &Admin{cfg: cfg}
	if err := a.initPush(); err != nil {
		return nil, err
	}
	if len(cfg.PinnedKey) > 0 {
		if len(cfg.PinnedKey) != ed25519.PublicKeySize {
			return nil, errors.New("invalid supervisor admin identity")
		}
		err := cfg.DB.PinAdmin(cfg.PinnedKey)
		if err != nil {
			return nil, err
		}
	}
	enrolled, err := cfg.DB.AdminEnrolled()
	if err != nil {
		return nil, err
	}
	if !enrolled && cfg.Broker == nil {
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
	var raw [32]byte
	rand.Read(raw[:])
	token := base64.RawURLEncoding.EncodeToString(raw[:]) + "\n"
	return fsutil.CreateFileOnce(a.tokenPath(), []byte(token), 0600)
}

var managementPage = template.Must(template.ParseFS(poudrineweb.Files, "admin.html"))

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /admin", a.management)
	mux.HandleFunc("GET /fonts/{font}", webtheme.ServeFont)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		http.ServeContent(w, r, "style.css", time.Time{}, strings.NewReader(poudrineweb.Stylesheet))
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
	mux.HandleFunc(routes.Status.Pattern(), a.signedEmptyQuery(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		adminJSON(w, struct {
			catalog.CatalogStats
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
	mux.HandleFunc(routes.Setup.Pattern(), a.signedEmptyQuery(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		adminJSON(w, map[string]bool{"enrolled": true})
	}))
	mux.HandleFunc(routes.Enroll.Pattern(), a.enroll)
	mux.HandleFunc(routes.Login.Pattern(), a.signed(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc(routes.ReadConfig.Pattern(), a.signedEmptyQuery(a.configuration))
	mux.HandleFunc(routes.SaveConfig.Pattern(), a.signed(a.configuration))
	mux.HandleFunc(routes.BuildStatus.Pattern(), a.signedEmptyQuery(a.buildStatus))
	mux.HandleFunc(routes.BuildEvents.Pattern(), a.signedAs(a.buildEvents))
	mux.HandleFunc(routes.Origins.Pattern(), a.signedEmptyQuery(a.buildOrigins))
	mux.HandleFunc(routes.Packages.Pattern(), a.signedEmptyQuery(a.packageInventory))
	mux.HandleFunc(routes.ClientSummary.Pattern(), a.signedEmptyQuery(a.clientSummaries))
	mux.HandleFunc(routes.ClientPackages.Pattern(), a.signed(queryJSON(a.clientPackages, writeDBQueryError)))
	mux.HandleFunc(routes.BuildIssues.Pattern(), a.signed(queryJSON(a.buildIssues, writeDiagnosticError)))
	mux.HandleFunc(routes.BuildResults.Pattern(), a.signed(queryJSON(a.buildResults, writeDiagnosticError)))
	mux.HandleFunc(routes.BuildLog.Pattern(), a.signed(queryJSON(a.packageLog, writeDiagnosticError)))
	mux.HandleFunc(routes.RetryBuild.Pattern(), a.signed(a.queueBuild))
	mux.HandleFunc(routes.ForceBuild.Pattern(), a.signed(a.queueBuild))
	mux.HandleFunc(routes.BuildHistory.Pattern(), a.signed(queryJSON(a.buildHistory, writeDBQueryError)))
	mux.HandleFunc(routes.HistoryPackages.Pattern(), a.signed(queryJSON(a.cfg.DB.BuildHistoryPackages, writeDBQueryError)))
	mux.HandleFunc(routes.Clients.Pattern(), a.signedEmptyQuery(listView(a.cfg.Clients)))
	mux.HandleFunc(routes.Tracked.Pattern(), a.signedEmptyQuery(listView(a.cfg.Tracked)))
	mux.HandleFunc(routes.PushKey.Pattern(), a.signedEmptyQuery(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		adminJSON(w, map[string]string{"public_key": a.pushPublic})
	}))
	mux.HandleFunc(routes.Subscribe.Pattern(), a.signed(a.subscribe))
	mux.HandleFunc(routes.Unsubscribe.Pattern(), a.signed(a.unsubscribe))
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
	if a.cfg.Broker != nil {
		return a.cfg.Broker.PublicKey()
	}
	return a.cfg.DB.AdminKey(ctx)
}

func (a *Admin) management(w http.ResponseWriter, r *http.Request) {
	key, err := a.enrolledKey(r.Context())
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := managementPage.Execute(w, struct {
		Locale   weblocale.Page
		Enrolled bool
	}{weblocale.ForPage(w, r), len(key) != 0}); err != nil {
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
	if routes.SaveConfig.Matches(r.Method, r.URL.Path) {
		limit = config.MaxConfigBytes + 1024
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
	if a.cfg.Broker != nil {
		_, status, err := a.cfg.Broker.Submit(r, body)
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
	n, err := a.cfg.DB.EnrollAdmin(r.Context(), key)
	if err != nil {
		adminInternal(w, err)
		return
	}
	if n == 0 {
		http.Error(w, "Enrollment closed", http.StatusConflict)
		return
	}
	if a.cfg.Broker == nil {
		if err := os.Remove(a.tokenPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove consumed setup token: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func queryArgs(w http.ResponseWriter, body []byte, args any) bool {
	if !brokerpkg.ValidQuery(body, args) {
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

func listView[T any](load func(context.Context) (T, error)) func(http.ResponseWriter, *http.Request, []byte) {
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
		used, err := a.cfg.DB.ConsumeAdminNonce(r.Context(), nonce, stamp)
		if err != nil {
			adminInternal(w, err)
			return
		}
		if !used {
			http.Error(w, "Request already processed", http.StatusConflict)
			return
		}
		next(w, r, body, key)
	}
}
