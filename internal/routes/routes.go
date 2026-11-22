// Package routes defines the method and path signed for each Poudrine API operation.
package routes

import (
	"net/http"

	"github.com/inkeliz/poudrine/internal/requestauth"
)

type Route struct {
	Method, Path string
}

func (r Route) Pattern() string { return r.Method + " " + r.Path }

func (r Route) Matches(method, path string) bool { return r.Method == method && r.Path == path }

const (
	setupPath        = "/api/setup"
	configPath       = "/api/admin/config"
	BuildStatusPath  = "/api/admin/build"
	RetryBuildPath   = BuildStatusPath + "/retry"
	subscriptionPath = "/api/admin/subscription"
)

var (
	Status          = Route{requestauth.Query, "/api/status"}
	Setup           = Route{requestauth.Query, setupPath}
	Enroll          = Route{http.MethodPost, setupPath}
	Login           = Route{http.MethodPost, "/api/admin/login"}
	ReadConfig      = Route{requestauth.Query, configPath}
	SaveConfig      = Route{http.MethodPut, configPath}
	BuildStatus     = Route{requestauth.Query, BuildStatusPath}
	BuildEvents     = Route{requestauth.Query, BuildStatusPath + "/events"}
	BuildIssues     = Route{requestauth.Query, BuildStatusPath + "/issues"}
	BuildResults    = Route{requestauth.Query, BuildStatusPath + "/results"}
	BuildLog        = Route{requestauth.Query, BuildStatusPath + "/log"}
	BuildHistory    = Route{requestauth.Query, BuildStatusPath + "/history"}
	RetryBuild      = Route{http.MethodPost, RetryBuildPath}
	ForceBuild      = Route{http.MethodPost, BuildStatusPath + "/force"}
	Origins         = Route{requestauth.Query, "/api/admin/origins"}
	Packages        = Route{requestauth.Query, "/api/admin/packages"}
	Clients         = Route{requestauth.Query, "/api/admin/clients"}
	ClientSummary   = Route{requestauth.Query, "/api/admin/clients/summary"}
	ClientPackages  = Route{requestauth.Query, "/api/admin/clients/packages"}
	Tracked         = Route{requestauth.Query, "/api/admin/tracked"}
	PushKey         = Route{requestauth.Query, "/api/admin/push-key"}
	Subscribe       = Route{http.MethodPost, subscriptionPath}
	Unsubscribe     = Route{http.MethodDelete, subscriptionPath}
	ReportInventory = Route{http.MethodPost, "/api/inventory"}
)
