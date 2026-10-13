// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testXMRAddress = "87bsQNEnRRe56A6ERMLmwDCanjkoN6Q2eFwCFqi5tLAQcwzvxDj6rxZUAa2rRPqvX8a6fdpoerpxnNSRTPyAFt4zJsDvZcB"
	testBTCRefund  = "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"
)

type fakePegasus struct {
	srv      *httptest.Server
	requests []*http.Request
	bodies   []string
	quote    string
	quoteErr int
	tx       string
}

func newFakePegasus(t *testing.T) *fakePegasus {
	f := &fakePegasus{
		quote: `{"pair":"BTC_XMR","amount":"0.01","exchangeRate":154.16,"receive":1.5416175203259680,"minAmount":1.2116616618981723e-3,"maxAmount":27.5}`,
		tx: `{"type":{"typeSwap":2},"pairs":{"deposit":{"coin":{"name":"BTC","value":0.01,"network":"BTC","fullName":"Bitcoin"},"address":"bc1qdeposit000000000000000000000000000000","memo":""},
			"receive":{"coin":{"name":"XMR","value":1.5416175,"network":"XMR","fullName":"Monero"},"address":"` + testXMRAddress + `","memo":""}},
			"receiveTransaction":{"txId":"","address":"","receivedTime":"","gettingInTheBlock":"","amount":null},
			"swapTime":{"end":"","duration":"","start":""},"orderNumber":"F723NT","status":9,"txId":"","refundAddress":"` + testBTCRefund + `",
			"receivedTime":"","gettingInTheBlock":"","confirmation":null,"createdAt":"2026-10-12T18:00:00.000Z"}`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, r.Clone(r.Context()))
		f.bodies = append(f.bodies, string(body))
		payload := strings.TrimPrefix(r.URL.Path, "/api/private/")
		mac := hmac.New(sha512.New, []byte("test-secret"))
		mac.Write([]byte(payload))
		if r.Header.Get("x-api-public-key") != "test-public" || r.Header.Get("x-api-payload") != payload ||
			r.Header.Get("x-api-signature") != hex.EncodeToString(mac.Sum(nil)) {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"statusCode":403,"message":"Invalid signature"}`)
			return
		}
		switch payload {
		case "exchange-coin":
			if f.quoteErr != 0 {
				w.WriteHeader(f.quoteErr)
				fmt.Fprint(w, `{"statusCode":400,"message":"Amount is less than minimum"}`)
				return
			}
			fmt.Fprint(w, f.quote)
		case "create-transaction":
			w.WriteHeader(201)
			fmt.Fprint(w, f.tx)
		case "get-transaction":
			if r.URL.Query().Get("id") != "F723NT" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"statusCode":404,"message":"Transaction not found"}`)
				return
			}
			fmt.Fprint(w, strings.Replace(f.tx, `"status":9`, `"status":4`, 1))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestExchanges(f *fakePegasus) *Exchanges {
	return &Exchanges{
		List:    []Exchange{&Pegasus{BaseURL: f.srv.URL + "/api/private/", PublicKey: "test-public", Secret: "test-secret", Client: f.srv.Client()}},
		Limiter: &Limiter{PerMinute: 30, TradesPerWindow: 5, TradeWindow: 10 * time.Minute, TradesPerDay: 100},
		Now:     func() time.Time { return testNow },
	}
}

func xget(h http.Handler, target, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = remote + ":4444"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestExchangesQuote(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	rec := xget(x, "/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01", "203.0.113.5")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Quotes []Quote  `json:"quotes"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Quotes) != 1 || len(got.Errors) != 0 {
		t.Fatalf("got %+v", got)
	}
	q := got.Quotes[0]
	if q.Exchange != "pegasus" || q.KYC != "A" || q.AmountFrom != "0.01" || q.AmountTo != "1.541617520325" || q.Min != "0.0012116616618981723" {
		t.Errorf("quote %+v", q)
	}
	params := f.requests[0].URL.Query()
	if params.Get("coinFrom") != "BTC" || params.Get("networkFrom") != "BTC" || params.Get("coinTo") != "XMR" || params.Get("lastSource") != "deposit" || params.Get("typeSwap") != "2" {
		t.Errorf("upstream query %v", params)
	}
	if ua := f.requests[0].Header.Get("User-Agent"); ua != "biscuit-relay" {
		t.Errorf("user agent %q", ua)
	}
}

func TestExchangesQuoteBelowMinimum(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	rec := xget(x, "/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.001", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), "PegasusSwap: the minimum is 0.001212 BTC.") {
		t.Errorf("below minimum: %s", rec.Body.String())
	}
	f.quoteErr = 400
	rec = xget(x, "/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.001", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), "PegasusSwap: Amount is less than minimum") {
		t.Errorf("upstream refusal: %s", rec.Body.String())
	}
}

func TestExchangesRejectsBadRequests(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	for _, target := range []string{
		"/api/x/quotes?from=pepe&from_network=ERC20&to=xmr&to_network=Mainnet&amount=1",
		"/api/x/quotes?from=xmr&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=1",
		"/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=1e3",
		"/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0",
		"/api/x/new_trade?exchange=pegasus&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address=x%22%7D",
		"/api/x/new_trade?exchange=other&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address=" + testXMRAddress,
	} {
		if rec := xget(x, target, "203.0.113.5"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
	if rec := xget(x, "/api/x/coins", "203.0.113.5"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", rec.Code)
	}
	if len(f.requests) != 0 {
		t.Errorf("%d requests reached the exchange", len(f.requests))
	}
}

func TestExchangesTradeThroughTor(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	// A Tor exit: allowed here, unlike the Trocador relay.
	rec := xget(x, "/api/x/new_trade?exchange=pegasus&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address="+
		testXMRAddress+"&refund="+testBTCRefund, "185.220.101.1")
	if rec.Code != 200 {
		t.Fatalf("new_trade %d: %s", rec.Code, rec.Body.String())
	}
	var trade Trade
	if err := json.Unmarshal(rec.Body.Bytes(), &trade); err != nil {
		t.Fatal(err)
	}
	if trade.ID != "pegasus-F723NT" || trade.Status != "waiting" || trade.From != (Asset{"btc", "Mainnet"}) ||
		trade.To != (Asset{"xmr", "Mainnet"}) || trade.DepositAddress == "" || trade.Refund != testBTCRefund || trade.KYC != "A" {
		t.Errorf("trade %+v", trade)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["receiveAddress"] != testXMRAddress || sent["depositAmount"] != 0.01 || sent["typeSwap"] != float64(2) ||
		sent["refundAddress"] != testBTCRefund || sent["depositNetwork"] != "BTC" {
		t.Errorf("upstream body %v", sent)
	}

	// Status: only with the payout address.
	rec = xget(x, "/api/x/trade?id=pegasus-F723NT&address="+testXMRAddress, "185.220.101.1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"finished"`) {
		t.Errorf("trade %d: %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{
		"/api/x/trade?id=pegasus-F723NT&address=" + testBTCRefund,
		"/api/x/trade?id=pegasus-F723NT",
		"/api/x/trade?id=pegasus-ZZZZZZ&address=" + testXMRAddress,
		"/api/x/trade?id=other-F723NT&address=" + testXMRAddress,
	} {
		if rec := xget(x, target, "185.220.101.1"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "deposit") {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestExchangesFixedRateTrade(t *testing.T) {
	f := newFakePegasus(t)
	// Fixed: 0.01 BTC received costs 1.57544184 XMR.
	f.quote = `{"amount":1.57544184,"pair":"XMR_BTC","receive":0.01,"exchangeRate":0.0063474,"maxAmount":1574.8,"minAmount":0.01873736}`
	x := newTestExchanges(f)
	rec := xget(x, "/api/x/new_trade?exchange=pegasus&fixed=1&from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=0.01&address="+
		testBTCRefund+"&refund="+testXMRAddress, "203.0.113.5")
	if rec.Code != 200 {
		t.Fatalf("new_trade %d: %s", rec.Code, rec.Body.String())
	}
	q := f.requests[0].URL.Query()
	if q.Get("lastSource") != "receive" || q.Get("typeSwap") != "1" || q.Get("amount") != "0.01" {
		t.Errorf("quote before the fixed trade: %v", q)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[1]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["depositAmount"] != 1.57544184 || sent["lastSource"] != "receive" || sent["typeSwap"] != float64(1) {
		t.Errorf("fixed trade body %v", sent)
	}
}

func TestPegasusTokensOnEthereum(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	xget(x, "/api/x/quotes?from=usdt&from_network=ERC20&to=xmr&to_network=Mainnet&amount=200", "203.0.113.5")
	if q := f.requests[0].URL.Query(); q.Get("coinFrom") != "USDT" || q.Get("networkFrom") != "ETH" {
		t.Errorf("USDT query %v", q)
	}
	p := &Pegasus{}
	for _, network := range []string{"ETH", "ERC20", "eth"} {
		if a, ok := p.asset("USDC", network); !ok || a != (Asset{"usdc", "ERC20"}) {
			t.Errorf("USDC on %s: %v %v", network, a, ok)
		}
	}
}

func TestExchangesListOutsideCoins(t *testing.T) {
	x := newTestExchanges(newFakePegasus(t))
	x.List = append(x.List, &ETZ{})
	rec := xget(x, "/api/x/assets", "203.0.113.5")
	var r struct {
		Assets []struct {
			Asset
			Memo bool `json:"memo"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	listed := map[Asset]bool{}
	for _, a := range r.Assets {
		listed[a.Asset] = a.Memo
	}
	// Coins Biscuit holds first, then outside coins any exchange maps: XRP
	// (ETZ only) needs a memo, ZEC is mapped by neither.
	if len(r.Assets) < 7 || r.Assets[0].Asset != (Asset{"xmr", "Mainnet"}) || r.Assets[6].Asset != (Asset{"usdt", "TRC20"}) {
		t.Errorf("order: %v", r.Assets)
	}
	if memo, ok := listed[Asset{"xrp", "Mainnet"}]; !ok || !memo {
		t.Errorf("XRP: listed %v, memo %v", ok, memo)
	}
	if _, ok := listed[Asset{"sol", "Mainnet"}]; !ok {
		t.Error("SOL not listed")
	}
	for _, a := range []Asset{{"zec", "Mainnet"}} {
		if _, ok := listed[a]; ok {
			t.Errorf("%v listed without an exchange", a)
		}
	}
}

func TestPegasusTokensOnTronAndSolana(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=usdt&to_network=TRC20&amount=1", "203.0.113.5")
	if q := f.requests[0].URL.Query(); q.Get("coinTo") != "USDT" || q.Get("networkTo") != "TRX" {
		t.Errorf("USDT on Tron query %v", q)
	}
	p := &Pegasus{}
	if a, ok := p.asset("USDC", "SOL"); !ok || a != (Asset{"usdc", "SOL"}) {
		t.Errorf("USDC on Solana: %v %v", a, ok)
	}
	if got := roundAmount("1.1234567891", Asset{"sol", "Mainnet"}, false); got != "1.123456789" {
		t.Errorf("SOL decimals: %s", got)
	}
}

func TestExchangesRateLimited(t *testing.T) {
	f := newFakePegasus(t)
	x := newTestExchanges(f)
	target := "/api/x/new_trade?exchange=pegasus&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address=" + testXMRAddress
	for i := 0; i < 5; i++ {
		if rec := xget(x, target, "203.0.113.9"); rec.Code != 200 {
			t.Fatalf("trade %d: %d", i, rec.Code)
		}
	}
	if rec := xget(x, target, "203.0.113.9"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("sixth trade: %d", rec.Code)
	}
}

func TestRoundAmount(t *testing.T) {
	xmr, usdt := Asset{"xmr", "Mainnet"}, Asset{"usdt", "ERC20"}
	for _, c := range []struct {
		in   string
		a    Asset
		up   bool
		want string
	}{
		{"0.18820341024579365", xmr, true, "0.188203410246"},
		{"0.18820341024579365", xmr, false, "0.188203410245"},
		{"527.35495", usdt, false, "527.35495"},
		{"527.3549512", usdt, true, "527.354952"},
		{"0.0000001", usdt, true, "0.000001"},
		{"0.0000001", usdt, false, "0"},
		{"12", xmr, true, "12"},
		{"1.5000000000000", xmr, true, "1.5"},
	} {
		if got := roundAmount(c.in, c.a, c.up); got != c.want {
			t.Errorf("roundAmount(%s, %s, %v) = %s, want %s", c.in, c.a.Ticker, c.up, got, c.want)
		}
	}
}

func TestDecimalKeepsPlainNotation(t *testing.T) {
	for in, want := range map[string]string{`"10"`: "10", `0.5`: "0.5", `1.2e-3`: "0.0012", `null`: "", `"7E2"`: "700"} {
		var d decimal
		if err := json.Unmarshal([]byte(in), &d); err != nil || string(d) != want {
			t.Errorf("%s: %q %v", in, d, err)
		}
	}
}

func TestOnionServesExchangesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakePegasus(t)
	onion := onionRoutes(staticSite(dir, ""), http.NotFoundHandler(), newTestExchanges(f))
	if rec := xget(onion, "http://abc.onion/api/x/assets", "127.0.0.1"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"xmr"`) {
		t.Errorf("assets on the onion: %d %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{"http://abc.onion/api/new_trade?id=R1", "http://abc.onion/api/coins"} {
		if rec := xget(onion, target, "127.0.0.1"); rec.Code != http.StatusNotFound {
			t.Errorf("%s on the onion: %d", target, rec.Code)
		}
	}
}
