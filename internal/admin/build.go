package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/routes"
)

type buildView struct {
	builder.BuildStatus
	CanForce bool `json:"can_force"`
}

func (a *Admin) workerRequest(r *http.Request, body []byte) (broker.Reply, error) {
	reply, err := a.cfg.Broker.Exchange(broker.ForwardedConfigRequest(r, body))
	if err != nil {
		return broker.Reply{Status: http.StatusServiceUnavailable}, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

type snapshotPolicy int

const (
	requireWorker snapshotPolicy = iota
	allowPartialInventory
)

// The worker owns the configured package list and build status. Standalone
// HTTP reads its files; the supervised child forwards the exact signed bytes.
func (a *Admin) workerSnapshot(r *http.Request, body []byte, policy snapshotPolicy) (broker.Reply, error) {
	var reply broker.Reply
	var err error
	needOrigins := r.URL.Path == routes.Origins.Path || r.URL.Path == routes.Packages.Path
	if a.cfg.Broker != nil {
		reply, err = a.workerRequest(r, body)
		if err == nil && reply.Status != http.StatusOK {
			reply.Status = http.StatusServiceUnavailable
			err = errors.New("unexpected worker snapshot response")
		}
		if err != nil {
			// Only authenticated successful replies supply worker-owned fields.
			reply = broker.Reply{Status: reply.Status}
		}
	} else {
		reply.Status = http.StatusOK
		if needOrigins {
			reply.Origins = builder.ConfiguredOrigins(a.cfg.ActiveSettings.PackagesFile)
		}
		if r.URL.Path != routes.Origins.Path && a.cfg.Build != nil {
			var status builder.BuildStatus
			status, err = a.cfg.Build()
			if err == nil {
				reply.Build = &status
				reply.PoudriereBytes = status.PoudriereBytes
			} else {
				reply.Status = http.StatusServiceUnavailable
			}
		}
	}
	if err != nil && (policy == requireWorker || reply.Status < 500) {
		return reply, err
	}
	if needOrigins {
		reply.Origins = a.cfg.ActiveSettings.PackageOverlays.BuildOrigins(reply.Origins)
	}
	return reply, nil
}

func (a *Admin) buildSnapshot(r *http.Request, body []byte) (buildView, int, error) {
	reply, err := a.workerSnapshot(r, body, requireWorker)
	if err != nil {
		return buildView{}, reply.Status, err
	}
	if reply.Build == nil {
		return buildView{}, http.StatusServiceUnavailable, errors.New("worker returned no build status")
	}
	return buildView{BuildStatus: *reply.Build, CanForce: a.cfg.Broker != nil}, http.StatusOK, nil
}

func writeBuildError(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		adminInternal(w, err)
		return
	}
	http.Error(w, err.Error(), status)
}

func (a *Admin) buildStatus(w http.ResponseWriter, r *http.Request, body []byte) {
	view, status, err := a.buildSnapshot(r, body)
	if err != nil {
		writeBuildError(w, status, err)
		return
	}
	adminJSON(w, view)
}

func (a *Admin) buildOrigins(w http.ResponseWriter, r *http.Request, body []byte) {
	reply, err := a.workerSnapshot(r, body, requireWorker)
	if err != nil {
		writeBuildError(w, reply.Status, err)
		return
	}
	adminJSON(w, struct {
		Origins []string `json:"origins"`
	}{reply.Origins})
}

func (a *Admin) queueBuild(w http.ResponseWriter, r *http.Request, body []byte) {
	if a.cfg.Broker == nil {
		http.Error(w, "Start the supervised Poudrine service to request a build", http.StatusServiceUnavailable)
		return
	}
	reply, err := a.workerRequest(r, body)
	if err != nil {
		if reply.Error != "" {
			http.Error(w, reply.Error, reply.Status)
		} else {
			adminInternal(w, err)
		}
		return
	}
	adminJSONStatus(w, http.StatusAccepted, map[string]bool{"queued": true, "already_queued": reply.AlreadyQueued})
}

// Read native diagnostics with the web service's filesystem permissions.
// Only the supervisor can enqueue a retry.
type runQuery struct {
	Started time.Time `json:"started"`
}

func (a *Admin) buildIssues(ctx context.Context, args runQuery) (builder.BuildIssuesView, error) {
	return builder.ReadBuildIssues(ctx, a.cfg.Builder, args.Started)
}

func (a *Admin) buildResults(ctx context.Context, args runQuery) (builder.BuildResultsView, error) {
	return builder.ReadBuildResults(ctx, a.cfg.Builder, args.Started)
}

type logQuery struct {
	Started time.Time `json:"started"`
	Package string    `json:"package"`
	Before  *int64    `json:"before"`
}

func (a *Admin) packageLog(ctx context.Context, args logQuery) (builder.BuildLogPage, error) {
	if args.Started.IsZero() || !packageinfo.ValidIssuePackage(args.Package) || (args.Before != nil && *args.Before < 0) {
		return builder.BuildLogPage{}, invalidQuery("Expected a build start time, package name and optional nonnegative log offset")
	}
	return builder.ReadBuildLog(ctx, a.cfg.Builder, args.Started, args.Package, args.Before)
}

func writeDiagnosticError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "No retained build details or package log for this selection", http.StatusNotFound)
	case errors.Is(err, fs.ErrInvalid):
		http.Error(w, "Invalid build, package or log offset", http.StatusBadRequest)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "The Poudrine service account cannot read these Poudriere build logs", http.StatusServiceUnavailable)
	default:
		adminInternal(w, err)
	}
}

type historyQuery struct {
	Interval string `json:"interval"`
}

func (a *Admin) buildHistory(ctx context.Context, args historyQuery) (database.BuildHistory, error) {
	return a.cfg.DB.BuildHistory(ctx, args.Interval, time.Now())
}

// Each connection starts with the current snapshot, so reconnecting clients
// can recover from missed updates without replaying an event log.
func (a *Admin) buildEvents(w http.ResponseWriter, r *http.Request, body []byte, signer ed25519.PublicKey) {
	if !queryArgs(w, body, &struct{}{}) {
		return
	}
	view, status, err := a.buildSnapshot(r, body)
	if err != nil {
		writeBuildError(w, status, err)
		return
	}
	data, err := json.Marshal(view)
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeBuildEvent(w, data); err != nil {
		return
	}
	previous := data
	lastWrite := time.Now()
	// The worker samples Poudriere's per-job files every two seconds.
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		// The broker checks the pinned key and signed timestamp on each read.
		// Close standalone streams when the enrolled identity changes.
		if a.cfg.Broker == nil {
			key, err := a.enrolledKey(r.Context())
			if err != nil || !signer.Equal(key) {
				return
			}
		}
		view, _, err = a.buildSnapshot(r, body)
		if err != nil {
			return // The browser reconnects with a fresh signed request.
		}
		data, err = json.Marshal(view)
		if err != nil {
			return
		}
		if bytes.Equal(previous, data) {
			if time.Since(lastWrite) < 15*time.Second {
				continue
			}
			data = nil // Keep idle streams and reverse proxies connected.
		} else {
			previous = data
		}
		if err := writeBuildEvent(w, data); err != nil {
			return
		}
		lastWrite = time.Now()
	}
}

func writeBuildEvent(w http.ResponseWriter, data []byte) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err == nil {
		defer controller.SetWriteDeadline(time.Time{})
	} else if !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	var err error
	if data == nil {
		_, err = io.WriteString(w, ": keepalive\n\n")
	} else {
		_, err = fmt.Fprintf(w, "event: build\ndata: %s\n\n", data)
	}
	if err != nil {
		return err
	}
	return controller.Flush()
}
