// Package statusweb embeds the public package-count page.
package statusweb

import _ "embed"

//go:embed status.html
var HTML string
