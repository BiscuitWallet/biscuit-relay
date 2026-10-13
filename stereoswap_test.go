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

// Answers follow Stereoswap's partner OpenAPI and real quotes (2026-10-13).
type fakeStereoswap struct {
	srv      *httptest.Server
	requests []*http.Request
	bodies   []string
}

const stereoswapUUID = "3f1c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f"

func stereoswapExchangeJSON(status int) string {
	return fmt.Sprintf(`{"status":%d,"type_swap":2,"steps":[],"id":"%s","order_hash":"SS7K2P9Q","mode":"standard","is_private":false,
		"refund_address":"%s","started_at":"2026-10-13T12:00:00Z","partner_profit":0,"finished_at":null,"created_at":"2026-10-13T12:00:00Z",
		"deposit":{"coin_network":{"coin":{"title":"Bitcoin","token":"BTC"},"network":{"title":"BTC"}},"amount":0.01,
			"address":"bc1qza5w3hqnznqdnhjedpkj6cj49nyjany8x84n3x","memo":null,"txid":null,"modified_at":"2026-10-13T12:00:00Z","created_at":"2026-10-13T12:00:00Z"},
		"withdrawal":{"coin_network":{"coin":{"title":"Monero","token":"XMR"},"network":{"title":"XMR"}},"amount":1.5612,
			"address":"%s","memo":null,"txid":null,"modified_at":"2026-10-13T12:00:00Z","created_at":"2026-10-13T12:00:00Z"}}`,
		status, stereoswapUUID, testBTCRefund, testXMRAddress)
}

func newFakeStereoswap(t *testing.T) *fakeStereoswap {
	f := &fakeStereoswap{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, r.Clone(r.Context()))
		f.bodies = append(f.bodies, string(body))
		if r.Header.Get("X-API-Key") != "stereo-key" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"detail": "Unauthorized"}`)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/partner/v1/")
		switch {
		case path == "exchange/calculate/":
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			if amount, _ := req["amount"].(float64); amount < 0.0664762 && req["last_source"] == "deposit" {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message": "Validation failed", "errors": [{"key": "amount", "code": "validation_error", "message": "Minimum exchange amount is 0.0664762."}]}`)
				return
			}
			if req["last_source"] == "receive" {
				fmt.Fprint(w, `{"amount": 0.9675102900473547, "receive_amount": 500.0, "rate": 516.79, "min_amount": 0.0664762, "max_amount": 1500.46272181}`)
				return
			}
			fmt.Fprint(w, `{"amount": 1.0, "receive_amount": 0.00629033, "rate": 0.00629033, "min_amount": 0.0664762, "max_amount": 1500.46272181}`)
		case path == "exchange/create/" && r.Method == http.MethodPost:
			fmt.Fprint(w, stereoswapExchangeJSON(9))
		case path == "exchange/"+stereoswapUUID:
			fmt.Fprint(w, stereoswapExchangeJSON(4))
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message": "Resource not found", "errors": [{"key": null, "code": "not_found", "message": "No Exchange matches the given query."}]}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestStereoswapExchanges(f *fakeStereoswap, referral string) *Exchanges {
	return &Exchanges{
		List:    []Exchange{&Stereoswap{BaseURL: f.srv.URL + "/partner/v1/", APIKey: "stereo-key", Referral: referral, Client: f.srv.Client()}},
		Limiter: &Limiter{PerMinute: 30, TradesPerWindow: 5, TradeWindow: 10 * time.Minute, TradesPerDay: 100},
		Now:     func() time.Time { return testNow },
	}
}

func TestStereoswapQuote(t *testing.T) {
	f := newFakeStereoswap(t)
	x := newTestStereoswapExchanges(f, "biscuit")
	rec := xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=1", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), `"exchange":"stereoswap"`) || !strings.Contains(rec.Body.String(), `"amount_to":"0.00629033"`) {
		t.Fatalf("quote: %s", rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["from_coin"] != "XMR" || sent["to_network"] != "BTC" || sent["amount"] != 1.0 || sent["last_source"] != "deposit" ||
		sent["type_swap"] != 2.0 || sent["mode"] != "standard" {
		t.Errorf("calculate body %v", sent)
	}

	// Fixed: the amount received, and the answer says how much to send.
	rec = xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=usdt&to_network=TRC20&amount=500&fixed=1", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), `"amount_from":"0.967510290048"`) || !strings.Contains(rec.Body.String(), `"amount_to":"500.0"`) {
		t.Errorf("fixed quote: %s", rec.Body.String())
	}
	if err := json.Unmarshal([]byte(f.bodies[1]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["last_source"] != "receive" || sent["type_swap"] != 1.0 || sent["to_coin"] != "USDT" || sent["to_network"] != "TRX" {
		t.Errorf("fixed body %v", sent)
	}

	rec = xget(x, "/api/x/quotes?from=xmr&from_network=Mainnet&to=btc&to_network=Mainnet&amount=0.01", "203.0.113.5")
	if !strings.Contains(rec.Body.String(), "Stereoswap: the minimum is 0.0664762 XMR.") {
		t.Errorf("minimum: %s", rec.Body.String())
	}
}

func TestStereoswapTrade(t *testing.T) {
	f := newFakeStereoswap(t)
	x := newTestStereoswapExchanges(f, "biscuit")
	rec := xget(x, "/api/x/new_trade?exchange=stereoswap&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address="+
		testXMRAddress+"&refund="+testBTCRefund, "185.220.101.1")
	if rec.Code != 200 {
		t.Fatalf("new_trade %d: %s", rec.Code, rec.Body.String())
	}
	var trade Trade
	if err := json.Unmarshal(rec.Body.Bytes(), &trade); err != nil {
		t.Fatal(err)
	}
	if trade.ID != "stereoswap-"+stereoswapUUID || trade.ExchangeID != "SS7K2P9Q" || trade.Status != "waiting" ||
		trade.DepositAddress == "" || trade.From != (Asset{"btc", "Mainnet"}) || trade.To != (Asset{"xmr", "Mainnet"}) || trade.Refund != testBTCRefund {
		t.Errorf("trade %+v", trade)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["referral_code"] != "biscuit" || sent["address"] != testXMRAddress || sent["refund_address"] != testBTCRefund || sent["amount"] != 0.01 {
		t.Errorf("create body %v", sent)
	}
	if _, ok := sent["email"]; ok {
		t.Error("an email was sent")
	}

	// Status: only for the payout address.
	rec = xget(x, "/api/x/trade?id="+trade.ID+"&address="+testXMRAddress, "185.220.101.1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"finished"`) {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{
		"/api/x/trade?id=stereoswap-00000000-0000-0000-0000-000000000000&address=" + testXMRAddress,
		"/api/x/trade?id=stereoswap-../exchange/create&address=" + testXMRAddress,
		"/api/x/trade?id=" + trade.ID + "&address=" + testBTCRefund,
	} {
		if rec := xget(x, target, "185.220.101.1"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestStereoswapNeedsReferralToCreate(t *testing.T) {
	f := newFakeStereoswap(t)
	x := newTestStereoswapExchanges(f, "")
	rec := xget(x, "/api/x/new_trade?exchange=stereoswap&from=btc&from_network=Mainnet&to=xmr&to_network=Mainnet&amount=0.01&address="+
		testXMRAddress, "203.0.113.5")
	if rec.Code == 200 || len(f.requests) != 0 {
		t.Errorf("created without a referral code: %d %s", rec.Code, rec.Body.String())
	}
}
