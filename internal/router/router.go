package router

import (
	"context"
	"fmt"
	"sort"

	"github.com/pendig/rute-bayar/internal/domain"
)

// AvailabilityFunc reports whether a gateway has credentials configured for
// the environment. Unconfigured gateways are never recommended.
type AvailabilityFunc func(ctx context.Context, code domain.ProviderCode, env domain.Environment) bool

// Router turns a payment method and amount into an ordered gateway plan.
type Router struct {
	cfg       Config
	support   []Support
	health    *Health
	available AvailabilityFunc
}

func New(cfg Config, health *Health, available AvailabilityFunc) *Router {
	if health == nil {
		health = NewHealth(cfg.Breaker)
	}
	return &Router{cfg: cfg, support: DefaultSupport(), health: health, available: available}
}

func (r *Router) Config() Config  { return r.cfg }
func (r *Router) Health() *Health { return r.health }

// QuoteInput mirrors GetPaymentRouteArgs from TRD v0.3 §3.1.
type QuoteInput struct {
	Amount      int64
	Currency    string
	Method      string
	Environment domain.Environment
	// HasClientToken is true when the device already produced a
	// gateway-specific token (e.g. a Midtrans card token) for TokenProvider.
	HasClientToken bool
	TokenProvider  domain.ProviderCode
	// Exclude removes gateways, e.g. ones that already failed for this order.
	Exclude []domain.ProviderCode
}

// Candidate is one gateway in the plan.
type Candidate struct {
	Provider       domain.ProviderCode `json:"provider"`
	Mode           ExecutionMode       `json:"mode"`
	GatewayFee     int64               `json:"gateway_fee"`
	HandlingFee    int64               `json:"handling_fee"`
	EstimatedFee   int64               `json:"estimated_fee"`
	FeeIndicative  bool                `json:"fee_indicative"`
	Status         GatewayStatus       `json:"gateway_status"`
	AdapterMethod  string              `json:"-"`
	AdapterChannel string              `json:"-"`
	NeedsClientTok bool                `json:"requires_client_token"`
	Requires       []string            `json:"requires,omitempty"`
	Support        Support             `json:"-"`
}

// Rejection explains why a gateway was left out of the plan.
type Rejection struct {
	Provider domain.ProviderCode `json:"provider"`
	Reason   string              `json:"reason"`
}

// Decision is the router's answer (RP-1).
type Decision struct {
	Method      Method      `json:"method"`
	Amount      int64       `json:"amount"`
	Currency    string      `json:"currency"`
	Recommended Candidate   `json:"recommended"`
	Fallbacks   []Candidate `json:"fallbacks"`
	Rejected    []Rejection `json:"rejected,omitempty"`
	// FallbackPolicy states when Fallbacks may be used at execution time.
	FallbackPolicy string `json:"fallback_policy"`
}

// Plan returns Recommended followed by Fallbacks.
func (d Decision) Plan() []Candidate {
	return append([]Candidate{d.Recommended}, d.Fallbacks...)
}

// ErrNoRoute is returned when no gateway can serve the request.
type ErrNoRoute struct {
	Method   string
	Rejected []Rejection
}

func (e *ErrNoRoute) Error() string {
	return fmt.Sprintf("no payment gateway can serve method %q", e.Method)
}

func (r *Router) Quote(ctx context.Context, input QuoteInput) (Decision, error) {
	if input.Amount <= 0 {
		return Decision{}, fmt.Errorf("amount must be greater than zero")
	}
	currency := input.Currency
	if currency == "" {
		currency = "IDR"
	}
	if currency != "IDR" {
		return Decision{}, fmt.Errorf("currency %q is not supported; Indonesian gateways settle in IDR", currency)
	}
	method, err := ParseMethod(input.Method)
	if err != nil {
		return Decision{}, err
	}
	rule, ok := r.ruleFor(method)
	if !ok {
		return Decision{}, &ErrNoRoute{Method: method.Key()}
	}

	excluded := map[domain.ProviderCode]bool{}
	for _, p := range input.Exclude {
		excluded[p] = true
	}

	var (
		candidates []Candidate
		rejected   []Rejection
		order      = map[domain.ProviderCode]int{}
	)
	for i, code := range rule.Candidates {
		order[code] = i
		if excluded[code] {
			rejected = append(rejected, Rejection{Provider: code, Reason: "excluded"})
			continue
		}
		support, ok := r.supportFor(code, method)
		if !ok {
			rejected = append(rejected, Rejection{Provider: code, Reason: "method_not_supported_by_adapter"})
			continue
		}
		if !r.cfg.IsEnabled(code, method) {
			rejected = append(rejected, Rejection{Provider: code, Reason: "not_enabled_for_merchant"})
			continue
		}
		if r.available != nil && !r.available(ctx, code, input.Environment) {
			rejected = append(rejected, Rejection{Provider: code, Reason: "not_configured"})
			continue
		}
		if support.RequiresClientToken && !(input.HasClientToken && input.TokenProvider == code) {
			rejected = append(rejected, Rejection{Provider: code, Reason: "requires_client_token"})
			continue
		}
		snap := r.health.Snapshot(code)
		if !r.health.Available(code) {
			rejected = append(rejected, Rejection{Provider: code, Reason: "circuit_open"})
			continue
		}
		feeRule, hasFee := r.cfg.Fees[code][method.Family]
		gatewayFee := feeRule.Fee(input.Amount)
		handling := FeeRule{BasisPoints: r.cfg.HandlingFeeBasisPoints, FixedIDR: r.cfg.HandlingFeeFixedIDR}.Fee(input.Amount)
		candidates = append(candidates, Candidate{
			Provider:       code,
			Mode:           support.Mode,
			GatewayFee:     gatewayFee,
			HandlingFee:    handling,
			EstimatedFee:   gatewayFee + handling,
			FeeIndicative:  !hasFee || !support.ChannelEnforced,
			Status:         snap.Status,
			AdapterMethod:  support.AdapterMethod,
			AdapterChannel: support.AdapterChannel(method.Channel),
			NeedsClientTok: support.RequiresClientToken,
			Requires:       support.Requirements(),
			Support:        support,
		})
	}

	if len(candidates) == 0 {
		return Decision{}, &ErrNoRoute{Method: method.Key(), Rejected: rejected}
	}

	statusRank := func(s GatewayStatus) int {
		if s == StatusHealthy {
			return 0
		}
		return 1
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if ra, rb := statusRank(a.Status), statusRank(b.Status); ra != rb {
			return ra < rb
		}
		if rule.Optimize == OptimizeLowestFee && a.EstimatedFee != b.EstimatedFee {
			return a.EstimatedFee < b.EstimatedFee
		}
		return order[a.Provider] < order[b.Provider]
	})

	policy := "push: fall back on timeout, network error, 5xx or open circuit; the unused instrument on the first gateway is left to expire"
	if ChargesImmediately(method, candidates[0].Mode) {
		policy = "direct charge: fall back only when the first gateway provably did not receive the charge; ambiguous failures return requires_reconcile"
	}

	return Decision{
		Method:         method,
		Amount:         input.Amount,
		Currency:       currency,
		Recommended:    candidates[0],
		Fallbacks:      candidates[1:],
		Rejected:       rejected,
		FallbackPolicy: policy,
	}, nil
}

func (r *Router) ruleFor(method Method) (Rule, bool) {
	var familyRule *Rule
	for i := range r.cfg.Rules {
		rule := r.cfg.Rules[i]
		if rule.Family != method.Family {
			continue
		}
		if rule.Channel != "" && rule.Channel == method.Channel {
			return rule, true
		}
		if rule.Channel == "" && familyRule == nil {
			familyRule = &r.cfg.Rules[i]
		}
	}
	if familyRule != nil {
		return *familyRule, true
	}
	return Rule{}, false
}

func (r *Router) supportFor(code domain.ProviderCode, method Method) (Support, bool) {
	for _, s := range r.support {
		if s.Provider == code && s.Family == method.Family && s.supportsChannel(method.Channel) {
			return s, true
		}
	}
	return Support{}, false
}
