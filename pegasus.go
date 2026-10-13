// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PegasusSwap (https://pegasusswap.com/api-docs): never asks for KYC, its
// terms say so even for refunds. Each request is signed with the partner
// secret: x-api-signature = hex(HMAC-SHA512(payload, secret)), where the
// payload is the endpoint's name.
type Pegasus struct {
	BaseURL   string // "https://api.pegasusswap.com/api/private/"
	PublicKey string
	Secret    string
	Client    *http.Client
}

// Pegasus coin and network of each asset, as get-all-coins lists them
// (checked 2026-10-12, outside coins 2026-10-13): tokens are on the network
// of their chain, "ETH", "TRX" or "SOL". No ZEC, XRP, XLM, BNB or TON.
var pegasusAssets = map[Asset][2]string{
	{"xmr", "Mainnet"}:  {"XMR", "XMR"},
	{"btc", "Mainnet"}:  {"BTC", "BTC"},
	{"ltc", "Mainnet"}:  {"LTC", "LTC"},
	{"eth", "ERC20"}:    {"ETH", "ETH"},
	{"usdt", "ERC20"}:   {"USDT", "ETH"},
	{"usdc", "ERC20"}:   {"USDC", "ETH"},
	{"usdt", "TRC20"}:   {"USDT", "TRX"},
	{"sol", "Mainnet"}:  {"SOL", "SOL"},
	{"trx", "Mainnet"}:  {"TRX", "TRX"},
	{"doge", "Mainnet"}: {"DOGE", "DOGE"},
	{"ada", "Mainnet"}:  {"ADA", "ADA"},
	{"bch", "Mainnet"}:  {"BCH", "BCH"},
	{"usdc", "SOL"}:     {"USDC", "SOL"},
}

func (p *Pegasus) ID() string   { return "pegasus" }
func (p *Pegasus) Name() string { return "PegasusSwap" }
func (p *Pegasus) KYC() string  { return "A" }

func (p *Pegasus) Support() string { return "https://pegasusswap.com/support" }

func (p *Pegasus) Supports(a Asset) bool {
	_, ok := pegasusAssets[a]
	return ok
}

// asset reads a swap's coin back. Their docs also show "ERC20" as a network
// name in swaps: taken as Ethereum.
func (p *Pegasus) asset(coin, network string) (Asset, bool) {
	if strings.EqualFold(network, "ERC20") {
		network = "ETH"
	}
	for a, c := range pegasusAssets {
		if strings.EqualFold(c[0], coin) && strings.EqualFold(c[1], network) {
			return a, true
		}
	}
	return Asset{}, false
}

func typeSwap(fixed bool) int {
	if fixed {
		return 1
	}
	return 2
}

func lastSource(fixed bool) string {
	if fixed {
		return "receive"
	}
	return "deposit"
}

func (p *Pegasus) Quote(ctx context.Context, q QuoteRequest) (Quote, error) {
	from, to := pegasusAssets[q.From], pegasusAssets[q.To]
	params := url.Values{
		"amount":      {q.Amount},
		"coinFrom":    {from[0]},
		"networkFrom": {from[1]},
		"coinTo":      {to[0]},
		"networkTo":   {to[1]},
		"lastSource":  {lastSource(q.Fixed)},
		"typeSwap":    {fmt.Sprint(typeSwap(q.Fixed))},
	}
	var r struct {
		Amount    decimal `json:"amount"`
		Receive   decimal `json:"receive"`
		MinAmount decimal `json:"minAmount"`
		MaxAmount decimal `json:"maxAmount"`
	}
	if err := p.call(ctx, http.MethodGet, "exchange-coin", params, nil, &r); err != nil {
		return Quote{}, err
	}
	quote := Quote{Exchange: p.ID(), Name: p.Name(), KYC: p.KYC(), Fixed: q.Fixed,
		AmountFrom: string(r.Amount), AmountTo: string(r.Receive),
		Min: string(r.MinAmount), Max: string(r.MaxAmount)}
	if q.Fixed {
		quote.AmountTo = q.Amount
	} else {
		quote.AmountFrom = q.Amount
	}
	if !positiveDecimal(quote.AmountFrom) || !positiveDecimal(quote.AmountTo) {
		return Quote{}, fmt.Errorf("pegasus: unusable quote %+v", r)
	}
	if msg := outOfRange(quote.AmountFrom, quote.Min, quote.Max, q.From); msg != "" {
		return Quote{}, &UserError{msg}
	}
	return quote, nil
}

// outOfRange says, for the user, when the amount sent is outside the
// exchange's limits.
func outOfRange(amount, min, max string, a Asset) string {
	v, ok := new(big.Float).SetString(amount)
	if !ok {
		return ""
	}
	unit := strings.ToUpper(a.Ticker)
	if m, ok := new(big.Float).SetString(min); ok && min != "" && v.Cmp(m) < 0 {
		return fmt.Sprintf("the minimum is %s %s.", roundUp(m), unit)
	}
	if m, ok := new(big.Float).SetString(max); ok && max != "" && m.Sign() > 0 && v.Cmp(m) > 0 {
		return fmt.Sprintf("the maximum is %s %s.", m.Text('f', 4), unit)
	}
	return ""
}

// roundUp shows a minimum with 6 decimals, rounded up so it is enough.
func roundUp(f *big.Float) string {
	scaled := new(big.Float).Mul(f, big.NewFloat(1e6))
	i, acc := scaled.Int(nil)
	if acc == big.Below {
		i.Add(i, big.NewInt(1))
	}
	s := new(big.Float).Quo(new(big.Float).SetInt(i), big.NewFloat(1e6)).Text('f', 6)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func (p *Pegasus) Create(ctx context.Context, t TradeRequest) (Trade, error) {
	from, to := pegasusAssets[t.From], pegasusAssets[t.To]
	// depositAmount is always what is sent. Fixed rate: t.Amount is what is
	// received, so a fresh quote says how much to send (lastSource=receive).
	// To confirm on the first real fixed-rate swap.
	deposit := t.Amount
	if t.Fixed {
		quote, err := p.Quote(ctx, t.QuoteRequest)
		if err != nil {
			return Trade{}, err
		}
		deposit = roundAmount(quote.AmountFrom, t.From, true)
	}
	amount, err := json.Marshal(json.Number(deposit))
	if err != nil {
		return Trade{}, err
	}
	body := map[string]any{
		"depositCoin":    from[0],
		"depositNetwork": from[1],
		"depositAmount":  json.RawMessage(amount),
		"receiveCoin":    to[0],
		"receiveNetwork": to[1],
		"receiveAddress": t.Address,
		"typeSwap":       typeSwap(t.Fixed),
		"lastSource":     lastSource(t.Fixed),
	}
	if t.Memo != "" {
		body["memo"] = t.Memo
	}
	if t.Refund != "" {
		body["refundAddress"] = t.Refund
		if t.RefundMemo != "" {
			body["refundMemo"] = t.RefundMemo
		}
	}
	var r pegasusTx
	if err := p.call(ctx, http.MethodPost, "create-transaction", nil, body, &r); err != nil {
		return Trade{}, err
	}
	trade, err := p.trade(r)
	if err != nil {
		return Trade{}, err
	}
	if trade.Refund == "" {
		trade.Refund = t.Refund
	}
	return trade, nil
}

func (p *Pegasus) Status(ctx context.Context, id string) (Trade, error) {
	if len(id) > 32 || !plausibleID(id) {
		return Trade{}, errNotFound
	}
	var r pegasusTx
	if err := p.call(ctx, http.MethodGet, "get-transaction", url.Values{"id": {id}}, nil, &r); err != nil {
		return Trade{}, err
	}
	return p.trade(r)
}

func plausibleID(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return s != ""
}

type pegasusSide struct {
	Coin struct {
		Name    string  `json:"name"`
		Value   decimal `json:"value"`
		Network string  `json:"network"`
	} `json:"coin"`
	Address string `json:"address"`
	Memo    string `json:"memo"`
}

type pegasusTx struct {
	Type struct {
		TypeSwap int `json:"typeSwap"`
	} `json:"type"`
	Pairs struct {
		Deposit pegasusSide `json:"deposit"`
		Receive pegasusSide `json:"receive"`
	} `json:"pairs"`
	ReceiveTransaction struct {
		TxID   string  `json:"txId"`
		Amount decimal `json:"amount"`
	} `json:"receiveTransaction"`
	OrderNumber   string `json:"orderNumber"`
	Status        int    `json:"status"`
	RefundAddress string `json:"refundAddress"`
	CreatedAt     string `json:"createdAt"`
}

// Pegasus statuses, in the app's names.
var pegasusStatus = map[int]string{
	9:  "waiting",    // New: waiting for the deposit
	1:  "confirming", // Confirmation
	3:  "confirming", // Exchanging
	12: "sending",
	4:  "finished", // Success
	5:  "expired",  // Overdue
	6:  "failed",   // Error
	10: "halted",   // Pause
	11: "halted",   // Stuck
	13: "refunded",
}

func (p *Pegasus) trade(r pegasusTx) (Trade, error) {
	from, okFrom := p.asset(r.Pairs.Deposit.Coin.Name, r.Pairs.Deposit.Coin.Network)
	to, okTo := p.asset(r.Pairs.Receive.Coin.Name, r.Pairs.Receive.Coin.Network)
	if r.OrderNumber == "" || !plausibleID(r.OrderNumber) || !okFrom || !okTo || r.Pairs.Deposit.Address == "" {
		return Trade{}, fmt.Errorf("pegasus: unusable transaction")
	}
	status, ok := pegasusStatus[r.Status]
	if !ok {
		status = "unknown"
	}
	amountTo := string(r.Pairs.Receive.Coin.Value)
	if r.ReceiveTransaction.Amount != "" && status == "finished" {
		amountTo = string(r.ReceiveTransaction.Amount)
	}
	return Trade{
		ID:             p.ID() + "-" + r.OrderNumber,
		Exchange:       p.ID(),
		Name:           p.Name(),
		KYC:            p.KYC(),
		ExchangeID:     r.OrderNumber,
		Support:        p.Support(),
		From:           from,
		To:             to,
		AmountFrom:     string(r.Pairs.Deposit.Coin.Value),
		AmountTo:       amountTo,
		Fixed:          r.Type.TypeSwap == 1,
		DepositAddress: r.Pairs.Deposit.Address,
		DepositMemo:    r.Pairs.Deposit.Memo,
		Address:        r.Pairs.Receive.Address,
		Memo:           r.Pairs.Receive.Memo,
		Refund:         r.RefundAddress,
		Status:         status,
		PayoutTxID:     r.ReceiveTransaction.TxID,
		CreatedAt:      r.CreatedAt,
	}, nil
}

const pegasusMaxResponse = 1 << 20

func (p *Pegasus) call(ctx context.Context, method, endpoint string, params url.Values, body any, out any) error {
	target := p.BaseURL + endpoint
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	mac := hmac.New(sha512.New, []byte(p.Secret))
	mac.Write([]byte(endpoint))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "biscuit-relay")
	req.Header.Set("x-api-public-key", p.PublicKey)
	req.Header.Set("x-api-payload", endpoint)
	req.Header.Set("x-api-signature", hex.EncodeToString(mac.Sum(nil)))

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := p.Client.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("pegasus %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, pegasusMaxResponse+1))
	if err != nil || len(data) > pegasusMaxResponse {
		return fmt.Errorf("pegasus %s: unreadable answer", endpoint)
	}
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("pegasus %s: %w", endpoint, err)
		}
		return nil
	case resp.StatusCode == http.StatusNotFound && endpoint == "get-transaction":
		return errNotFound
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == 422:
		// Amount out of range, invalid address, unavailable pair: their
		// message is meant for the user.
		return &UserError{pegasusMessage(data, resp.StatusCode)}
	default:
		return fmt.Errorf("pegasus %s: HTTP %d", endpoint, resp.StatusCode)
	}
}

// pegasusMessage extracts {"message": "..."} (a string or a list of strings).
func pegasusMessage(data []byte, status int) string {
	var r struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(data, &r) == nil {
		var s string
		var list []string
		if json.Unmarshal(r.Message, &s) == nil && s != "" {
			return clean(s)
		}
		if json.Unmarshal(r.Message, &list) == nil && len(list) > 0 {
			return clean(strings.Join(list, "; "))
		}
	}
	if status == 422 {
		return "this pair is unavailable right now."
	}
	return "the exchange refused the request."
}

// clean keeps an upstream message short and printable.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}
