// Package poudrineweb embeds the private repository website and its client setup template.
package poudrineweb

import (
	"embed"

	webtheme "github.com/inkeliz/poudrine/internal/web"
)

//go:embed admin.html index.html poudrine.js setup-client.sh.tmpl report-inventory.sh.tmpl
var Files embed.FS

//go:embed style.css
var pageCSS string

// Stylesheet is served in one request on both private pages.
var Stylesheet = webtheme.CSS + "\n" + pageCSS
