// Package webtheme embeds the shared visual styles for both web servers.
package webtheme

import (
	_ "embed"
	"net/http"
	"strings"
	"time"
)

//go:embed theme.css
var CSS string

//go:embed fonts/sora-latin.woff2
var soraLatin string

//go:embed fonts/sora-latin-ext.woff2
var soraLatinExt string

//go:embed fonts/atkinson-hyperlegible-next.woff2
var atkinson string

//go:embed fonts/geist-mono.woff2
var geistMono string

// ServeFont serves both sites' typefaces from the executable, including in Capsicum.
func ServeFont(w http.ResponseWriter, r *http.Request) {
	var font string
	switch r.URL.Path {
	case "/fonts/sora-latin.woff2":
		font = soraLatin
	case "/fonts/sora-latin-ext.woff2":
		font = soraLatinExt
	case "/fonts/atkinson-hyperlegible-next.woff2", "/fonts/geist-sans.woff2":
		// Older stylesheets still reference these paths.
		font = atkinson
	case "/fonts/geist-mono.woff2":
		font = geistMono
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", "font/woff2")
	http.ServeContent(w, r, r.URL.Path, time.Time{}, strings.NewReader(font))
}
