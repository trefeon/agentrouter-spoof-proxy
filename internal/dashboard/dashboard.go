// Package dashboard embeds the admin web UI served at the proxy root.
package dashboard

import (
	_ "embed"
)

//go:embed static/index.html
var indexHTML string

// HTML returns the embedded dashboard page.
func HTML() string {
	return indexHTML
}
