package status

import (
	"bytes"
	"html/template"
	"strconv"

	"github.com/inkeliz/poudrine/internal/statusproto"
	statusweb "github.com/inkeliz/poudrine/internal/web/poudrine-status"
)

var statusPage = template.Must(template.New("status").Parse(statusweb.HTML))

func displayCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

func renderStatusPage(s statusproto.Counts, available bool) ([]byte, error) {
	data := struct {
		Available       bool
		Packages, Ahead string
	}{Available: available}
	if available {
		data.Packages = displayCount(s.Packages)
		data.Ahead = displayCount(s.Ahead)
	}
	var buf bytes.Buffer
	err := statusPage.Execute(&buf, data)
	return buf.Bytes(), err
}
