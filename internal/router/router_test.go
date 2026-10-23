package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

func allConfigured(context.Context, domain.ProviderCode, domain.Environment) bool { return true }

func TestParseMethod(t *testing.T) {
	cases := map[string]Method{
		"qris":              {Family: FamilyQRIS},
		"va_mandiri":        {Family: FamilyVA, Channel: "mandiri"},
		"VA-BCA":            {Family: FamilyVA, Channel: "bca"},
		"bank_transfer_bri": {Family: FamilyVA, Channel: "bri"},
		"direct_debit_bca":  {Family: FamilyDirectDebit, Channel: "bca"},
		"credit_card":       {Family: FamilyCreditCard},
		"ewallet_ovo":       {Family: FamilyEWallet, Channel: "ovo"},
		"shopee_pay":        {Family: FamilyEWallet, Channel: "shopeepay"},
	}
	for raw, want := range cases {
		got, err := ParseMethod(raw)
		if err != nil {
			t.Fatalf("ParseMethod(%q) error: %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseMethod(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, bad := range []string{"", "va", "bitcoin", "direct_debit"} {
		if _, err := ParseMethod(bad); err == nil {
			t.Fatalf("ParseMethod(%q) expected error", bad)
		}
	}
}

func TestFeeRoundsUp(t *testing.T) {
	rule := FeeRule{BasisPoints: 70}
	if got := rule.Fee(150000); got != 1050 {
		t.Fatalf("fee = %d, want 1050", got)
	}
	if got := rule.Fee(150001); got != 1051 {
		t.Fatalf("fee = %d, want 1051 (rounded up)", got)
	}
	if got := (FeeRule{BasisPoints: 290, FixedIDR: 2000}).Fee(100000); got != 4900 {
		t.Fatalf("card fee = %d, want 4900", got)
	}
}

func TestQuoteQRISPicksLowestFee(t *testing.T) {
	r := New(DefaultConfig(), nil, allConfigured)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 150000, Method: "qris"})
	if err != nil {
		t.Fatal(err)
	}
	// Default table: Xendit 0.63% beats the 0.70% of the others.
	if d.Recommended.Provider != domain.ProviderXendit {
		t.Fatalf("recommended = %s, want xendit", d.Recommended.Provider)
	}
	if d.Recommended.EstimatedFee != 945 {
		t.Fatalf("fee = %d, want 945", d.Recommended.EstimatedFee)
	}
	if len(d.Fallbacks) != 3 {
		t.Fatalf("fallbacks = %d, want 3", len(d.Fallbacks))
	}
	// Ties keep rule order: midtrans before doku before ipaymu.
	if d.Fallbacks[0].Provider != domain.ProviderMidtrans {
		t.Fatalf("first fallback = %s, want midtrans", d.Fallbacks[0].Provider)
	}
}

func TestQuoteVAMandiriPriorityAndExclusion(t *testing.T) {
	r := New(DefaultConfig(), nil, allConfigured)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 250000, Method: "va_mandiri"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider != domain.ProviderMidtrans || d.Recommended.AdapterMethod != "bank_transfer" || d.Recommended.AdapterChannel != "mandiri" {
		t.Fatalf("unexpected recommendation %+v", d.Recommended)
	}
	d, err = r.Quote(context.Background(), QuoteInput{Amount: 250000, Method: "va_mandiri", Exclude: []domain.ProviderCode{domain.ProviderMidtrans}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider != domain.ProviderXendit || d.Recommended.Mode != ModeHosted || !d.Recommended.FeeIndicative {
		t.Fatalf("unexpected fallback recommendation %+v", d.Recommended)
	}
}

func TestQuoteSkipsUnconfiguredAndUnsupported(t *testing.T) {
	onlyMidtrans := func(_ context.Context, code domain.ProviderCode, _ domain.Environment) bool {
		return code == domain.ProviderMidtrans
	}
	r := New(DefaultConfig(), nil, onlyMidtrans)
	_, err := r.Quote(context.Background(), QuoteInput{Amount: 100000, Method: "direct_debit_bca"})
	var noRoute *ErrNoRoute
	if !errors.As(err, &noRoute) {
		t.Fatalf("expected ErrNoRoute, got %v", err)
	}
	reasons := map[domain.ProviderCode]string{}
	for _, rej := range noRoute.Rejected {
		reasons[rej.Provider] = rej.Reason
	}
	if reasons[domain.ProviderXendit] != "not_configured" || reasons[domain.ProviderDoku] != "method_not_supported_by_adapter" {
		t.Fatalf("unexpected rejections %+v", noRoute.Rejected)
	}
}

func TestQuoteCardNeedsClientTokenForMidtrans(t *testing.T) {
	r := New(DefaultConfig(), nil, allConfigured)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 500000, Method: "credit_card"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider != domain.ProviderDoku {
		t.Fatalf("recommended = %s, want doku", d.Recommended.Provider)
	}
	for _, c := range d.Fallbacks {
		if c.Provider == domain.ProviderMidtrans {
			t.Fatal("midtrans must be skipped without a client card token")
		}
	}
	d, err = r.Quote(context.Background(), QuoteInput{Amount: 500000, Method: "credit_card", HasClientToken: true, TokenProvider: domain.ProviderMidtrans})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fallbacks) == 0 || d.Fallbacks[0].Provider != domain.ProviderMidtrans {
		t.Fatalf("midtrans should follow doku when a token exists: %+v", d.Fallbacks)
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	cfg := DefaultBreakerConfig()
	h := NewHealth(cfg)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		h.Record(domain.ProviderXendit, 100*time.Millisecond, false)
	}
	if !h.Allow(domain.ProviderXendit) {
		t.Fatal("breaker should stay closed below threshold")
	}
	// A slow success counts as a breaker failure (EX-2: > 3000 ms).
	h.Record(domain.ProviderXendit, 3500*time.Millisecond, true)
	if h.Allow(domain.ProviderXendit) {
		t.Fatal("breaker should be open after 3 failures")
	}
	if s := h.Snapshot(domain.ProviderXendit); s.Status != StatusDown || s.OpenUntil == nil {
		t.Fatalf("snapshot = %+v, want DOWN", s)
	}

	r := New(DefaultConfig(), h, allConfigured)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 150000, Method: "qris"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider == domain.ProviderXendit {
		t.Fatal("open circuit gateway must not be recommended")
	}

	now = now.Add(5*time.Minute + time.Second)
	if !h.Allow(domain.ProviderXendit) {
		t.Fatal("half-open breaker should admit one probe")
	}
	if h.Allow(domain.ProviderXendit) {
		t.Fatal("half-open breaker should admit only one probe")
	}
	h.Record(domain.ProviderXendit, 200*time.Millisecond, true)
	if !h.Allow(domain.ProviderXendit) {
		t.Fatal("breaker should close after a successful probe")
	}
}

func TestDegradedGatewaySortsAfterHealthy(t *testing.T) {
	h := NewHealth(DefaultBreakerConfig())
	// Xendit is cheapest for QRIS but slow (p95 > 2000 ms): DEGRADED.
	for i := 0; i < 10; i++ {
		h.Record(domain.ProviderXendit, 2500*time.Millisecond, true)
		h.Record(domain.ProviderXendit, 100*time.Millisecond, true)
	}
	r := New(DefaultConfig(), h, allConfigured)
	// Slow successes trip the breaker once three are consecutive; reset it.
	h.Record(domain.ProviderXendit, 100*time.Millisecond, true)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 150000, Method: "qris"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider == domain.ProviderXendit {
		t.Fatalf("degraded gateway should not be first: %+v", d.Recommended)
	}
	last := d.Fallbacks[len(d.Fallbacks)-1]
	if last.Provider != domain.ProviderXendit || last.Status != StatusDegraded {
		t.Fatalf("degraded gateway should sort last, got %+v", last)
	}
}

func TestClassifyFailureAndFallbackPolicy(t *testing.T) {
	qris, _ := ParseMethod("qris")
	card, _ := ParseMethod("credit_card")

	timeout := fmt.Errorf("call midtrans create payment: %w", context.DeadlineExceeded)
	refused := fmt.Errorf("call xendit create payment: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	server := errors.New("xendit create payment returned status 503")
	client := errors.New("doku create payment returned status 400")

	if ClassifyFailure(timeout) != FailureAmbiguous || ClassifyFailure(refused) != FailureNotSent ||
		ClassifyFailure(server) != FailureAmbiguous || ClassifyFailure(client) != FailureRejected {
		t.Fatal("unexpected classification")
	}
	if !MayFallBack(qris, ModeDirect, FailureAmbiguous) {
		t.Fatal("QRIS may fall back after a timeout")
	}
	if MayFallBack(card, ModeDirect, FailureAmbiguous) {
		t.Fatal("a direct card charge must not fall back after a timeout")
	}
	if !MayFallBack(card, ModeHosted, FailureAmbiguous) {
		t.Fatal("a hosted card page charges nothing yet and may fall back")
	}
	if !MayFallBack(card, ModeDirect, FailureNotSent) {
		t.Fatal("a charge that never left may fall back")
	}
	if MayFallBack(qris, ModeDirect, FailureRejected) {
		t.Fatal("4xx must not fall back")
	}
}

func TestLoadConfigOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	body := `{"fees":{"midtrans":{"qris":{"basis_points":55}}},"latency_budget_ms":2500,"handling_fee_fixed_idr":1000}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fees[domain.ProviderMidtrans][FamilyQRIS].BasisPoints != 55 || cfg.LatencyBudget != 2500*time.Millisecond || cfg.Breaker.SlowCall != 2500*time.Millisecond {
		t.Fatalf("overlay not applied: %+v", cfg)
	}
	r := New(cfg, nil, allConfigured)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 100000, Method: "qris"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Recommended.Provider != domain.ProviderMidtrans || d.Recommended.HandlingFee != 1000 || d.Recommended.EstimatedFee != 1550 {
		t.Fatalf("unexpected decision %+v", d.Recommended)
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"rules":[{"family":"qris","candidates":["paypal"],"optimize":"priority"}]}`), 0o600)
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("expected validation error for unknown provider")
	}
}

func TestMerchantEnabledListRestrictsGateways(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = map[domain.ProviderCode][]string{
		domain.ProviderMidtrans: {"va_bca", "qris"}, // BNI VA not activated on the merchant account
		domain.ProviderIPaymu:   {},                 // gateway off
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := New(cfg, nil, nil)
	d, err := r.Quote(context.Background(), QuoteInput{Amount: 10000, Method: "va_bni"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Plan() {
		if c.Provider == domain.ProviderMidtrans || c.Provider == domain.ProviderIPaymu {
			t.Fatalf("%s should be excluded for va_bni: %+v", c.Provider, d.Plan())
		}
	}
	reasons := map[domain.ProviderCode]string{}
	for _, rej := range d.Rejected {
		reasons[rej.Provider] = rej.Reason
	}
	if reasons[domain.ProviderMidtrans] != "not_enabled_for_merchant" || reasons[domain.ProviderIPaymu] != "not_enabled_for_merchant" {
		t.Fatalf("rejections = %v", d.Rejected)
	}
	if d, _ := r.Quote(context.Background(), QuoteInput{Amount: 10000, Method: "va_bca"}); d.Recommended.Provider != domain.ProviderMidtrans {
		t.Fatalf("va_bca should still route to midtrans: %+v", d.Recommended)
	}
	cfg.Enabled = map[domain.ProviderCode][]string{domain.ProviderMidtrans: {"va_zzz_bad!"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid enabled entries must fail validation")
	}
}
