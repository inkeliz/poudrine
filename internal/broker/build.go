package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// Only the supervisor has access to the active worker. Check the original
// signature and reserve its nonce there before changing the run queue.
func (a *Authority) handleBuild(m Message) Reply {
	if a.Builder == nil {
		return Reply{Status: 503, Error: "Build worker unavailable"}
	}
	if m.Method == MethodQuery {
		status := a.Builder.Status()
		return Reply{Status: 200, Build: &status}
	}
	if len(m.Body) != 0 && !ValidQuery(m.Body, &struct{}{}) {
		return Reply{Status: 400, Error: "A build request takes no arguments"}
	}
	if reply := a.recordNonce(m); reply.Error != "" {
		return reply
	}
	return Reply{Status: http.StatusAccepted, AlreadyQueued: !a.Builder.RequestBuild()}
}

// The web process forwards the original signed request. Only the supervisor
// selects origins for the next bulk, using its own jail and tree's records.
func (a *Authority) handleBuildRetry(ctx context.Context, m Message) Reply {
	var args struct {
		Started time.Time       `json:"started"`
		Package json.RawMessage `json:"package"`
	}
	var pkg string
	if !ValidQuery(m.Body, &args) || args.Started.IsZero() || args.Started.After(time.Now()) ||
		(args.Package != nil && (json.Unmarshal(args.Package, &pkg) != nil || !packageinfo.ValidIssuePackage(pkg))) {
		return Reply{Status: http.StatusBadRequest, Error: "Expected a build start time and an optional package name"}
	}
	if a.Builder == nil {
		return Reply{Status: http.StatusServiceUnavailable, Error: "Build worker unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	failed, skipped, err := builder.ReadRunIssues(ctx, a.Builder.Cfg, args.Started)
	if errors.Is(err, fs.ErrNotExist) {
		return Reply{Status: http.StatusNotFound, Error: "This Poudriere run is no longer available"}
	}
	if err != nil {
		log.Printf("reading Poudriere results for retry: %v", err)
		return Reply{Status: http.StatusServiceUnavailable, Error: "Cannot read Poudriere results for this run"}
	}
	origins := affectedOrigins(failed, skipped, pkg)
	if len(origins) == 0 {
		if pkg != "" {
			return Reply{Status: http.StatusNotFound, Error: "This package has no failure or skip in that run"}
		}
		return Reply{Status: http.StatusConflict, Error: "This run has no failed or skipped packages to retry"}
	}
	if reply := a.recordNonce(m); reply.Error != "" {
		return reply
	}
	added := a.Builder.QueueRetry(origins)
	return Reply{Status: http.StatusAccepted, AlreadyQueued: !added}
}

// Poudriere records an edge from each skipped package to the package that
// blocked it. If several failures share dependents, retry the connected group
// together so the same dependency cannot immediately block the selected port.
func affectedOrigins(failed, skipped []builder.BuildIssue, pkg string) []string {
	originOf := make(map[string]string, len(failed)+len(skipped))
	related := make(map[string][]string)
	for _, item := range failed {
		originOf[item.Package] = item.Origin
	}
	for _, item := range skipped {
		originOf[item.Package] = item.Origin
		if item.BlockedBy != "" {
			related[item.Package] = append(related[item.Package], item.BlockedBy)
			related[item.BlockedBy] = append(related[item.BlockedBy], item.Package)
		}
	}
	var origins []string
	if pkg == "" {
		origins = slices.Collect(maps.Values(originOf))
	} else if _, ok := originOf[pkg]; ok {
		seen := map[string]bool{pkg: true}
		queue := []string{pkg}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if origin := originOf[current]; origin != "" {
				origins = append(origins, origin)
			}
			for _, adjacent := range related[current] {
				if !seen[adjacent] {
					seen[adjacent] = true
					queue = append(queue, adjacent)
				}
			}
		}
	}
	slices.Sort(origins)
	return slices.Compact(origins)
}
