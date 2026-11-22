package poudrine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"time"
)

const buildStatusPath = "/api/admin/build"
const buildEventsPath = buildStatusPath + "/events"
const forceBuildPath = buildStatusPath + "/force"
const originsPath = "/api/admin/origins"

type buildView struct {
	BuildStatus
	CanForce bool `json:"can_force"`
}

// Only the supervisor has access to the active worker. Check the original
// signature and reserve its nonce there before changing the run queue.
func (a *configAuthority) handleBuild(m configMessage) configReply {
	if a.builder == nil {
		return configReply{Status: 503, Error: "Build worker unavailable"}
	}
	if m.Method == methodQuery {
		status := a.builder.Status()
		return configReply{Status: 200, Build: &status}
	}
	if len(m.Body) != 0 {
		var args map[string]json.RawMessage
		if json.Unmarshal(m.Body, &args) != nil || args == nil || len(args) != 0 {
			return configReply{Status: 400, Error: "A build request takes no arguments"}
		}
	}
	if reply := a.recordNonce(m); reply.Error != "" {
		return reply
	}
	return configReply{Status: http.StatusAccepted, AlreadyQueued: !a.builder.RequestBuild()}
}

func (a *Admin) workerRequest(w http.ResponseWriter, r *http.Request, body []byte) (configReply, bool) {
	reply, err := a.cfg.broker.exchange(forwardedConfigRequest(r, body))
	if err != nil {
		adminInternal(w, err)
		return configReply{}, false
	}
	if reply.Error != "" {
		http.Error(w, reply.Error, reply.Status)
		return configReply{}, false
	}
	return reply, true
}

func (a *Admin) buildSnapshot(r *http.Request, body []byte) (buildView, int, error) {
	if a.cfg.broker == nil {
		// Standalone serve can display an independent worker's status file.
		status, err := a.cfg.Build()
		if err != nil {
			return buildView{}, http.StatusServiceUnavailable, err
		}
		return buildView{BuildStatus: status}, http.StatusOK, nil
	}
	reply, err := a.cfg.broker.exchange(forwardedConfigRequest(r, body))
	if err != nil {
		return buildView{}, http.StatusServiceUnavailable, err
	}
	if reply.Error != "" {
		return buildView{}, reply.Status, errors.New(reply.Error)
	}
	if reply.Build == nil {
		return buildView{}, http.StatusServiceUnavailable, errors.New("worker returned no build status")
	}
	return buildView{BuildStatus: *reply.Build, CanForce: true}, http.StatusOK, nil
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

// Return nil when the source is unavailable. A readable, empty file produces
// an empty JSON array. Never expose comments or the contents of invalid lines.
func configuredOrigins(path string) []string {
	file, err := readPackagesFile(path)
	if err != nil {
		return nil
	}
	origins, err := buildOrigins(file, nil)
	if err != nil {
		return nil
	}
	return origins
}

func (a *Admin) buildOrigins(w http.ResponseWriter, r *http.Request, body []byte) {
	var origins []string
	if a.cfg.broker == nil {
		origins = configuredOrigins(a.cfg.ActiveSettings.PackagesFile)
	} else {
		reply, ok := a.workerRequest(w, r, body)
		if !ok {
			return
		}
		origins = reply.Origins
	}
	origins = a.cfg.ActiveSettings.PackageOverlays.buildOrigins(origins)
	adminJSON(w, struct {
		Origins []string `json:"origins"`
	}{origins})
}

func (a *Admin) queueBuild(w http.ResponseWriter, r *http.Request, body []byte) {
	if a.cfg.broker == nil {
		http.Error(w, "Start the supervised Poudrine service to request a build", http.StatusServiceUnavailable)
		return
	}
	if reply, ok := a.workerRequest(w, r, body); ok {
		adminJSONStatus(w, http.StatusAccepted, map[string]bool{"queued": true, "already_queued": reply.AlreadyQueued})
	}
}

// Read native diagnostics with the web service's filesystem permissions.
// Only the supervisor can enqueue a retry.
func (a *Admin) buildIssues(w http.ResponseWriter, r *http.Request, body []byte) {
	var args struct {
		Started time.Time `json:"started"`
	}
	if !queryArgs(w, body, &args) {
		return
	}
	view, err := readBuildIssues(r.Context(), a.cfg.builder, args.Started)
	if err != nil {
		writeDiagnosticError(w, err)
		return
	}
	adminJSON(w, view)
}

func (a *Admin) buildResults(w http.ResponseWriter, r *http.Request, body []byte) {
	var args struct {
		Started time.Time `json:"started"`
	}
	if !queryArgs(w, body, &args) {
		return
	}
	view, err := readBuildResults(r.Context(), a.cfg.builder, args.Started)
	if err != nil {
		writeDiagnosticError(w, err)
		return
	}
	adminJSON(w, view)
}

func (a *Admin) packageLog(w http.ResponseWriter, r *http.Request, body []byte) {
	var args struct {
		Started time.Time `json:"started"`
		Package string    `json:"package"`
		Before  *int64    `json:"before"`
	}
	if !queryArgs(w, body, &args) {
		return
	}
	if args.Started.IsZero() || len(args.Package) > 255 || !jobPackage.MatchString(args.Package) || (args.Before != nil && *args.Before < 0) {
		http.Error(w, "Expected a build start time, package name and optional nonnegative log offset", http.StatusBadRequest)
		return
	}
	page, err := readBuildLog(r.Context(), a.cfg.builder, args.Started, args.Package, args.Before)
	if err != nil {
		writeDiagnosticError(w, err)
		return
	}
	adminJSON(w, page)
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

func (a *Admin) buildHistory(w http.ResponseWriter, r *http.Request, body []byte) {
	var args struct {
		Interval string `json:"interval"`
	}
	if !queryArgs(w, body, &args) {
		return
	}
	history, err := (inventory{a.cfg.DB}).buildHistory(r.Context(), args.Interval, time.Now())
	if errors.Is(err, ErrBuildInterval) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		adminInternal(w, err)
		return
	}
	adminJSON(w, history)
}
