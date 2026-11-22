// Package poudrineweb embeds the private repository website and its client setup template.
package poudrineweb

import "embed"

//go:embed admin.html index.html style.css poudrine.js setup-client.sh.tmpl report-inventory.sh.tmpl
var Files embed.FS
