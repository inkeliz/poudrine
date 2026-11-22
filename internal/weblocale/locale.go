// Package weblocale selects the locale used to format values in the web UI.
package weblocale

import (
	"net/http"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// ForRequest returns the highest-weight language with formatting data.
func ForRequest(r *http.Request) language.Tag {
	best := language.English
	var weight float32
	for _, field := range r.Header.Values("Accept-Language") {
		for value := range strings.SplitSeq(field, ",") {
			tags, weights, err := language.ParseAcceptLanguage(value)
			if err != nil || len(tags) == 0 || !(weights[0] > weight && weights[0] <= 1) {
				continue
			}
			// Index zero is the CLDR root, including wildcards and private-use tags.
			if index, _ := language.CompactIndex(tags[0]); index == 0 {
				continue
			}
			best, weight = tags[0], weights[0]
			if weight == 1 {
				return best
			}
		}
	}
	return best
}

type Page struct {
	Tag language.Tag
	*message.Printer
}

func ForPage(w http.ResponseWriter, r *http.Request) Page {
	tag := ForRequest(r)
	w.Header().Add("Vary", "Accept-Language")
	w.Header().Set("Content-Language", tag.String())
	return Page{Tag: tag, Printer: message.NewPrinter(tag)}
}
