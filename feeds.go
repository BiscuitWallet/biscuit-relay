// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The public data the app shows (prices, fiat rates, Monero crowdfunding),
// fetched here and served from the website, so the sources only ever see this
// server: the app talks to one host, like Feather with its own service.
// Responses are passed on unchanged; nothing about the requests is logged.

// feedCoins is the coin list the app asked CoinGecko for.
var feedCoins = []string{
	"monero", "bitcoin", "litecoin", "ethereum", "tether", "usd-coin", "bitcoin-cash",
	"dash", "zcash", "dogecoin", "solana", "ripple", "cardano", "tron", "wownero",
}

func coinGeckoURL() string {
	q := url.Values{}
	q.Set("vs_currency", "usd")
	q.Set("ids", strings.Join(feedCoins, ","))
	q.Set("price_change_percentage", "24h")
	return "https://api.coingecko.com/api/v3/coins/markets?" + q.Encode()
}

// Feed is one upstream document, refreshed on its own schedule.
type Feed struct {
	Name     string        // served as /data/<Name>.json
	URL      string        // upstream
	Every    time.Duration // refresh interval
	WantList bool          // the document must be a JSON array

	mu      sync.RWMutex
	body    []byte
	fetched time.Time
}

// DefaultFeeds are the documents the app uses.
func DefaultFeeds() []*Feed {
	return []*Feed{
		{Name: "crypto", URL: coinGeckoURL(), Every: time.Minute, WantList: true},
		{Name: "fiat", URL: "https://api.frankfurter.dev/v1/latest?base=USD", Every: 30 * time.Minute},
		{Name: "ccs", URL: "https://ccs.getmonero.org/index.php/projects", Every: 15 * time.Minute},
	}
}

const feedMaxBytes = 4 << 20

// Refresh fetches the upstream document and keeps it if it is valid JSON of
// the expected shape; on any error the previous copy stays.
func (f *Feed) Refresh(ctx context.Context, client *http.Client, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "biscuitwallet.com")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", f.Name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, feedMaxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > feedMaxBytes {
		return fmt.Errorf("%s: response too large", f.Name)
	}
	body = bytes.TrimSpace(body)
	if !json.Valid(body) {
		return fmt.Errorf("%s: not JSON", f.Name)
	}
	if f.WantList && (len(body) < 2 || body[0] != '[' || string(body) == "[]") {
		return errors.New(f.Name + ": expected a non-empty list")
	}
	f.mu.Lock()
	f.body, f.fetched = body, now
	f.mu.Unlock()
	return nil
}

func (f *Feed) snapshot() ([]byte, time.Time) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.body, f.fetched
}

// Run refreshes the feed forever: every minute until the first success, then
// on its own interval.
func (f *Feed) Run(client *http.Client) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := f.Refresh(ctx, client, time.Now())
		cancel()
		if err != nil {
			log.Printf("feed %v", err)
		}
		if _, fetched := f.snapshot(); fetched.IsZero() {
			time.Sleep(time.Minute)
		} else {
			time.Sleep(f.Every)
		}
	}
}

// feedsHandler serves /data/<name>.json: GET and HEAD only, no logging.
func feedsHandler(feeds []*Feed) http.Handler {
	byPath := map[string]*Feed{}
	for _, f := range feeds {
		byPath["/data/"+f.Name+".json"] = f
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f, ok := byPath[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, fetched := f.snapshot()
		if body == nil {
			http.Error(w, "not available yet", http.StatusServiceUnavailable)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "public, max-age=60")
		h.Set("Last-Modified", fetched.UTC().Format(http.TimeFormat))
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.Method == http.MethodGet {
			w.Write(body)
		}
	})
}
