// Package web embeds the built Vue SPA (web/dist) into the backend binary, so
// the API and the UI ship as one artifact and cannot drift apart.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA rooted at dist/. It errors if the SPA was never
// built (dist/ empty), so the server can say so rather than serve nothing.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := sub.Open("index.html"); err != nil {
		return nil, err
	}
	return sub, nil
}
