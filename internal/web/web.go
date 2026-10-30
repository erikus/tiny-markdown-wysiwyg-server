// Package web embeds the built frontend assets (internal/web/static) so the
// binary is self-contained.
package web

import (
	"embed"
	"html/template"
	"io/fs"
)

const (
	// staticDir is the embedded directory holding the built assets.
	staticDir = "static"
	// indexFile is the editor page inside staticDir.
	indexFile = staticDir + "/index.html"
)

// Static holds the built frontend: index.html, app.js, app.css and the
// third-party license file. Paths inside are prefixed with "static/".
//
//go:embed static/*
var Static embed.FS

// IndexTemplate is index.html parsed as an html/template. The only dynamic
// value is .ViewBase, which the page exposes as
// <meta name="mdwebd-view-base" content="{{.ViewBase}}">.
var IndexTemplate *template.Template = template.Must(template.ParseFS(Static, indexFile))

// IndexData is the data passed to IndexTemplate.
type IndexData struct {
	ViewBase string
}

// Assets returns Static rooted at the static directory, so that "app.js"
// resolves to static/app.js. Suitable for http.FileServerFS.
func Assets() (fs.FS, error) {
	return fs.Sub(Static, staticDir)
}
