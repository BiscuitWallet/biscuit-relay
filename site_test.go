// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSite(t *testing.T) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "home")
	write("blog/index.html", "blog")
	write("downloads/biscuit.dmg", "dmg")
	write(".git/config", "secret")

	stub := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
	}
	return routes("relay.example.org", stub("api"), stub("ok"), "example.org", staticSite(dir, ""), nil)
}

func get(mux *http.ServeMux, method, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
	return rec
}

func TestSiteAndRelayAreSeparatedByHost(t *testing.T) {
	mux := newTestSite(t)
	cases := []struct {
		url  string
		code int
		body string
	}{
		{"https://example.org/", 200, "home"},
		{"https://example.org/blog/", 200, "blog"},
		{"https://example.org/downloads/biscuit.dmg", 200, "dmg"},
		{"https://relay.example.org/api/coins", 200, "api"},
		{"https://relay.example.org/health", 200, "ok"},
		{"https://example.org/api/coins", 404, ""},
		{"https://relay.example.org/", 404, ""},
		{"https://unknown.example.net/", 404, ""},
	}
	for _, c := range cases {
		rec := get(mux, http.MethodGet, c.url)
		if rec.Code != c.code || (c.body != "" && rec.Body.String() != c.body) {
			t.Errorf("%s: got %d %q, want %d %q", c.url, rec.Code, rec.Body.String(), c.code, c.body)
		}
	}
}

func TestWwwRedirectsToApex(t *testing.T) {
	rec := get(newTestSite(t), http.MethodGet, "https://www.example.org/blog/?a=1")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://example.org/blog/?a=1" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSiteHidesListingsAndDotfiles(t *testing.T) {
	mux := newTestSite(t)
	for _, url := range []string{"https://example.org/downloads/", "https://example.org/.git/config", "https://example.org/.git/"} {
		if rec := get(mux, http.MethodGet, url); rec.Code != 404 {
			t.Errorf("%s: got %d, want 404", url, rec.Code)
		}
	}
}

func TestSiteIsReadOnlyWithSecurityHeaders(t *testing.T) {
	mux := newTestSite(t)
	if rec := get(mux, http.MethodPost, "https://example.org/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d", rec.Code)
	}
	rec := get(mux, http.MethodGet, "https://example.org/")
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "Strict-Transport-Security"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
}

func TestFeeds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list":
			io.WriteString(w, `[{"symbol":"xmr","current_price":300}]`)
		case "/empty":
			io.WriteString(w, `[]`)
		case "/html":
			io.WriteString(w, `<html>`)
		default:
			http.Error(w, "down", http.StatusBadGateway)
		}
	}))
	defer upstream.Close()

	crypto := &Feed{Name: "crypto", URL: upstream.URL + "/list", WantList: true}
	fiat := &Feed{Name: "fiat", URL: upstream.URL + "/down"}
	mux := routes("relay.example.org", http.NotFoundHandler(), http.NotFoundHandler(), "example.org", http.NotFoundHandler(), feedsHandler([]*Feed{crypto, fiat}))

	if rec := get(mux, "GET", "https://example.org/data/crypto.json"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before the first fetch: %d", rec.Code)
	}
	ctx := context.Background()
	if err := crypto.Refresh(ctx, upstream.Client(), time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := get(mux, "GET", "https://example.org/data/crypto.json")
	if rec.Code != 200 || rec.Body.String() != `[{"symbol":"xmr","current_price":300}]` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("crypto: %d %q", rec.Code, rec.Body.String())
	}
	// Bad upstream answers keep the previous copy.
	for _, path := range []string{"/empty", "/html", "/down"} {
		crypto.URL = upstream.URL + path
		if err := crypto.Refresh(ctx, upstream.Client(), time.Now()); err == nil {
			t.Fatalf("%s: accepted", path)
		}
	}
	if rec := get(mux, "GET", "https://example.org/data/crypto.json"); rec.Body.String() != `[{"symbol":"xmr","current_price":300}]` {
		t.Fatalf("previous copy lost: %q", rec.Body.String())
	}
	if err := fiat.Refresh(ctx, upstream.Client(), time.Now()); err == nil {
		t.Fatal("fiat: HTTP error accepted")
	}
	if rec := get(mux, "POST", "https://example.org/data/crypto.json"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
	if rec := get(mux, "GET", "https://example.org/data/other.json"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown feed: %d", rec.Code)
	}
	if rec := get(mux, "GET", "https://relay.example.org/data/crypto.json"); rec.Code != http.StatusNotFound {
		t.Fatalf("feeds on the relay host: %d", rec.Code)
	}
}

func TestOnionServesNoRelay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := dataRoutes(http.NotFoundHandler(), torCheckHandler(testTorExits(), true))
	onion := onionRoutes(staticSite(dir, ""), data)
	for _, target := range []string{
		"http://abc.onion/api/new_trade?id=R1",
		"http://abc.onion/api/coins",
		"http://relay.example.org/api/new_trade?id=R1",
		"http://abc.onion/health",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = "127.0.0.1:5555"
		onion.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on the onion: %d %q", target, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	onion.ServeHTTP(rec, httptest.NewRequest("GET", "http://abc.onion/", nil))
	if rec.Code != 200 || rec.Body.String() != "home" {
		t.Errorf("site on the onion: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	onion.ServeHTTP(rec, httptest.NewRequest("GET", "http://abc.onion/data/tor-check", nil))
	if rec.Body.String() != "{\"tor\":\"yes\"}\n" {
		t.Errorf("tor-check on the onion: %q", rec.Body.String())
	}
}

func TestOnionLocationOnlyOverHTTPS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := staticSite(dir, "abc.onion")
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "https://example.org/download/", nil))
	if got := rec.Header().Get("Onion-Location"); got != "http://abc.onion/download/" {
		t.Errorf("Onion-Location = %q", got)
	}
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "http://abc.onion/", nil))
	if rec.Header().Get("Onion-Location") != "" {
		t.Error("Onion-Location sent on the onion itself")
	}
}

func newPlainSite(t *testing.T, files map[string]string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return staticSite(dir, "")
}

func TestSiteNotFoundPage(t *testing.T) {
	site := newPlainSite(t, map[string]string{"index.html": "home", "404/index.html": "lost bear", ".git/config": "secret"})
	for _, url := range []string{"/nope/", "/missing.png", "/.git/config"} {
		rec := httptest.NewRecorder()
		site.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 404 || rec.Body.String() != "lost bear" {
			t.Errorf("%s: %d %q, want the 404 page", url, rec.Code, rec.Body.String())
		}
	}
}

func TestSiteSecurityTxt(t *testing.T) {
	site := newPlainSite(t, map[string]string{"index.html": "home", ".well-known/security.txt": "Contact: mailto:x@example.org", ".well-known/other": "no"})
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/security.txt", nil))
	if rec.Code != 200 || rec.Body.String() != "Contact: mailto:x@example.org" {
		t.Errorf("security.txt: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/other", nil))
	if rec.Code != 404 {
		t.Errorf("other dotfile: %d, want 404", rec.Code)
	}
}

func TestSiteGzip(t *testing.T) {
	page := strings.Repeat("<p>Biscuit</p>", 200)
	site := newPlainSite(t, map[string]string{"index.html": page, "img.png": "\x89PNG"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("page not gzipped: %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != page {
		t.Error("gzipped page differs")
	}

	req = httptest.NewRequest("GET", "/img.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "\x89PNG" {
		t.Error("image should be sent as it is")
	}

	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != page {
		t.Error("no gzip without Accept-Encoding")
	}
}
