package admin

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/public"
)

func (a *Admin) index(w http.ResponseWriter, r *http.Request) {
	files, err := public.MakeClientInstructions(a.cfg.RepositoryURL, a.cfg.PublicKey())
	if err != nil {
		adminInternal(w, err)
		return
	}
	var page bytes.Buffer
	err = public.RepositoryPage.Execute(&page, struct {
		public.ClientInstructions
		Stats catalog.CatalogStats
	}{files, a.cfg.Stats()})
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
