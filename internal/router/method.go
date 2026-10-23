// Package router implements Wishpull's payment-gateway routing plane
// (PRD v0.3 RP-1, RP-3, EX-2): it chooses which licensed payment gateway
// should serve a payment, estimates the fee, tracks gateway health, and
// decides when it is safe to fall back to another gateway.
//
// The router never moves money. It returns decisions; execution happens in
// paymentsvc under a verified routing token (see internal/routetoken).
package router

import (
	"fmt"
	"strings"
)

// Family groups payment methods that share routing and fee rules.
type Family string

const (
	FamilyQRIS        Family = "qris"
	FamilyVA          Family = "va"
	FamilyDirectDebit Family = "direct_debit"
	FamilyCreditCard  Family = "credit_card"
	FamilyEWallet     Family = "ewallet"
)

// Families lists every family the router knows about.
func Families() []Family {
	return []Family{FamilyQRIS, FamilyVA, FamilyDirectDebit, FamilyCreditCard, FamilyEWallet}
}

// Method is a parsed payment method: a family plus an optional channel
// (bank or wallet code), e.g. {va, mandiri} or {ewallet, ovo}.
type Method struct {
	Family  Family `json:"family"`
	Channel string `json:"channel,omitempty"`
}

// Key renders the method in Wishpull's wire format, e.g. "va_mandiri".
func (m Method) Key() string {
	if m.Channel == "" {
		return string(m.Family)
	}
	return string(m.Family) + "_" + m.Channel
}

// IsPull reports whether the method debits the customer's instrument
// directly (card charge, linked-account debit). Pull methods are never
// retried on another gateway after an ambiguous failure, because the first
// gateway may already have charged the customer.
func (m Method) IsPull() bool {
	return m.Family == FamilyCreditCard || m.Family == FamilyDirectDebit
}

var walletChannels = map[string]struct{}{
	"ovo": {}, "dana": {}, "linkaja": {}, "shopeepay": {}, "gopay": {}, "doku": {},
}

// ParseMethod accepts the PRD wire values ("qris", "va_mandiri",
// "direct_debit_bca", "credit_card") plus common aliases.
func ParseMethod(raw string) (Method, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.ReplaceAll(value, "-", "_")
	if value == "" {
		return Method{}, fmt.Errorf("payment method is required")
	}

	switch value {
	case "qris", "qr":
		return Method{Family: FamilyQRIS}, nil
	case "credit_card", "card", "cc":
		return Method{Family: FamilyCreditCard}, nil
	case "va", "virtual_account", "bank_transfer":
		return Method{}, fmt.Errorf("payment method %q needs a bank, e.g. va_bca or va_mandiri", raw)
	case "direct_debit":
		return Method{}, fmt.Errorf("payment method %q needs a bank, e.g. direct_debit_bca", raw)
	case "ewallet", "e_wallet":
		return Method{}, fmt.Errorf("payment method %q needs a wallet, e.g. ewallet_ovo", raw)
	}

	for _, prefix := range []string{"virtual_account_", "bank_transfer_", "va_"} {
		if channel, ok := strings.CutPrefix(value, prefix); ok && channel != "" {
			return Method{Family: FamilyVA, Channel: normalizeBank(channel)}, nil
		}
	}
	if channel, ok := strings.CutPrefix(value, "direct_debit_"); ok && channel != "" {
		return Method{Family: FamilyDirectDebit, Channel: normalizeBank(channel)}, nil
	}
	for _, prefix := range []string{"ewallet_", "e_wallet_"} {
		if channel, ok := strings.CutPrefix(value, prefix); ok && channel != "" {
			return Method{Family: FamilyEWallet, Channel: normalizeWallet(channel)}, nil
		}
	}
	if _, ok := walletChannels[normalizeWallet(value)]; ok {
		return Method{Family: FamilyEWallet, Channel: normalizeWallet(value)}, nil
	}

	return Method{}, fmt.Errorf("payment method %q is not supported", raw)
}

func normalizeBank(value string) string {
	switch value {
	case "bank_mandiri", "mandiri_bill", "echannel":
		return "mandiri"
	default:
		return value
	}
}

func normalizeWallet(value string) string {
	switch value {
	case "shopee_pay":
		return "shopeepay"
	default:
		return value
	}
}
