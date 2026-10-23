// Package mandate implements the gateway-side half of Wishpull's
// mandateCovers check (PRD v0.3 EX-3, TRD v0.2 §5.4).
//
// The AP2 mandate service verifies the user's signature on the Checkout
// Mandate. This package then refuses to send a charge to any gateway unless
// the routing token, the verified mandate and the charge request all agree
// exactly on order, merchant, cart hash, amount and currency. It is plain
// deterministic code: no model output reaches it.
package mandate

import (
	"fmt"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/routetoken"
)

// Snapshot is the verified mandate as passed by the Temporal worker.
type Snapshot struct {
	MandateID  string    `json:"mandate_id"`
	OrderID    string    `json:"order_id"`
	MerchantID string    `json:"merchant_id"`
	CartHash   string    `json:"cart_hash"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked"`
}

// Charge is what is about to be sent to the gateway.
type Charge struct {
	ExternalRef string
	Amount      int64
	Currency    string
	Method      string
	Provider    domain.ProviderCode
}

// Violation lists every mismatch, so the audit log shows the full picture.
type Violation struct {
	Reasons []string
}

func (v *Violation) Error() string {
	return "mandate does not cover this charge: " + strings.Join(v.Reasons, "; ")
}

// Covers returns nil only when every condition holds.
func Covers(claims routetoken.Claims, m Snapshot, c Charge, now time.Time) error {
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }

	if m.Revoked {
		add("mandate %s is revoked", m.MandateID)
	}
	if m.ExpiresAt.IsZero() {
		add("mandate expiry is missing")
	} else if !now.Before(m.ExpiresAt) {
		add("mandate expired at %s", m.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if claims.MandateID != m.MandateID {
		add("token mandate %q != mandate %q", claims.MandateID, m.MandateID)
	}
	if claims.OrderID != m.OrderID || m.OrderID != c.ExternalRef {
		add("order mismatch: token %q, mandate %q, charge %q", claims.OrderID, m.OrderID, c.ExternalRef)
	}
	if claims.MerchantID != m.MerchantID {
		add("merchant mismatch: token %q, mandate %q", claims.MerchantID, m.MerchantID)
	}
	if claims.CartHash == "" || claims.CartHash != m.CartHash {
		add("cart hash mismatch")
	}
	if claims.Amount != m.Amount || m.Amount != c.Amount || c.Amount <= 0 {
		add("amount mismatch: token %d, mandate %d, charge %d", claims.Amount, m.Amount, c.Amount)
	}
	if claims.Currency != m.Currency || m.Currency != c.Currency {
		add("currency mismatch: token %q, mandate %q, charge %q", claims.Currency, m.Currency, c.Currency)
	}
	if claims.Method != c.Method {
		add("method mismatch: token %q, charge %q", claims.Method, c.Method)
	}
	if !claims.AllowsProvider(c.Provider) {
		add("gateway %q is not pinned by the routing token", c.Provider)
	}

	if len(reasons) > 0 {
		return &Violation{Reasons: reasons}
	}
	return nil
}
