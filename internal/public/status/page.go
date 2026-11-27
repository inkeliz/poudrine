package status

import (
	"bytes"
	"html/template"

	"github.com/inkeliz/poudrine/internal/statusproto"
	webtheme "github.com/inkeliz/poudrine/internal/web"
	statusweb "github.com/inkeliz/poudrine/internal/web/poudrine-status"
	"github.com/inkeliz/poudrine/internal/weblocale"
)

// Inline the embedded theme so the confined reader can serve a complete page.
var theme = template.Must(template.New("theme").Parse(webtheme.CSS))
var statusPage = template.Must(theme.New("status").Parse(statusweb.HTML))

func renderStatusPage(locale weblocale.Page, s statusproto.Counts, available bool) ([]byte, error) {
	data := struct {
		Locale          weblocale.Page
		Available       bool
		LocalBuildCount int64
		statusproto.Counts
	}{Locale: locale, Available: available, Counts: s}
	if s.LocalBuilds != nil {
		data.LocalBuildCount = *s.LocalBuilds
	}
	var buf bytes.Buffer
	err := statusPage.Execute(&buf, data)
	return buf.Bytes(), err
}
