package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/config"
)

type configUpdate struct {
	Settings json.RawMessage `json:"settings"`
	Revision string          `json:"revision"`
}

// Empty overrides in poudrine.json follow the host and other settings. Keep
// their editable display values separate so an unrelated save preserves that.
func (a *Admin) settingDefaults(ctx context.Context, cfg config.Configuration) (map[string]string, error) {
	o, err := cfg.Options(a.cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	values := map[string]string{"local_repo": o.Builder.RepositoryDir(), "working_hours": "00:00-24:00"}
	if cfg.WebTLS {
		values["tls_cert"], values["tls_key"] = config.AutomaticTLSFiles(o.State)
	}
	port := 0
	if strings.HasSuffix(o.RepoListen, ":0") {
		port = a.cfg.RepositoryPort
	}
	if repo, err := o.AdvertisedRepository(port); err == nil {
		if cfg.RepoURL == "" {
			values["repo_url"] = repo
		}
		if contact, err := defaultPushContact(repo); err == nil {
			values["push_contact"] = contact
		}
	}
	if cfg.Release == "" {
		if release, err := config.HostUserlandRelease(ctx); err == nil {
			values["release"] = release
		}
	}
	if cfg.Upstream == "" {
		if a.cfg.ActiveSettings.Upstream == "" && a.cfg.UpstreamURL != "" {
			values["upstream"] = a.cfg.UpstreamURL
		} else if upstream, err := config.DefaultUpstream(ctx); err == nil {
			values["upstream"] = upstream
		}
	}
	return values, nil
}

func (a *Admin) configuration(w http.ResponseWriter, r *http.Request, body []byte) {
	if a.cfg.ConfigPath == "" {
		http.Error(w, "Configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == broker.MethodQuery {
		var cfg config.Configuration
		var revision string
		var err error
		serverTimeAt := time.Now()
		var serverTime string // Older Management pages display this field verbatim.
		var workerPending bool
		if a.cfg.Broker != nil {
			reply, err := a.cfg.Broker.Read(r, body)
			if err != nil {
				http.Error(w, err.Error(), reply.Status)
				return
			}
			cfg, revision, workerPending, serverTime = *reply.Settings, reply.Revision, reply.WorkerPending, reply.ServerTime
			serverTimeAt = reply.ServerTimeAt
		} else {
			serverTime = serverTimeAt.Format("15:04 MST")
			cfg, revision, err = config.LoadConfiguration(a.cfg.ConfigPath)
			if err != nil {
				if errors.Is(err, config.ErrInvalidConfig) {
					http.Error(w, err.Error()+". Correct the file on the host and reload settings.", http.StatusUnprocessableEntity)
					return
				}
				adminInternal(w, err)
				return
			}
		}
		defaults, err := a.settingDefaults(r.Context(), cfg)
		if err != nil {
			adminInternal(w, err)
			return
		}
		adminJSON(w, struct {
			Settings     config.Configuration `json:"settings"`
			Defaults     map[string]string    `json:"defaults"`
			Revision     string               `json:"revision"`
			Active       bool                 `json:"active"`
			ServerTime   string               `json:"server_time"`
			ServerTimeAt time.Time            `json:"server_time_at,omitzero"`
		}{cfg, defaults, revision, cfg.SameGeneration(a.cfg.ActiveSettings) && !workerPending, serverTime, serverTimeAt})
		return
	}
	var revision string
	if a.cfg.Broker != nil {
		var status int
		var err error
		revision, status, err = a.cfg.Broker.Submit(r, body)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
	} else {
		var update configUpdate
		if err := json.Unmarshal(body, &update); err != nil || update.Revision == "" || len(update.Settings) == 0 {
			http.Error(w, "Expected settings and revision", http.StatusBadRequest)
			return
		}
		var err error
		revision, err = config.SaveConfiguration(a.cfg.ConfigPath, update.Revision, update.Settings)
		if errors.Is(err, config.ErrConfigConflict) {
			http.Error(w, "Configuration was edited elsewhere. Reload settings before saving.", http.StatusConflict)
			return
		}
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			adminInternal(w, err)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	adminJSON(w, map[string]string{"revision": revision})
}
