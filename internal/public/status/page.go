package status

import (
	"bytes"
	"html/template"

	"github.com/inkeliz/poudrine/internal/statusproto"
	statusweb "github.com/inkeliz/poudrine/internal/web/poudrine-status"
	"github.com/inkeliz/poudrine/internal/weblocale"
)

var statusPage = template.Must(template.New("status").Parse(statusweb.HTML))

func renderStatusPage(locale weblocale.Page, s statusproto.Counts, available bool) ([]byte, error) {
	data := struct {
		Locale    weblocale.Page
		Available bool
		statusproto.Counts
	}{locale, available, s}
	var buf bytes.Buffer
	err := statusPage.Execute(&buf, data)
	return buf.Bytes(), err
}
