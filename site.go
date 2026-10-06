// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"compress/gzip"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// routes serves the relay API on relayHost and, when siteHost is set, the
// static website on siteHost with www.siteHost redirected to it. Anything else
// (unknown host, relay paths on the site, site paths on the relay) is a 404.
// Serving both from one program keeps the user's real IP reaching the relay
// directly, with no reverse proxy in between. The public data cache (feeds.go)
// is under /data/ on siteHost.
func routes(relayHost string, relay, health http.Handler, siteHost string, site, data http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(relayHost+"/api/", relay)
	mux.Handle(relayHost+"/health", health)
	if siteHost != "" {
		mux.Handle(siteHost+"/", site)
		if data != nil {
			mux.Handle(siteHost+"/data/", data)
		}
		mux.HandleFunc("www."+siteHost+"/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+siteHost+r.URL.RequestURI(), http.StatusMovedPermanently)
		})
	}
	return mux
}

// dataRoutes is the public data cache plus the Tor check, under /data/.
func dataRoutes(feeds, torCheck http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/data/", feeds)
	mux.Handle("/data/tor-check", torCheck)
	return mux
}

// onionRoutes is what the onion service serves: the website and /data/, any
// Host. Nothing else exists there, the relay above all.
func onionRoutes(site, data http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", site)
	mux.Handle("/data/", data)
	return mux
}

// staticSite serves the files in dir: no directory listings, no dotfiles
// (except /.well-known/security.txt), GET and HEAD only, and no logging of any
// kind. Missing pages get the site's own 404 page (dir/404/index.html), and
// text is gzipped for browsers that accept it. With onionAddress, pages served
// over HTTPS tell Tor Browser about the onion service.
func staticSite(dir, onionAddress string) http.Handler {
	fsys := noListing{http.Dir(dir)}
	files := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Browsers may keep files but must ask again each time (a cheap
		// "not modified" when nothing changed): site updates show at once.
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
			if onionAddress != "" {
				h.Set("Onion-Location", "http://"+onionAddress+r.URL.RequestURI())
			}
		}
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && r.Header.Get("Range") == "" {
			gw := &gzipWriter{ResponseWriter: w}
			defer gw.close()
			w = gw
		}
		h.Add("Vary", "Accept-Encoding")
		switch {
		case r.URL.Path == "/.well-known/security.txt":
			// The one dotfile served: where to report a vulnerability (RFC 9116).
			http.ServeFile(w, r, filepath.Join(dir, ".well-known", "security.txt"))
		case !exists(fsys, r.URL.Path):
			notFound(w, dir)
		default:
			files.ServeHTTP(w, r)
		}
	})
}

// exists tells whether the file server would find something at urlPath.
func exists(fsys http.FileSystem, urlPath string) bool {
	f, err := fsys.Open(path.Clean("/" + urlPath))
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// notFound answers 404 with the site's own page, or a plain one without it.
func notFound(w http.ResponseWriter, dir string) {
	page, err := os.ReadFile(filepath.Join(dir, "404", "index.html"))
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write(page)
}

// gzipWriter compresses text responses (pages, style sheets, feeds, text files)
// on the way out. Images and archives are sent as they are.
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.decided {
		g.decided = true
		h := g.Header()
		if (code == http.StatusOK || code == http.StatusNotFound) && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
			h.Del("Content-Length")
			h.Set("Content-Encoding", "gzip")
			g.gz = gzip.NewWriter(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
	}
}

func compressible(contentType string) bool {
	for _, t := range []string{"text/", "application/xml", "application/json", "application/atom+xml", "image/svg+xml"} {
		if strings.HasPrefix(contentType, t) {
			return true
		}
	}
	return false
}

// noListing hides dotfiles and directories that have no index.html.
type noListing struct{ fs http.FileSystem }

func (n noListing) Open(name string) (http.File, error) {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return nil, fs.ErrNotExist
		}
	}
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		index, err := n.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			if errors.Is(err, os.ErrNotExist) {
				return nil, fs.ErrNotExist
			}
			return nil, err
		}
		index.Close()
	}
	return f, nil
}
