// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	return routes("relay.example.org", stub("api"), stub("ok"), "example.org", staticSite(dir), nil)
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
