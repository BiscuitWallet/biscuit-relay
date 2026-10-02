// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tor exit addresses come from two Tor Project sources, both required: the
// bulk list measures the address each exit really uses, Onionoo adds the
// relays' own addresses (new exits, IPv6).
const (
	TorBulkExitList = "https://check.torproject.org/torbulkexitlist"
	OnionooExits    = "https://onionoo.torproject.org/details?flag=Exit&running=true&fields=or_addresses,exit_addresses"
)

type torStatus int

const (
	torListMissing torStatus = iota // a list never loaded or too old: trade creation fails closed
	torExit
	notTorExit
)

// ExitList is one source of exit addresses. A failed refresh keeps the
// previous addresses, but only until they are older than TorExits.MaxAge.
type ExitList struct {
	URL   string
	Parse func(io.Reader) ([]string, error)

	mu      sync.RWMutex
	keys    map[string]bool
	updated time.Time
}

// TorExits answers whether an address is a Tor exit, from all its lists.
type TorExits struct {
	Lists  []*ExitList
	Client *http.Client
	MaxAge time.Duration
	Now    func() time.Time
}

func NewTorExits(client *http.Client) *TorExits {
	return &TorExits{
		Lists: []*ExitList{
			{URL: TorBulkExitList, Parse: parseBulkList},
			{URL: OnionooExits, Parse: parseOnionoo},
		},
		Client: client,
		MaxAge: 24 * time.Hour,
		Now:    time.Now,
	}
}

func (t *TorExits) Contains(ip string) torStatus {
	key := addressKey(ip)
	if key == "" {
		return torListMissing
	}
	now := t.Now()
	found := false
	for _, l := range t.Lists {
		l.mu.RLock()
		fresh := !l.updated.IsZero() && now.Sub(l.updated) <= t.MaxAge
		hit := l.keys[key]
		l.mu.RUnlock()
		if !fresh {
			return torListMissing
		}
		found = found || hit
	}
	if found {
		return torExit
	}
	return notTorExit
}

// Refresh reloads every list; each one that fails keeps its previous state.
func (t *TorExits) Refresh(ctx context.Context) error {
	var errs []error
	for _, l := range t.Lists {
		if err := t.refresh(ctx, l); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *TorExits) refresh(ctx context.Context, l *ExitList) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return err
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", l.URL, resp.StatusCode)
	}
	addrs, err := l.Parse(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", l.URL, err)
	}
	keys := map[string]bool{}
	for _, a := range addrs {
		if k := addressKey(a); k != "" {
			keys[k] = true
		}
	}
	if len(keys) < 100 {
		// A truncated or bogus answer must not replace a good list.
		return fmt.Errorf("%s: only %d addresses", l.URL, len(keys))
	}
	l.mu.Lock()
	l.keys, l.updated = keys, t.Now()
	l.mu.Unlock()
	return nil
}

func parseBulkList(r io.Reader) ([]string, error) {
	var addrs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			addrs = append(addrs, line)
		}
	}
	return addrs, sc.Err()
}

func parseOnionoo(r io.Reader) ([]string, error) {
	var doc struct {
		Relays []struct {
			OrAddresses   []string `json:"or_addresses"`
			ExitAddresses []string `json:"exit_addresses"`
		} `json:"relays"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	var addrs []string
	for _, relay := range doc.Relays {
		for _, a := range relay.OrAddresses {
			if host, _, err := net.SplitHostPort(a); err == nil {
				addrs = append(addrs, host)
			}
		}
		addrs = append(addrs, relay.ExitAddresses...)
	}
	return addrs, nil
}

// addressKey identifies an address for the Tor lists and the rate limits:
// IPv4 as is, IPv6 by its /64, because anyone with IPv6 holds a whole /64
// and an exit may leave from any address in it.
func addressKey(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Limiter counts requests per address (IPv6 per /64) in fixed windows: one
// minute for all requests, TradeWindow and a day for trade creation. The daily
// cap stops a server that would resell the relay to its own users, whose
// addresses would then be missing from the trade log. Counters live in memory
// only and are dropped after their window ends.
type Limiter struct {
	PerMinute       int
	TradesPerWindow int
	TradeWindow     time.Duration
	TradesPerDay    int

	mu       sync.Mutex
	requests map[string]*window
	trades   map[string]*window
	daily    map[string]*window
}

type window struct {
	start time.Time
	count int
}

func (l *Limiter) Allow(ip string, isTrade bool, now time.Time) bool {
	key := addressKey(ip)
	if key == "" {
		key = ip
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.requests == nil {
		l.requests, l.trades, l.daily = map[string]*window{}, map[string]*window{}, map[string]*window{}
	}
	if !hit(l.requests, key, now, time.Minute, l.PerMinute) {
		return false
	}
	if !isTrade {
		return true
	}
	if !hit(l.trades, key, now, l.TradeWindow, l.TradesPerWindow) {
		return false
	}
	if !hit(l.daily, key, now, 24*time.Hour, l.TradesPerDay) {
		if l.daily[key].count == l.TradesPerDay+1 {
			// Counted, never identified: no address in the system log.
			log.Printf("limiter: an address reached the daily cap of %d trades", l.TradesPerDay)
		}
		return false
	}
	return true
}

func hit(m map[string]*window, ip string, now time.Time, length time.Duration, max int) bool {
	w := m[ip]
	if w == nil || now.Sub(w.start) >= length {
		w = &window{start: now}
		m[ip] = w
	}
	w.count++
	return w.count <= max
}

// Sweep forgets windows that have ended, so no IP stays in memory.
func (l *Limiter) Sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, w := range l.requests {
		if now.Sub(w.start) >= time.Minute {
			delete(l.requests, ip)
		}
	}
	for ip, w := range l.trades {
		if now.Sub(w.start) >= l.TradeWindow {
			delete(l.trades, ip)
		}
	}
	for ip, w := range l.daily {
		if now.Sub(w.start) >= 24*time.Hour {
			delete(l.daily, ip)
		}
	}
}

// torCheckHandler tells the app whether its request came through Tor, so it
// can warn when Tor mode is on but its traffic does not reach Tor. It answers
// only yes, no or unknown: never the address, and nothing is logged. On the
// onion service every request comes through Tor.
func torCheckHandler(tor *TorExits, onion bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		answer := "unknown"
		switch {
		case onion:
			answer = "yes"
		default:
			switch tor.Contains(clientIP(r)) {
			case torExit:
				answer = "yes"
			case notTorExit:
				answer = "no"
			}
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "{\"tor\":%q}\n", answer)
	})
}
