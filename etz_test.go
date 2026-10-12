// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Answers follow ETZ-Swap's OpenAPI examples and real quotes (2026-10-12).
type fakeETZ struct {
	srv      *httptest.Server
	requests []*http.Request
	bodies   []string
	rateErr  string
}

const etzToken = "a8e2cf6e-4b49-4ad7-b8cd-8df321bdabcf"

func etzTxJSON(status string) string {
	return `{"data":{"transactionId":"trJ0VwAal5sTKImP","amount":0.01,"amountTo":1.567,"coinFrom":{"coinCode":"BTC","coinName":"Bitcoin","network":"BTC"},
		"coinTo":{"coinCode":"XMR","coinName":"Monero","network":"XMR"},"deposit":{"address":"bc1qza5w3hqnznqdnhjedpkj6cj49nyjany8x84n3x","memo":""},
		"withdraw":{"address":"` + testXMRAddress + `","memo":""},"refund":{"address":"` + testBTCRefund + `","memo":""},
		"inboundTransfer":{"hash":"","link":""},"outboundTransfer":{"hash":"","link":""},"createdAt":"2026-10-12T19:30:00Z","rate":156.7,
		"rateType":"float","status":"` + status + `","confirmations":"0/1","transactionSource":"api",
		"transactionSecurity":{"type":"secureToken","secureToken":"` + etzToken + `","isSecured":true},
		"amountsInfo":{"expectedDepositAmount":0.01,"receivedDepositAmount":0,"expectedWithdrawalAmount":1.567,"sentWithdrawalAmount":1.5669}},"errors":{}}`
}

func newFakeETZ(t *testing.T) *fakeETZ {
	f := &fakeETZ{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, r.Clone(r.Context()))
		f.bodies = append(f.bodies, string(body))
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/deposit/public/")
		switch {
		case path == "rate":
			if f.rateErr != "" {
				w.WriteHeader(400)
				fmt.Fprint(w, f.rateErr)
				return
			}
			fmt.Fprint(w, `{"data":{"amountFrom":1,"amountTo":0.00637859,"minAmountFrom":0.1211109,"maxAmountFrom":1139.84492925,"rate":0.00634686},"errors":{}}`)
		case path == "transaction" && r.Method == http.MethodPost:
			fmt.Fprint(w, etzTxJSON("deposit"))
		case path == "transactions/trJ0VwAal5sTKImP":
			if r.URL.Query().Get("secureToken") != etzToken {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"data":null,"errors":{"transactionSecurityRestrictedAccess":{"message":"Transaction restricted access","args":{}}}}`)
				return
			}
			fmt.Fprint(w, etzTxJSON("success"))
		default:
			w.WriteHeader(400)
			fmt.Fprint(w, `{"data":null,"errors":{"transactionNotExist":{"message":"Transaction not exist","args":{}}}}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestETZExchanges(f *fakeETZ, keys bool) *Exchanges {
	e := &ETZ{BaseURL: f.srv.URL + "/api/v1/deposit/public/", KeyVersion: "1", Client: f.srv.Client()}
	if keys {
		e.PublicKey, e.Secret = "etz-public", "etz-secret"
	}
	return &Exchanges{
		List:    []Exchange{e},
		Limiter: &Limiter{PerMinute: 30, TradesPerWindow: 5, TradeWindow: 10 * time.Minute, TradesPerDay: 100},
		Now:     func() time.Time { return testNow },
	}
}

func TestETZQuote(t *testing.T) {
	f := newFakeETZ(t)
	x := newTestETZExchanges(f, false)
	rec := xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=1", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), `"exchange":"etz"`) || !strings.Contains(rec.Body.String(), `"amount_to":"0.00637859"`) {
		t.Fatalf("quote: %s", rec.Body.String())
	}
	q := f.requests[0].URL.Query()
	if q.Get("coinFrom") != "XMR" || q.Get("amountFrom") != "1" || q.Get("rateType") != "float" || q.Has("amountTo") {
		t.Errorf("query %v", q)
	}
	if f.requests[0].Header.Get("X-API-KEY") != "" {
		t.Error("key headers sent without keys")
	}

	// Fixed: the amount received goes as amountTo.
	xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=0.01&fixed=1", "203.0.113.5")
	if q := f.requests[1].URL.Query(); q.Get("amountTo") != "0.01" || q.Get("rateType") != "fixed" || q.Has("amountFrom") {
		t.Errorf("fixed query %v", q)
	}
}

func TestETZQuoteErrors(t *testing.T) {
	f := newFakeETZ(t)
	x := newTestETZExchanges(f, false)
	f.rateErr = `{"data":null,"errors":{"amountTooSmall":{"message":"Rate amount too small","args":{"actualAmount":0.05,"minAllowedAmount":0.1211109}}}}`
	rec := xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=0.05", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), "ETZ-Swap: the minimum is 0.1211109 XMR.") {
		t.Errorf("minimum: %s", rec.Body.String())
	}
	f.rateErr = `{"data":null,"errors":{"unsupportedExchangePairs":{"message":"Rate unsupported exchange pairs","args":{}}}}`
	rec = xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=1", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), "ETZ-Swap: Rate unsupported exchange pairs.") {
		t.Errorf("pair: %s", rec.Body.String())
	}
}

func TestETZTradeWithSecureToken(t *testing.T) {
	f := newFakeETZ(t)
	x := newTestETZExchanges(f, true)
	rec := xget(x, "/api/x/new_trade?exchange=etz&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address="+
		testXMRAddress+"&refund="+testBTCRefund, "185.220.101.1")
	if rec.Code != 200 {
		t.Fatalf("new_trade %d: %s", rec.Code, rec.Body.String())
	}
	var trade Trade
	if err := json.Unmarshal(rec.Body.Bytes(), &trade); err != nil {
		t.Fatal(err)
	}
	if trade.ID != "etz-trJ0VwAal5sTKImP."+etzToken || trade.ExchangeID != "trJ0VwAal5sTKImP" || trade.Status != "waiting" ||
		trade.DepositAddress == "" || trade.From != (Asset{"btc", "Mainnet"}) || trade.Refund != testBTCRefund {
		t.Errorf("trade %+v", trade)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["securityType"] != "secureToken" || sent["amountFrom"] != 0.01 || sent["withdrawalAddress"] != testXMRAddress || sent["rateType"] != "float" {
		t.Errorf("create body %v", sent)
	}
	if h := f.requests[0].Header; h.Get("X-API-KEY") != "etz-public" || h.Get("X-API-SECRET-KEY") != "etz-secret" || h.Get("X-API-KEY-VERSION") != "1" {
		t.Errorf("key headers %v", h)
	}

	// Status: with the token from the ID, and only for the payout address.
	rec = xget(x, "/api/x/trade?id="+trade.ID+"&address="+testXMRAddress, "185.220.101.1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"finished"`) || !strings.Contains(rec.Body.String(), `"amount_to":"1.5669"`) {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{
		"/api/x/trade?id=etz-trJ0VwAal5sTKImP.wrong-token&address=" + testXMRAddress,
		"/api/x/trade?id=etz-trJ0VwAal5sTKImP&address=" + testXMRAddress,
		"/api/x/trade?id=" + trade.ID + "&address=" + testBTCRefund,
	} {
		if rec := xget(x, target, "185.220.101.1"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestQuotesFromSeveralExchanges(t *testing.T) {
	pegasus, etz := newFakePegasus(t), newFakeETZ(t)
	x := newTestExchanges(pegasus)
	x.List = append(x.List, newTestETZExchanges(etz, false).List...)
	etz.rateErr = `{"data":null,"errors":{"amountTooSmall":{"message":"Rate amount too small","args":{"minAllowedAmount":0.02}}}}`
	rec := xget(x, "/api/x/quotes?from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01", "203.0.113.5")
	var got struct {
		Quotes []Quote  `json:"quotes"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Quotes) != 1 || got.Quotes[0].Exchange != "pegasus" || len(got.Errors) != 1 || got.Errors[0] != "ETZ-Swap: the minimum is 0.02 BTC." {
		t.Errorf("got %+v", got)
	}
}
