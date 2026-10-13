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
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Stereoswap (https://api.stereoswap.app/partner/swagger): never asks for
// KYC; deposits scored as high-risk are sent back to the refund address. The
// partner API needs the account's API key (X-API-Key) for everything, and a
// referral code of that account to create swaps, which earns the partner
// share. A swap is read back by its UUID, kept by the app in the swap ID
// ("stereoswap-<uuid>").
type Stereoswap struct {
	BaseURL  string // "https://api.stereoswap.app/partner/v1/"
	APIKey   string
	Referral string
	Client   *http.Client
}

// Stereoswap coin token and network title of each asset, as
// coin-network/list gives them (checked 2026-10-13). No USDC on Ethereum,
// ZEC, XRP, ADA or XLM.
var stereoswapAssets = map[Asset][2]string{
	{"xmr", "Mainnet"}:  {"XMR", "XMR"},
	{"btc", "Mainnet"}:  {"BTC", "BTC"},
	{"ltc", "Mainnet"}:  {"LTC", "LTC"},
	{"eth", "ERC20"}:    {"ETH", "ETH"},
	{"usdt", "ERC20"}:   {"USDT", "ETH"},
	{"usdt", "TRC20"}:   {"USDT", "TRX"},
	{"sol", "Mainnet"}:  {"SOL", "SOL"},
	{"trx", "Mainnet"}:  {"TRX", "TRX"},
	{"doge", "Mainnet"}: {"DOGE", "DOGE"},
	{"bnb", "BEP20"}:    {"BNB", "BSC"},
	{"bch", "Mainnet"}:  {"BCH", "BCH"},
	{"usdc", "SOL"}:     {"USDC", "SOL"},
}

func (s *Stereoswap) ID() string      { return "stereoswap" }
func (s *Stereoswap) Name() string    { return "Stereoswap" }
func (s *Stereoswap) KYC() string     { return "A" }
func (s *Stereoswap) Support() string { return "https://stereoswap.app/en/faq" }

func (s *Stereoswap) Supports(a Asset) bool {
	_, ok := stereoswapAssets[a]
	return ok
}

// Float: the amount sent ("deposit"). Fixed: the exact amount received
// ("receive"), and the answer says how much to send.
func stereoswapRequest(q QuoteRequest) (map[string]any, error) {
	from, to := stereoswapAssets[q.From], stereoswapAssets[q.To]
	amount, err := json.Marshal(json.Number(q.Amount))
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"amount":       json.RawMessage(amount),
		"last_source":  "deposit",
		"type_swap":    2,
		"mode":         "standard",
		"from_coin":    from[0],
		"from_network": from[1],
		"to_coin":      to[0],
		"to_network":   to[1],
	}
	if q.Fixed {
		body["last_source"] = "receive"
		body["type_swap"] = 1
	}
	return body, nil
}

func (s *Stereoswap) Quote(ctx context.Context, q QuoteRequest) (Quote, error) {
	body, err := stereoswapRequest(q)
	if err != nil {
		return Quote{}, err
	}
	var r struct {
		Amount        decimal `json:"amount"`
		ReceiveAmount decimal `json:"receive_amount"`
		MinAmount     decimal `json:"min_amount"`
		MaxAmount     decimal `json:"max_amount"`
	}
	if err := s.call(ctx, http.MethodPost, "exchange/calculate/", body, q.From, &r); err != nil {
		return Quote{}, err
	}
	quote := Quote{Exchange: s.ID(), Name: s.Name(), KYC: s.KYC(), Fixed: q.Fixed,
		AmountFrom: string(r.Amount), AmountTo: string(r.ReceiveAmount),
		Min: string(r.MinAmount), Max: string(r.MaxAmount)}
	if !positiveDecimal(quote.AmountFrom) || !positiveDecimal(quote.AmountTo) {
		return Quote{}, fmt.Errorf("stereoswap: unusable quote %+v", r)
	}
	if msg := outOfRange(quote.AmountFrom, quote.Min, quote.Max, q.From); msg != "" {
		return Quote{}, &UserError{msg}
	}
	return quote, nil
}

func (s *Stereoswap) Create(ctx context.Context, t TradeRequest) (Trade, error) {
	if s.Referral == "" {
		return Trade{}, fmt.Errorf("stereoswap: no referral code")
	}
	body, err := stereoswapRequest(t.QuoteRequest)
	if err != nil {
		return Trade{}, err
	}
	body["referral_code"] = s.Referral
	body["address"] = t.Address
	if t.Memo != "" {
		body["memo"] = t.Memo
	}
	if t.Refund != "" {
		body["refund_address"] = t.Refund
	}
	var r stereoswapExchange
	if err := s.call(ctx, http.MethodPost, "exchange/create/", body, t.From, &r); err != nil {
		return Trade{}, err
	}
	return s.trade(r)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (s *Stereoswap) Status(ctx context.Context, exchangeID string) (Trade, error) {
	id := strings.ToLower(exchangeID)
	if !uuidPattern.MatchString(id) {
		return Trade{}, errNotFound
	}
	var r stereoswapExchange
	if err := s.call(ctx, http.MethodGet, "exchange/"+id, nil, Asset{}, &r); err != nil {
		return Trade{}, err
	}
	return s.trade(r)
}

type stereoswapOperation struct {
	CoinNetwork struct {
		Coin struct {
			Token string `json:"token"`
		} `json:"coin"`
		Network struct {
			Title string `json:"title"`
		} `json:"network"`
	} `json:"coin_network"`
	Amount  decimal `json:"amount"`
	Address string  `json:"address"`
	Memo    string  `json:"memo"`
	TxID    string  `json:"txid"`
}

type stereoswapExchange struct {
	ID            string               `json:"id"`
	OrderHash     string               `json:"order_hash"`
	Status        int                  `json:"status"`
	TypeSwap      int                  `json:"type_swap"`
	RefundAddress string               `json:"refund_address"`
	CreatedAt     string               `json:"created_at"`
	Deposit       *stereoswapOperation `json:"deposit"`
	Withdrawal    *stereoswapOperation `json:"withdrawal"`
}

// Stereoswap statuses, in the app's names.
var stereoswapStatus = map[int]string{
	9:  "waiting",    // New: waiting for the deposit
	1:  "confirming", // Confirmation
	3:  "sending",    // Exchanging
	4:  "finished",   // Success
	12: "finished",   // Success
	5:  "expired",    // Overdue
	6:  "failed",     // Error
	10: "halted",     // Pause
	11: "halted",     // Frozen
	13: "refunded",   // Refund
}

func (s *Stereoswap) asset(op *stereoswapOperation) (Asset, bool) {
	if op == nil {
		return Asset{}, false
	}
	for a, code := range stereoswapAssets {
		if strings.EqualFold(code[0], op.CoinNetwork.Coin.Token) && strings.EqualFold(code[1], op.CoinNetwork.Network.Title) {
			return a, true
		}
	}
	return Asset{}, false
}

func (s *Stereoswap) trade(r stereoswapExchange) (Trade, error) {
	from, okFrom := s.asset(r.Deposit)
	to, okTo := s.asset(r.Withdrawal)
	id := strings.ToLower(r.ID)
	if !uuidPattern.MatchString(id) || !okFrom || !okTo || r.Deposit.Address == "" {
		return Trade{}, fmt.Errorf("stereoswap: unusable exchange")
	}
	status, ok := stereoswapStatus[r.Status]
	if !ok {
		status = "unknown"
	}
	exchangeID := r.OrderHash // what their support asks for
	if exchangeID == "" {
		exchangeID = id
	}
	return Trade{
		ID:             s.ID() + "-" + id,
		Exchange:       s.ID(),
		Name:           s.Name(),
		KYC:            s.KYC(),
		ExchangeID:     exchangeID,
		Support:        s.Support(),
		From:           from,
		To:             to,
		AmountFrom:     string(r.Deposit.Amount),
		AmountTo:       string(r.Withdrawal.Amount),
		Fixed:          r.TypeSwap == 1,
		DepositAddress: r.Deposit.Address,
		DepositMemo:    r.Deposit.Memo,
		Address:        r.Withdrawal.Address,
		Memo:           r.Withdrawal.Memo,
		Refund:         r.RefundAddress,
		Status:         status,
		PayoutTxID:     r.Withdrawal.TxID,
		CreatedAt:      r.CreatedAt,
	}, nil
}

const stereoswapMaxResponse = 1 << 20

var stereoswapLimit = regexp.MustCompile(`(?i)(minimum exchange amount is|max amount)\s*([0-9]+(?:\.[0-9]+)?)`)

// call answers data into out. sent is the coin sent, to word "the minimum
// is…" (zero for status calls).
func (s *Stereoswap) call(ctx context.Context, method, endpoint string, body any, sent Asset, out any) error {
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
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "biscuit-relay")
	req.Header.Set("X-API-Key", s.APIKey)
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("stereoswap %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, stereoswapMaxResponse+1))
	if err != nil || len(data) > stereoswapMaxResponse {
		return fmt.Errorf("stereoswap %s: unreadable answer", endpoint)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("stereoswap %s: %w", endpoint, err)
		}
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	// {"message": "Validation failed", "errors": [{"key": "amount", "message": "Minimum exchange amount is 0.06."}]}
	var r struct {
		Errors []struct {
			Key     string `json:"key"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if resp.StatusCode == http.StatusUnprocessableEntity && json.Unmarshal(data, &r) == nil && len(r.Errors) > 0 {
		e := r.Errors[0]
		if m := stereoswapLimit.FindStringSubmatch(e.Message); m != nil && sent.Ticker != "" {
			if strings.HasPrefix(strings.ToLower(m[1]), "minimum") {
				return &UserError{fmt.Sprintf("the minimum is %s %s.", roundAmount(m[2], sent, true), strings.ToUpper(sent.Ticker))}
			}
			return &UserError{fmt.Sprintf("the maximum is %s %s.", roundAmount(m[2], sent, false), strings.ToUpper(sent.Ticker))}
		}
		if e.Key == "address" || e.Key == "refund_address" || e.Key == "memo" {
			return &UserError{clean(e.Message)}
		}
	}
	return errors.New("stereoswap " + endpoint + ": HTTP " + fmt.Sprint(resp.StatusCode))
}
