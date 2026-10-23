package routingsvc

import (
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/router"
)

// QuoteView renders a decision in the TRD v0.3 §3.1 GetPaymentRouteResponse
// shape, plus the fallback plan.
func QuoteView(d router.Decision) map[string]any {
	fallbacks := make([]map[string]any, 0, len(d.Fallbacks))
	for _, c := range d.Fallbacks {
		fallbacks = append(fallbacks, candidateView(c))
	}
	return map[string]any{
		"method":                 d.Method.Key(),
		"amount":                 d.Amount,
		"currency":               d.Currency,
		"recommended_gateway":    d.Recommended.Provider,
		"estimated_fee":          d.Recommended.EstimatedFee,
		"gateway_fee":            d.Recommended.GatewayFee,
		"handling_fee":           d.Recommended.HandlingFee,
		"fee_indicative":         d.Recommended.FeeIndicative,
		"gateway_status":         d.Recommended.Status,
		"mode":                   d.Recommended.Mode,
		"requires_client_token":  d.Recommended.NeedsClientTok,
		"requires":               d.Recommended.Requires,
		"routing_token_required": true,
		"fallbacks":              fallbacks,
		"rejected":               d.Rejected,
		"fallback_policy":        d.FallbackPolicy,
	}
}

func candidateView(c router.Candidate) map[string]any {
	return map[string]any{
		"gateway":        c.Provider,
		"estimated_fee":  c.EstimatedFee,
		"fee_indicative": c.FeeIndicative,
		"gateway_status": c.Status,
		"mode":           c.Mode,
		"requires":       c.Requires,
	}
}

// TokenView is what the agent receives for RP-2. The token carries no
// credentials and grants nothing without the user's mandate.
func TokenView(t TokenResult) map[string]any {
	return map[string]any{
		"routing_token":       t.Token,
		"token_id":            t.Claims.ID,
		"expires_at":          t.ExpiresAt.Format(time.RFC3339),
		"order_id":            t.Claims.OrderID,
		"amount":              t.Claims.Amount,
		"currency":            t.Claims.Currency,
		"method":              t.Claims.Method,
		"pinned_gateways":     t.Claims.Providers,
		"recommended_gateway": t.Decision.Recommended.Provider,
		"estimated_fee":       t.Claims.EstimatedFee,
	}
}

// AgentStatusView is the redacted payment status for AI agents: no
// instrument, redirect or 3DS URLs (TR-6: the agent never sees the
// challenge page), only what it needs to talk to the user.
func AgentStatusView(orderID string, view ExecutionView, intent *domain.PaymentIntent) map[string]any {
	out := map[string]any{"order_id": orderID, "found": view.Found || intent != nil}
	if view.Found {
		out["execution_status"] = view.Record.Status
		out["gateway"] = view.Record.ProviderCode
		out["challenge_required"] = view.Result.ChallengeRequired
		if view.Result.NextAction != nil {
			out["next_action"] = view.Result.NextAction.Type
		}
		out["updated_at"] = view.Record.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if intent != nil {
		out["payment_status"] = intent.Status
		out["amount"] = intent.Amount
		out["currency"] = intent.Currency
		if !view.Found {
			out["gateway"] = intent.ProviderCode
		}
	}
	return out
}

// HealthView lists every gateway's breaker and latency (RP-3).
func HealthView(r *router.Router) map[string]any {
	return map[string]any{"gateways": r.Health().SnapshotAll(domain.SupportedProviders())}
}
