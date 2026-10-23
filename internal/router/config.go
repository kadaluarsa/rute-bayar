package router

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

// ExecutionMode tells the caller what the customer will see.
type ExecutionMode string

const (
	// ModeDirect returns a payable instrument (VA number, QR string, charge).
	ModeDirect ExecutionMode = "direct"
	// ModeHosted returns a redirect URL to the gateway's own page, which the
	// Wishpull app renders in a secure web view (the Trusted Surface).
	ModeHosted ExecutionMode = "hosted"
)

// Optimize selects how candidates for a rule are ordered.
type Optimize string

const (
	OptimizeLowestFee Optimize = "lowest_fee"
	OptimizePriority  Optimize = "priority"
)

// Support declares what an adapter in this repository can actually execute.
// It is code, not config: routing to a gateway the adapter cannot drive
// would fail at execution time.
type Support struct {
	Provider domain.ProviderCode
	Family   Family
	// Channels the adapter accepts; empty means any channel.
	Channels []string
	Mode     ExecutionMode
	// RequiresClientToken is set when the charge needs a token produced on
	// the device by the gateway's own SDK (e.g. Midtrans card tokens).
	// Such tokens are gateway-specific, so they also block fallback.
	RequiresClientToken bool
	// ChannelEnforced is false for hosted pages that let the customer pick
	// another method; fee estimates are then indicative only.
	ChannelEnforced bool
	// AdapterMethod and AdapterChannel translate the route into the
	// provider.CreatePaymentRequest fields the adapter expects.
	AdapterMethod  string
	AdapterChannel func(channel string) string
}

func (s Support) supportsChannel(channel string) bool {
	if len(s.Channels) == 0 || channel == "" {
		return true
	}
	for _, c := range s.Channels {
		if c == channel {
			return true
		}
	}
	return false
}

func identity(channel string) string { return channel }

// DefaultSupport mirrors the adapters shipped in internal/provider.
func DefaultSupport() []Support {
	return []Support{
		{Provider: domain.ProviderMidtrans, Family: FamilyVA, Channels: []string{"bca", "bni", "bri", "permata", "cimb", "mandiri"}, Mode: ModeDirect, ChannelEnforced: true, AdapterMethod: "bank_transfer", AdapterChannel: identity},
		{Provider: domain.ProviderMidtrans, Family: FamilyQRIS, Mode: ModeDirect, ChannelEnforced: true, AdapterMethod: "qris", AdapterChannel: func(string) string { return "" }},
		{Provider: domain.ProviderMidtrans, Family: FamilyCreditCard, Mode: ModeDirect, RequiresClientToken: true, ChannelEnforced: true, AdapterMethod: "credit_card", AdapterChannel: identity},

		{Provider: domain.ProviderDoku, Family: FamilyVA, Channels: []string{"bca", "mandiri", "bri", "bni", "permata", "cimb", "danamon", "maybank", "btn", "bnc", "sinarmas"}, Mode: ModeHosted, ChannelEnforced: true, AdapterMethod: "va", AdapterChannel: identity},
		{Provider: domain.ProviderDoku, Family: FamilyQRIS, Mode: ModeHosted, ChannelEnforced: true, AdapterMethod: "qris", AdapterChannel: identity},
		{Provider: domain.ProviderDoku, Family: FamilyCreditCard, Mode: ModeHosted, ChannelEnforced: true, AdapterMethod: "credit_card", AdapterChannel: identity},
		{Provider: domain.ProviderDoku, Family: FamilyEWallet, Channels: []string{"ovo", "dana", "linkaja", "shopeepay", "doku"}, Mode: ModeHosted, ChannelEnforced: true, AdapterMethod: "ewallet", AdapterChannel: identity},

		{Provider: domain.ProviderIPaymu, Family: FamilyVA, Channels: []string{"bca", "bni", "bri", "mandiri", "permata", "cimb", "bsi", "muamalat"}, Mode: ModeDirect, ChannelEnforced: true, AdapterMethod: "va", AdapterChannel: identity},
		{Provider: domain.ProviderIPaymu, Family: FamilyQRIS, Mode: ModeDirect, ChannelEnforced: true, AdapterMethod: "qris", AdapterChannel: func(string) string { return "qris" }},

		// The Xendit adapter drives Payment Sessions (hosted payment link):
		// the customer completes VA, QRIS, e-wallet or direct debit on
		// Xendit's page. Native linked-account direct debit is not built yet.
		{Provider: domain.ProviderXendit, Family: FamilyVA, Mode: ModeHosted, AdapterMethod: "payment_link", AdapterChannel: func(string) string { return "" }},
		{Provider: domain.ProviderXendit, Family: FamilyQRIS, Mode: ModeHosted, AdapterMethod: "payment_link", AdapterChannel: func(string) string { return "" }},
		{Provider: domain.ProviderXendit, Family: FamilyEWallet, Mode: ModeHosted, AdapterMethod: "payment_link", AdapterChannel: func(string) string { return "" }},
		{Provider: domain.ProviderXendit, Family: FamilyDirectDebit, Mode: ModeHosted, AdapterMethod: "payment_link", AdapterChannel: func(string) string { return "" }},
		{Provider: domain.ProviderXendit, Family: FamilyCreditCard, Mode: ModeHosted, AdapterMethod: "payment_link", AdapterChannel: func(string) string { return "" }},
	}
}

// FeeRule is a gateway's MDR for one family: a percentage in basis points
// (70 = 0.70%) plus a fixed amount in IDR.
type FeeRule struct {
	BasisPoints int64 `json:"basis_points"`
	FixedIDR    int64 `json:"fixed_idr"`
}

// Fee returns the fee for amount, rounding the percentage part up to a
// whole rupiah so the estimate never undercharges.
func (f FeeRule) Fee(amount int64) int64 {
	if amount <= 0 {
		return f.FixedIDR
	}
	return (amount*f.BasisPoints+9999)/10000 + f.FixedIDR
}

// Rule orders the candidate gateways for one family (optionally one channel).
type Rule struct {
	Family     Family                `json:"family"`
	Channel    string                `json:"channel,omitempty"`
	Candidates []domain.ProviderCode `json:"candidates"`
	Optimize   Optimize              `json:"optimize"`
}

// Config is the operator-tunable part of the router.
type Config struct {
	Rules []Rule `json:"rules"`
	// Fees[provider][family]. Contracted MDRs differ per merchant: the
	// defaults are indicative placeholders and must be overridden.
	Fees map[domain.ProviderCode]map[Family]FeeRule `json:"fees"`
	// HandlingFeeBasisPoints/HandlingFeeFixedIDR model Wishpull's own fee on
	// top of the gateway MDR (PRD v0.3 open question); zero by default.
	HandlingFeeBasisPoints int64 `json:"handling_fee_basis_points"`
	HandlingFeeFixedIDR    int64 `json:"handling_fee_fixed_idr"`

	// LatencyBudget is the per-attempt timeout for push methods before the
	// router may fall back (PRD EX-2: 3000 ms).
	LatencyBudget time.Duration `json:"-"`
	// PullMethodTimeout is the per-attempt timeout for pull methods, which
	// never fall back after a timeout.
	PullMethodTimeout time.Duration `json:"-"`
	Breaker           BreakerConfig `json:"-"`

	LatencyBudgetMs     int64 `json:"latency_budget_ms"`
	PullMethodTimeoutMs int64 `json:"pull_method_timeout_ms"`
	BreakerFailures     int   `json:"breaker_failure_threshold"`
	BreakerOpenMs       int64 `json:"breaker_open_ms"`
}

// DefaultConfig follows PRD v0.3 §7, adjusted to what the adapters support.
func DefaultConfig() Config {
	return Config{
		Rules: []Rule{
			{Family: FamilyQRIS, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderXendit, domain.ProviderDoku, domain.ProviderIPaymu}, Optimize: OptimizeLowestFee},
			{Family: FamilyVA, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderXendit, domain.ProviderDoku, domain.ProviderIPaymu}, Optimize: OptimizePriority},
			{Family: FamilyDirectDebit, Candidates: []domain.ProviderCode{domain.ProviderXendit, domain.ProviderDoku}, Optimize: OptimizePriority},
			{Family: FamilyCreditCard, Candidates: []domain.ProviderCode{domain.ProviderDoku, domain.ProviderMidtrans, domain.ProviderXendit}, Optimize: OptimizePriority},
			{Family: FamilyEWallet, Candidates: []domain.ProviderCode{domain.ProviderDoku, domain.ProviderXendit}, Optimize: OptimizeLowestFee},
		},
		Fees: map[domain.ProviderCode]map[Family]FeeRule{
			domain.ProviderMidtrans: {FamilyQRIS: {BasisPoints: 70}, FamilyVA: {FixedIDR: 4000}, FamilyCreditCard: {BasisPoints: 290, FixedIDR: 2000}},
			domain.ProviderXendit:   {FamilyQRIS: {BasisPoints: 63}, FamilyVA: {FixedIDR: 4500}, FamilyEWallet: {BasisPoints: 150}, FamilyDirectDebit: {BasisPoints: 190}, FamilyCreditCard: {BasisPoints: 290, FixedIDR: 2000}},
			domain.ProviderDoku:     {FamilyQRIS: {BasisPoints: 70}, FamilyVA: {FixedIDR: 4500}, FamilyEWallet: {BasisPoints: 150}, FamilyCreditCard: {BasisPoints: 280, FixedIDR: 2500}},
			domain.ProviderIPaymu:   {FamilyQRIS: {BasisPoints: 70}, FamilyVA: {FixedIDR: 4000}},
		},
		LatencyBudget:     3000 * time.Millisecond,
		PullMethodTimeout: 20 * time.Second,
		Breaker:           DefaultBreakerConfig(),
	}
}

// LoadConfig reads a JSON routing config and overlays it on the defaults.
// An empty path returns DefaultConfig.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	path = strings.TrimSpace(path)
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read routing config: %w", err)
	}
	var overlay Config
	if err := json.Unmarshal(raw, &overlay); err != nil {
		return Config{}, fmt.Errorf("parse routing config: %w", err)
	}
	if len(overlay.Rules) > 0 {
		cfg.Rules = overlay.Rules
	}
	for providerCode, fees := range overlay.Fees {
		if cfg.Fees[providerCode] == nil {
			cfg.Fees[providerCode] = map[Family]FeeRule{}
		}
		for family, rule := range fees {
			cfg.Fees[providerCode][family] = rule
		}
	}
	cfg.HandlingFeeBasisPoints = overlay.HandlingFeeBasisPoints
	cfg.HandlingFeeFixedIDR = overlay.HandlingFeeFixedIDR
	if overlay.LatencyBudgetMs > 0 {
		cfg.LatencyBudget = time.Duration(overlay.LatencyBudgetMs) * time.Millisecond
	}
	if overlay.PullMethodTimeoutMs > 0 {
		cfg.PullMethodTimeout = time.Duration(overlay.PullMethodTimeoutMs) * time.Millisecond
	}
	if overlay.BreakerFailures > 0 {
		cfg.Breaker.FailureThreshold = overlay.BreakerFailures
	}
	if overlay.BreakerOpenMs > 0 {
		cfg.Breaker.OpenFor = time.Duration(overlay.BreakerOpenMs) * time.Millisecond
	}
	cfg.Breaker.SlowCall = cfg.LatencyBudget
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configs that reference unknown providers or families.
func (c Config) Validate() error {
	known := map[domain.ProviderCode]bool{}
	for _, p := range domain.SupportedProviders() {
		known[p] = true
	}
	families := map[Family]bool{}
	for _, f := range Families() {
		families[f] = true
	}
	for _, rule := range c.Rules {
		if !families[rule.Family] {
			return fmt.Errorf("routing rule has unknown family %q", rule.Family)
		}
		if rule.Optimize != OptimizeLowestFee && rule.Optimize != OptimizePriority {
			return fmt.Errorf("routing rule for %q has unknown optimize %q", rule.Family, rule.Optimize)
		}
		if len(rule.Candidates) == 0 {
			return fmt.Errorf("routing rule for %q has no candidates", rule.Family)
		}
		for _, p := range rule.Candidates {
			if !known[p] {
				return fmt.Errorf("routing rule for %q references unknown provider %q", rule.Family, p)
			}
		}
	}
	for p, fees := range c.Fees {
		if !known[p] {
			return fmt.Errorf("fee table references unknown provider %q", p)
		}
		for f, rule := range fees {
			if !families[f] {
				return fmt.Errorf("fee table for %q has unknown family %q", p, f)
			}
			if rule.BasisPoints < 0 || rule.FixedIDR < 0 {
				return fmt.Errorf("fee table for %q/%q has negative values", p, f)
			}
		}
	}
	return nil
}
