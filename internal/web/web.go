// Package web holds the templates and static assets embedded in the binary.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/url"
	"sync"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var (
	tmplMu    sync.Mutex
	tmplCache = map[string]*template.Template{}
)

// TemplatesFor returns the template set with asset URLs stamped with version.
//
// The stamp is what makes a static file safe to cache forever: a new binary asks
// for a new URL, so no browser can pair a fresh page with a stale stylesheet.
// Parsing is cheap but not free, so the sets are cached per version — tests build
// hundreds of apps.
func TemplatesFor(version string) *template.Template {
	tmplMu.Lock()
	defer tmplMu.Unlock()
	if t, ok := tmplCache[version]; ok {
		return t
	}
	asset := func(name string) string {
		if version == "" {
			return "/static/" + name
		}
		return "/static/" + name + "?v=" + url.QueryEscape(version)
	}
	t := template.Must(template.New("dumpster").
		Funcs(template.FuncMap{"asset": asset}).
		ParseFS(templateFS, "templates/*.html"))
	tmplCache[version] = t
	return t
}

// Templates is the unversioned set, for tests and anything that has no build
// stamp to offer.
var Templates = TemplatesFor("")

// Static is the root of /static/.
var Static = mustSub(staticFS, "static")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
