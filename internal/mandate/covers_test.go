package mandate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/routetoken"
)

func fixtures() (routetoken.Claims, Snapshot, Charge, time.Time) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	claims := routetoken.Claims{
		OrderID: "ord_1", MandateID: "mdt_1", MerchantID: "mer_1", CartHash: "h1",
		Amount: 150000, Currency: "IDR", Method: "qris",
		Providers: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderXendit},
	}
	m := Snapshot{MandateID: "mdt_1", OrderID: "ord_1", MerchantID: "mer_1", CartHash: "h1", Amount: 150000, Currency: "IDR", ExpiresAt: now.Add(10 * time.Minute)}
	c := Charge{ExternalRef: "ord_1", Amount: 150000, Currency: "IDR", Method: "qris", Provider: domain.ProviderXendit}
	return claims, m, c, now
}

func TestCoversHappyPath(t *testing.T) {
	claims, m, c, now := fixtures()
	if err := Covers(claims, m, c, now); err != nil {
		t.Fatal(err)
	}
}

func TestCoversRejectsEveryMismatch(t *testing.T) {
	cases := map[string]func(*routetoken.Claims, *Snapshot, *Charge){
		"amount":   func(_ *routetoken.Claims, _ *Snapshot, c *Charge) { c.Amount = 150001 },
		"order":    func(_ *routetoken.Claims, _ *Snapshot, c *Charge) { c.ExternalRef = "ord_2" },
		"merchant": func(_ *routetoken.Claims, m *Snapshot, _ *Charge) { m.MerchantID = "mer_x" },
		"cart":     func(_ *routetoken.Claims, m *Snapshot, _ *Charge) { m.CartHash = "h2" },
		"mandate":  func(cl *routetoken.Claims, _ *Snapshot, _ *Charge) { cl.MandateID = "mdt_2" },
		"currency": func(_ *routetoken.Claims, _ *Snapshot, c *Charge) { c.Currency = "USD" },
		"method":   func(_ *routetoken.Claims, _ *Snapshot, c *Charge) { c.Method = "credit_card" },
		"gateway":  func(_ *routetoken.Claims, _ *Snapshot, c *Charge) { c.Provider = domain.ProviderDoku },
		"revoked":  func(_ *routetoken.Claims, m *Snapshot, _ *Charge) { m.Revoked = true },
		"expired": func(_ *routetoken.Claims, m *Snapshot, _ *Charge) {
			m.ExpiresAt = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
		},
	}
	for name, mutate := range cases {
		claims, m, c, now := fixtures()
		mutate(&claims, &m, &c)
		err := Covers(claims, m, c, now)
		var v *Violation
		if !errors.As(err, &v) || len(v.Reasons) == 0 {
			t.Fatalf("%s: expected violation, got %v", name, err)
		}
	}

	claims, m, c, now := fixtures()
	c.Amount = 1
	m.Revoked = true
	err := Covers(claims, m, c, now)
	if !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "amount mismatch") {
		t.Fatalf("all reasons should be reported: %v", err)
	}
}
