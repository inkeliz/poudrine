package admin

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/public"
	"github.com/inkeliz/poudrine/internal/weblocale"
)

func (a *Admin) index(w http.ResponseWriter, r *http.Request) {
	files, err := public.MakeClientInstructions(a.cfg.RepositoryURL, a.cfg.PublicKey())
	if err != nil {
		adminInternal(w, err)
		return
	}
	var page bytes.Buffer
	stats := a.cfg.Stats()
	var published string
	if stats.Updated != nil {
		published = stats.Updated.UTC().Format(time.RFC3339)
	}
	err = public.RepositoryPage.Execute(&page, struct {
		public.ClientInstructions
		Locale    weblocale.Page
		Stats     catalog.CatalogStats
		Published string
	}{files, weblocale.ForPage(w, r), stats, published})
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = page.WriteTo(w)
}

func (a *Admin) clientSetup(w http.ResponseWriter, r *http.Request) {
	files, err := public.MakeClientInstructions(a.cfg.RepositoryURL, a.cfg.PublicKey())
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="setup-client.sh"`)
	// Keep the catalog refresh for clients using the original download.
	fmt.Fprint(w, "#!/bin/sh\nset -e\n", files.SetupCommand, "pkg update -f -r Poudrine\n")
}
