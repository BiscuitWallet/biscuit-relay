// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Biscuit's own small aggregator, under /api/x/: exchanges that never ask for
// KYC, called with keys kept on this server. Unlike Trocador, they accept
// swaps created through Tor, so these routes work from Tor exits and from the
// onion service. Nothing is logged: no IP, no trade.
//
//	GET /api/x/assets
//	GET /api/x/quotes?from=&from_network=&to=&to_network=&amount=&fixed=
//	GET /api/x/new_trade?exchange=&from=&from_network=&to=&to_network=&amount=&fixed=&address=&memo=&refund=&refund_memo=
//	GET /api/x/trade?id=&address=
//
// Assets use the app's names: lowercase ticker and Trocador's network names
// ("Mainnet", "ERC20"). fixed=1: amount is the exact amount received, and the
// quote says how much to send.

// Asset is a coin on one network, named as in the app.
type Asset struct {
	Ticker  string `json:"ticker"`
	Network string `json:"network"`
}

// The coins Biscuit holds, with their decimals. An exchange only quotes the
// ones it maps.
var walletAssets = []struct {
	Asset
	Name     string
	Decimals int
}{
	{Asset{"xmr", "Mainnet"}, "Monero", 12},
	{Asset{"btc", "Mainnet"}, "Bitcoin", 8},
	{Asset{"ltc", "Mainnet"}, "Litecoin", 8},
	{Asset{"eth", "ERC20"}, "Ethereum", 18},
	{Asset{"usdt", "ERC20"}, "Tether", 6},
	{Asset{"usdc", "ERC20"}, "USD Coin", 6},
}

func decimalsOf(a Asset) int {
	for _, w := range walletAssets {
		if w.Asset == a {
			return w.Decimals
		}
	}
	return 18
}

// roundAmount cuts a decimal string to the coin's decimals: up for what the
// user sends (the exchange must receive enough), down for what they get. The
// app sends exact atomic amounts and refuses extra digits.
func roundAmount(s string, a Asset, up bool) string {
	intPart, frac, found := strings.Cut(s, ".")
	n := decimalsOf(a)
	if !found || len(frac) <= n {
		return s
	}
	cut, rest := frac[:n], frac[n:]
	r := new(big.Int)
	if _, ok := r.SetString(intPart+cut, 10); !ok {
		return s
	}
	if up && strings.Trim(rest, "0") != "" {
		r.Add(r, big.NewInt(1))
	}
	digits := r.String()
	if len(digits) <= n {
		digits = strings.Repeat("0", n-len(digits)+1) + digits
	}
	out := digits[:len(digits)-n] + "." + digits[len(digits)-n:]
	out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	return out
}

type QuoteRequest struct {
	From, To Asset
	Amount   string // sent, or received when Fixed
	Fixed    bool
}

type Quote struct {
	Exchange   string `json:"exchange"`
	Name       string `json:"name"`
	KYC        string `json:"kyc"`
	AmountFrom string `json:"amount_from"`
	AmountTo   string `json:"amount_to"`
	Fixed      bool   `json:"fixed"`
	Min        string `json:"min,omitempty"` // in the coin sent
	Max        string `json:"max,omitempty"`
}

type TradeRequest struct {
	QuoteRequest
	Address, Memo      string
	Refund, RefundMemo string
}

// Trade is what the app stores and polls. ID is "<exchange>-<exchange id>".
type Trade struct {
	ID             string `json:"id"`
	Exchange       string `json:"exchange"`
	Name           string `json:"name"`
	KYC            string `json:"kyc"`
	ExchangeID     string `json:"exchange_id"`
	Support        string `json:"support"` // the exchange's support page
	From           Asset  `json:"from"`
	To             Asset  `json:"to"`
	AmountFrom     string `json:"amount_from"`
	AmountTo       string `json:"amount_to"`
	Fixed          bool   `json:"fixed"`
	DepositAddress string `json:"deposit_address"`
	DepositMemo    string `json:"deposit_memo"`
	Address        string `json:"address"`
	Memo           string `json:"memo"`
	Refund         string `json:"refund"`
	Status         string `json:"status"` // the app's names: waiting, confirming, sending, finished...
	PayoutTxID     string `json:"payout_txid,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// Exchange is one partner. Errors of type *UserError are shown to the user.
type Exchange interface {
	ID() string // short, lowercase, no '-'
	Name() string
	KYC() string // "A": never asks for KYC
	Support() string
	Supports(a Asset) bool
	Quote(ctx context.Context, q QuoteRequest) (Quote, error)
	Create(ctx context.Context, t TradeRequest) (Trade, error)
	Status(ctx context.Context, exchangeID string) (Trade, error)
}

// UserError carries a message meant for the user (amount too small...).
type UserError struct{ Message string }

func (e *UserError) Error() string { return e.Message }

var errNotFound = errors.New("not found")

type Exchanges struct {
	List    []Exchange
	Limiter *Limiter
	Now     func() time.Time
}

func (x *Exchanges) find(id string) Exchange {
	for _, e := range x.List {
		if e.ID() == id {
			return e
		}
	}
	return nil
}

func (x *Exchanges) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/api/x/")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Unknown endpoint.")
		return
	}
	switch method {
	case "assets", "quotes", "new_trade", "trade":
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown endpoint.")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed.")
		return
	}
	if len(r.URL.RawQuery) > maxQueryBytes {
		writeError(w, http.StatusRequestURITooLong, "query_too_long", "Query too long.")
		return
	}
	if !x.Limiter.Allow(clientIP(r), method == "new_trade", x.Now()) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again in a few minutes.")
		return
	}
	if len(x.List) == 0 {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Swaps are temporarily unavailable. Try again later.")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_query", "Malformed query.")
		return
	}

	switch method {
	case "assets":
		x.assets(w)
	case "quotes":
		x.quotes(w, r.Context(), query)
	case "new_trade":
		x.newTrade(w, r.Context(), query)
	case "trade":
		x.trade(w, r.Context(), query)
	}
}

func (x *Exchanges) assets(w http.ResponseWriter) {
	type item struct {
		Asset
		Name string `json:"name"`
	}
	list := []item{}
	for _, a := range walletAssets {
		for _, e := range x.List {
			if e.Supports(a.Asset) {
				list = append(list, item{a.Asset, a.Name})
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": list})
}

func parseQuoteRequest(q url.Values) (QuoteRequest, string) {
	req := QuoteRequest{
		From:   Asset{q.Get("from"), q.Get("from_network")},
		To:     Asset{q.Get("to"), q.Get("to_network")},
		Amount: q.Get("amount"),
		Fixed:  q.Get("fixed") == "1" || q.Get("fixed") == "true",
	}
	if !knownAsset(req.From) || !knownAsset(req.To) || req.From == req.To {
		return req, "Unsupported coins."
	}
	if !positiveDecimal(req.Amount) {
		return req, "Invalid amount."
	}
	return req, ""
}

func knownAsset(a Asset) bool {
	for _, w := range walletAssets {
		if w.Asset == a {
			return true
		}
	}
	return false
}

// positiveDecimal accepts plain decimals only ("0.5", "12"), as the app sends.
func positiveDecimal(s string) bool {
	if s == "" || len(s) > 40 || strings.Count(s, ".") > 1 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	nonZero := false
	for _, c := range s {
		switch {
		case c == '.':
		case c >= '0' && c <= '9':
			if c != '0' {
				nonZero = true
			}
		default:
			return false
		}
	}
	return nonZero
}

func (x *Exchanges) quotes(w http.ResponseWriter, ctx context.Context, q url.Values) {
	req, bad := parseQuoteRequest(q)
	if bad != "" {
		writeError(w, http.StatusBadRequest, "bad_request", bad)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	type result struct {
		quote Quote
		err   string
	}
	results := make([]result, len(x.List))
	var wg sync.WaitGroup
	for i, e := range x.List {
		if !e.Supports(req.From) || !e.Supports(req.To) {
			continue
		}
		wg.Add(1)
		go func(i int, e Exchange) {
			defer wg.Done()
			quote, err := e.Quote(ctx, req)
			if err != nil {
				results[i].err = e.Name() + ": " + userMessage(err)
				return
			}
			quote.AmountFrom = roundAmount(quote.AmountFrom, req.From, true)
			quote.AmountTo = roundAmount(quote.AmountTo, req.To, false)
			results[i].quote = quote
		}(i, e)
	}
	wg.Wait()

	quotes, errs := []Quote{}, []string{}
	for _, r := range results {
		if r.err != "" {
			errs = append(errs, r.err)
		} else if r.quote.Exchange != "" {
			quotes = append(quotes, r.quote)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"quotes": quotes, "errors": errs})
}

func (x *Exchanges) newTrade(w http.ResponseWriter, ctx context.Context, q url.Values) {
	req, bad := parseQuoteRequest(q)
	if bad != "" {
		writeError(w, http.StatusBadRequest, "bad_request", bad)
		return
	}
	e := x.find(q.Get("exchange"))
	if e == nil || !e.Supports(req.From) || !e.Supports(req.To) {
		writeError(w, http.StatusBadRequest, "bad_request", "Unknown exchange.")
		return
	}
	t := TradeRequest{QuoteRequest: req, Address: q.Get("address"), Memo: q.Get("memo"),
		Refund: q.Get("refund"), RefundMemo: q.Get("refund_memo")}
	if !plausibleAddress(t.Address) || (t.Refund != "" && !plausibleAddress(t.Refund)) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid address.")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	trade, err := e.Create(ctx, t)
	if err != nil {
		writeExchangeError(w, e, err)
		return
	}
	writeJSON(w, http.StatusOK, rounded(trade))
}

// trade answers only to whoever knows the payout address: swap ids are
// short, they must not be enough to read someone's addresses.
func (x *Exchanges) trade(w http.ResponseWriter, ctx context.Context, q url.Values) {
	exchangeID, id, _ := strings.Cut(q.Get("id"), "-")
	e := x.find(exchangeID)
	address := q.Get("address")
	if e == nil || id == "" || address == "" {
		writeError(w, http.StatusNotFound, "not_found", "Swap not found.")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	trade, err := e.Status(ctx, id)
	if errors.Is(err, errNotFound) || (err == nil && !strings.EqualFold(trade.Address, address)) {
		writeError(w, http.StatusNotFound, "not_found", "Swap not found.")
		return
	}
	if err != nil {
		writeExchangeError(w, e, err)
		return
	}
	writeJSON(w, http.StatusOK, rounded(trade))
}

func rounded(t Trade) Trade {
	t.AmountFrom = roundAmount(t.AmountFrom, t.From, true)
	t.AmountTo = roundAmount(t.AmountTo, t.To, false)
	return t
}

func plausibleAddress(s string) bool {
	if len(s) < 20 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func userMessage(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Message
	}
	return "unavailable, try again later."
}

func writeExchangeError(w http.ResponseWriter, e Exchange, err error) {
	var ue *UserError
	if errors.As(err, &ue) {
		writeError(w, http.StatusBadRequest, "exchange_refused", e.Name()+": "+ue.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", e.Name()+" is unavailable. Try again later.")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decimal is a JSON number or numeric string, kept as a plain decimal string
// ("0.00627", never "6.27e-3"), as the app expects.
type decimal string

func (d *decimal) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = ""
		return nil
	}
	f, ok := new(big.Float).SetPrec(200).SetString(s)
	if !ok {
		return fmt.Errorf("not a number: %q", s)
	}
	if !strings.ContainsAny(s, "eE") {
		*d = decimal(s)
		return nil
	}
	*d = decimal(f.Text('f', -1))
	return nil
}
