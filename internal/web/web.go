// Package web holds the templates and static assets embedded in the binary.
package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Templates is the parsed template set. A malformed template panics at startup,
// which is the only place a panic is allowed.
var Templates = template.Must(template.New("dumpster").ParseFS(templateFS, "templates/*.html"))

// Static is the root of /static/.
var Static = mustSub(staticFS, "static")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
