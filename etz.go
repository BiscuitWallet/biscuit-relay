// SPDX-License-Identifier: BSD-3-Clause
// SPDX-FileCopyrightText: The Biscuit developers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ETZ-Swap (https://api.etz-swap.com/v3/api-docs): never asks for KYC, runs
// its own liquidity. The public routes work without a key; with partner keys
// (X-API-KEY, X-API-SECRET-KEY, X-API-KEY-VERSION) swaps earn the affiliate
// share. Swaps are created with the "secureToken" security type: only whoever
// holds the token can read them. The token travels inside the swap ID the
// app keeps ("etz-<id>.<token>"), the relay stores nothing.
type ETZ struct {
	BaseURL    string // "https://api.etz-swap.com/api/v1/deposit/public/"
	PublicKey  string // optional
	Secret     string
	KeyVersion string
	Client     *http.Client
}

// ETZ coin and network of each asset (checked 2026-10-12, outside coins
// 2026-10-13 in public/coins). No ZEC, TON or BCH.
var etzAssets = map[Asset][2]string{
	{"xmr", "Mainnet"}:  {"XMR", "XMR"},
	{"btc", "Mainnet"}:  {"BTC", "BTC"},
	{"ltc", "Mainnet"}:  {"LTC", "LTC"},
	{"eth", "ERC20"}:    {"ETH", "ETH"},
	{"usdt", "ERC20"}:   {"USDT", "ETH"},
	{"usdc", "ERC20"}:   {"USDC", "ETH"},
	{"usdt", "TRC20"}:   {"USDT", "TRX"},
	{"sol", "Mainnet"}:  {"SOL", "SOL"},
	{"xrp", "Mainnet"}:  {"XRP", "XRP"},
	{"trx", "Mainnet"}:  {"TRX", "TRX"},
	{"doge", "Mainnet"}: {"DOGE", "DOGE"},
	{"ada", "Mainnet"}:  {"ADA", "ADA"},
	{"xlm", "Mainnet"}:  {"XLM", "XLM"},
	{"bnb", "BEP20"}:    {"BNB", "BSC"},
	{"usdc", "SOL"}:     {"USDC", "SOL"},
}

func (e *ETZ) ID() string      { return "etz" }
func (e *ETZ) Name() string    { return "ETZ-Swap" }
func (e *ETZ) KYC() string     { return "A" }
func (e *ETZ) Support() string { return "https://etz-swap.com" }

func (e *ETZ) Supports(a Asset) bool {
	_, ok := etzAssets[a]
	return ok
}

func rateType(fixed bool) string {
	if fixed {
		return "fixed"
	}
	return "float"
}

func (e *ETZ) Quote(ctx context.Context, q QuoteRequest) (Quote, error) {
	from, to := etzAssets[q.From], etzAssets[q.To]
	params := url.Values{
		"coinFrom": {from[0]}, "networkFrom": {from[1]},
		"coinTo": {to[0]}, "networkTo": {to[1]},
		"rateType": {rateType(q.Fixed)},
	}
	if q.Fixed {
		params.Set("amountTo", q.Amount)
	} else {
		params.Set("amountFrom", q.Amount)
	}
	var r struct {
		AmountFrom    decimal `json:"amountFrom"`
		AmountTo      decimal `json:"amountTo"`
		MinAmountFrom decimal `json:"minAmountFrom"`
		MaxAmountFrom decimal `json:"maxAmountFrom"`
	}
	if err := e.call(ctx, http.MethodGet, "rate", params, nil, q.From, &r); err != nil {
		return Quote{}, err
	}
	quote := Quote{Exchange: e.ID(), Name: e.Name(), KYC: e.KYC(), Fixed: q.Fixed,
		AmountFrom: string(r.AmountFrom), AmountTo: string(r.AmountTo),
		Min: string(r.MinAmountFrom), Max: string(r.MaxAmountFrom)}
	if !positiveDecimal(quote.AmountFrom) || !positiveDecimal(quote.AmountTo) {
		return Quote{}, fmt.Errorf("etz: unusable quote %+v", r)
	}
	if msg := outOfRange(quote.AmountFrom, quote.Min, quote.Max, q.From); msg != "" {
		return Quote{}, &UserError{msg}
	}
	return quote, nil
}

func (e *ETZ) Create(ctx context.Context, t TradeRequest) (Trade, error) {
	from, to := etzAssets[t.From], etzAssets[t.To]
	amount, err := json.Marshal(json.Number(t.Amount))
	if err != nil {
		return Trade{}, err
	}
	body := map[string]any{
		"coinFrom": from[0], "networkFrom": from[1],
		"coinTo": to[0], "networkTo": to[1],
		"withdrawalAddress": t.Address,
		"rateType":          rateType(t.Fixed),
		"securityType":      "secureToken",
	}
	// Fixed rate: the exact amount received; ETZ says how much to send.
	if t.Fixed {
		body["amountTo"] = json.RawMessage(amount)
	} else {
		body["amountFrom"] = json.RawMessage(amount)
	}
	if t.Memo != "" {
		body["withdrawalMemo"] = t.Memo
	}
	if t.Refund != "" {
		body["refundAddress"] = t.Refund
		if t.RefundMemo != "" {
			body["refundMemo"] = t.RefundMemo
		}
	}
	var r etzTx
	if err := e.call(ctx, http.MethodPost, "transaction", nil, body, t.From, &r); err != nil {
		return Trade{}, err
	}
	if r.TransactionSecurity.SecureToken == "" {
		return Trade{}, fmt.Errorf("etz: no secure token")
	}
	return e.trade(r, r.TransactionSecurity.SecureToken)
}

func (e *ETZ) Status(ctx context.Context, exchangeID string) (Trade, error) {
	id, token, _ := strings.Cut(exchangeID, ".")
	if len(id) > 64 || !plausibleID(id) || !plausibleToken(token) {
		return Trade{}, errNotFound
	}
	var r etzTx
	if err := e.call(ctx, http.MethodGet, "transactions/"+id, url.Values{"secureToken": {token}}, nil, Asset{}, &r); err != nil {
		return Trade{}, err
	}
	return e.trade(r, token)
}

// plausibleToken: a UUID-like secure token.
func plausibleToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

type etzAddress struct {
	Address string `json:"address"`
	Memo    string `json:"memo"`
}

type etzCoin struct {
	CoinCode string `json:"coinCode"`
	Network  string `json:"network"`
}

type etzTx struct {
	TransactionID    string     `json:"transactionId"`
	Amount           decimal    `json:"amount"`
	AmountTo         decimal    `json:"amountTo"`
	CoinFrom         etzCoin    `json:"coinFrom"`
	CoinTo           etzCoin    `json:"coinTo"`
	Deposit          etzAddress `json:"deposit"`
	Withdraw         etzAddress `json:"withdraw"`
	Refund           etzAddress `json:"refund"`
	OutboundTransfer struct {
		Hash string `json:"hash"`
	} `json:"outboundTransfer"`
	CreatedAt           string `json:"createdAt"`
	RateType            string `json:"rateType"`
	Status              string `json:"status"`
	TransactionSecurity struct {
		SecureToken string `json:"secureToken"`
	} `json:"transactionSecurity"`
	AmountsInfo struct {
		SentWithdrawalAmount decimal `json:"sentWithdrawalAmount"`
	} `json:"amountsInfo"`
}

// ETZ statuses, in the app's names.
var etzStatus = map[string]string{
	"deposit":       "waiting",
	"confirmations": "confirming",
	"confirmed":     "confirming",
	"exchanging":    "confirming",
	"sending":       "sending",
	"success":       "finished",
	"overdue":       "expired",
	"refunded":      "refunded",
}

func (e *ETZ) asset(c etzCoin) (Asset, bool) {
	for a, code := range etzAssets {
		if strings.EqualFold(code[0], c.CoinCode) && strings.EqualFold(code[1], c.Network) {
			return a, true
		}
	}
	return Asset{}, false
}

func (e *ETZ) trade(r etzTx, token string) (Trade, error) {
	from, okFrom := e.asset(r.CoinFrom)
	to, okTo := e.asset(r.CoinTo)
	if r.TransactionID == "" || !plausibleID(r.TransactionID) || !plausibleToken(token) || !okFrom || !okTo || r.Deposit.Address == "" {
		return Trade{}, fmt.Errorf("etz: unusable transaction")
	}
	status, ok := etzStatus[r.Status]
	if !ok {
		status = "unknown"
	}
	amountTo := string(r.AmountTo)
	if status == "finished" && r.AmountsInfo.SentWithdrawalAmount != "" {
		amountTo = string(r.AmountsInfo.SentWithdrawalAmount)
	}
	return Trade{
		ID:             e.ID() + "-" + r.TransactionID + "." + token,
		Exchange:       e.ID(),
		Name:           e.Name(),
		KYC:            e.KYC(),
		ExchangeID:     r.TransactionID,
		Support:        e.Support(),
		From:           from,
		To:             to,
		AmountFrom:     string(r.Amount),
		AmountTo:       amountTo,
		Fixed:          r.RateType == "fixed",
		DepositAddress: r.Deposit.Address,
		DepositMemo:    r.Deposit.Memo,
		Address:        r.Withdraw.Address,
		Memo:           r.Withdraw.Memo,
		Refund:         r.Refund.Address,
		Status:         status,
		PayoutTxID:     r.OutboundTransfer.Hash,
		CreatedAt:      r.CreatedAt,
	}, nil
}

const etzMaxResponse = 1 << 20

// call answers data into out. sent is the coin sent, to word "the minimum
// is…" (zero for status calls).
func (e *ETZ) call(ctx context.Context, method, endpoint string, params url.Values, body any, sent Asset, out any) error {
	target := e.BaseURL + endpoint
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
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "biscuit-relay")
	if e.PublicKey != "" {
		req.Header.Set("X-API-KEY", e.PublicKey)
		req.Header.Set("X-API-SECRET-KEY", e.Secret)
		req.Header.Set("X-API-KEY-VERSION", e.KeyVersion)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("etz %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, etzMaxResponse+1))
	if err != nil || len(data) > etzMaxResponse {
		return fmt.Errorf("etz %s: unreadable answer", endpoint)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors map[string]struct {
			Message string                     `json:"message"`
			Args    map[string]json.RawMessage `json:"args"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("etz %s: HTTP %d", endpoint, resp.StatusCode)
	}
	if len(envelope.Errors) > 0 {
		codes := make([]string, 0, len(envelope.Errors))
		for code := range envelope.Errors {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		code := codes[0]
		switch code {
		case "transactionNotExist", "transactionSecurityRestrictedAccess", "transactionSecurityDisposed", "transactionSecuritySecureTokenMissed":
			return errNotFound
		case "amountTooSmall", "amountTooBig":
			var limit decimal
			key := "minAllowedAmount"
			if code == "amountTooBig" {
				key = "maxAllowedAmount"
			}
			if raw, ok := envelope.Errors[code].Args[key]; ok && json.Unmarshal(raw, &limit) == nil && limit != "" && sent.Ticker != "" {
				if code == "amountTooSmall" {
					return &UserError{fmt.Sprintf("the minimum is %s %s.", roundAmount(string(limit), sent, true), strings.ToUpper(sent.Ticker))}
				}
				return &UserError{fmt.Sprintf("the maximum is %s %s.", roundAmount(string(limit), sent, false), strings.ToUpper(sent.Ticker))}
			}
			return &UserError{clean(envelope.Errors[code].Message)}
		case "notAuthenticated":
			return fmt.Errorf("etz %s: not authenticated", endpoint)
		}
		if resp.StatusCode == http.StatusBadRequest {
			return &UserError{clean(envelope.Errors[code].Message) + "."}
		}
		return fmt.Errorf("etz %s: %s", endpoint, code)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("etz %s: HTTP %d", endpoint, resp.StatusCode)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("etz %s: %w", endpoint, err)
	}
	return nil
}
