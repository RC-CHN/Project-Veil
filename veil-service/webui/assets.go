// Package webui contains shared connection management assets for platform hosts.
package webui

import (
	"embed"
	"net/http"
)

//go:embed connections.js connections.css i18n.js
var assets embed.FS

// Handler serves only the embedded UI assets, under /shared/ in desktop hosts.
func Handler() http.Handler { return http.StripPrefix("/shared/", http.FileServer(http.FS(assets))) }
