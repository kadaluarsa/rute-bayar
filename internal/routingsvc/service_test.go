package routingsvc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/mandate"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/paymentsvc"
	"github.com/pendig/rute-bayar/internal/provider"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routetoken"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
)

// fakePayments scripts one outcome per provider.
type fakePayments struct {
	mu      sync.Mutex
	outcome map[domain.ProviderCode]func(paymentsvc.CreateInput) (paymentsvc.CreateResult, error)
	calls   []paymentsvc.CreateInput
}

func (f *fakePayments) Create(_ context.Context, in paymentsvc.CreateInput) (paymentsvc.CreateResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	fn := f.outcome[in.Provider]
	f.mu.Unlock()
	if fn == nil {
		return paymentsvc.CreateResult{}, fmt.Errorf("unexpected call to %s", in.Provider)
	}
	return fn(in)
}

func (f *fakePayments) providers() []domain.ProviderCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.ProviderCode, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Provider)
	}
	return out
}

func ok(va, redirect string) func(paymentsvc.CreateInput) (paymentsvc.CreateResult, error) {
	return func(in paymentsvc.CreateInput) (paymentsvc.CreateResult, error) {
		return paymentsvc.CreateResult{ProviderCode: in.Provider, Reference: in.ExternalRef, Response: provider.CreatePaymentResponse{
			ProviderReference: "gw-" + in.ExternalRef, VANumber: va, RedirectURL: redirect, Status: domain.PaymentStatusPending,
		}}, nil
	}
}

func fail(err error) func(paymentsvc.CreateInput) (paymentsvc.CreateResult, error) {
	return func(paymentsvc.CreateInput) (paymentsvc.CreateResult, error) { return paymentsvc.CreateResult{}, err }
}

type harness struct {
	svc      *Service
	payments *fakePayments
	store    *sqlite.Store
	now      time.Time
}

func newHarness(t *testing.T, cfg router.Config) *harness {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "rb.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	signer, err := routetoken.ParseKeys("k1:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	if err != nil {
		t.Fatal(err)
	}
	payments := &fakePayments{outcome: map[domain.ProviderCode]func(paymentsvc.CreateInput) (paymentsvc.CreateResult, error){}}
	svc := New(router.New(cfg, nil, nil), routetoken.NewIssuer(signer, 15*time.Minute), payments, store, metrics.NewSet(), Config{Environment: domain.EnvironmentSandbox})
	return &harness{svc: svc, payments: payments, store: store, now: time.Now()}
}

func (h *harness) token(t *testing.T, orderID, method string, amount int64) TokenResult {
	t.Helper()
	res, err := h.svc.IssueToken(context.Background(), TokenInput{
		OrderID: orderID, MandateID: "mdt-" + orderID, MerchantID: "mer-1", CartHash: "cart-" + orderID,
		Amount: amount, Method: method, MandateExpiresAt: h.now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return res
}

func (h *harness) mandateFor(orderID string, amount int64) mandate.Snapshot {
	return mandate.Snapshot{MandateID: "mdt-" + orderID, OrderID: orderID, MerchantID: "mer-1", CartHash: "cart-" + orderID, Amount: amount, Currency: "IDR", ExpiresAt: h.now.Add(10 * time.Minute)}
}

func vaConfig() router.Config {
	cfg := router.DefaultConfig()
	cfg.Rules = []router.Rule{{Family: router.FamilyVA, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderIPaymu}, Optimize: router.OptimizePriority}}
	return cfg
}

func TestExecuteIssuesOnPrimaryAndReplaysIdempotently(t *testing.T) {
	h := newHarness(t, vaConfig())
	h.payments.outcome[domain.ProviderMidtrans] = ok("8808123", "")
	tok := h.token(t, "ord-1", "va_bca", 150000)
	if got := tok.Claims.Providers; len(got) != 2 || got[0] != domain.ProviderMidtrans {
		t.Fatalf("token should pin midtrans then ipaymu, got %v", got)
	}

	in := ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-1", 150000)}
	res, err := h.svc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionIssued || res.Provider != domain.ProviderMidtrans || res.Instrument.VANumber != "8808123" || res.NextAction.Type != "show_va" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if call := h.payments.calls[0]; call.Method != "bank_transfer" || call.Channel != "bca" || call.ExternalRef != "ord-1" || call.Amount != 150000 {
		t.Fatalf("adapter request not translated from route: %+v", call)
	}

	again, err := h.svc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Instrument.VANumber != "8808123" {
		t.Fatalf("replay should return stored outcome: %+v", again)
	}
	if n := len(h.payments.calls); n != 1 {
		t.Fatalf("retry must not call the gateway again, calls=%d", n)
	}

	// A different token for the same order cannot create a second payment.
	other := h.token(t, "ord-1", "va_bca", 150000)
	third, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: other.Token, Mandate: h.mandateFor("ord-1", 150000)})
	if err != nil || !third.Replayed || len(h.payments.calls) != 1 {
		t.Fatalf("second token must not charge again: %+v err=%v calls=%d", third, err, len(h.payments.calls))
	}
}

func TestExecuteFallsBackOnTimeoutForPushMethods(t *testing.T) {
	h := newHarness(t, vaConfig())
	h.payments.outcome[domain.ProviderMidtrans] = fail(fmt.Errorf("post charge: %w", context.DeadlineExceeded))
	h.payments.outcome[domain.ProviderIPaymu] = ok("7001", "")
	tok := h.token(t, "ord-2", "va_bni", 50000)

	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-2", 50000)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionIssued || res.Provider != domain.ProviderIPaymu {
		t.Fatalf("expected fallback to ipaymu: %+v", res)
	}
	if len(res.Attempts) != 2 || !res.Attempts[0].FellBack || res.Attempts[0].FailureClass != router.FailureAmbiguous {
		t.Fatalf("attempts should record the fallback: %+v", res.Attempts)
	}
}

func TestExecuteNeverFallsBackAfterAmbiguousDirectCharge(t *testing.T) {
	cfg := router.DefaultConfig()
	cfg.Rules = []router.Rule{{Family: router.FamilyCreditCard, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderDoku}, Optimize: router.OptimizePriority}}
	h := newHarness(t, cfg)
	h.payments.outcome[domain.ProviderMidtrans] = fail(errors.New("midtrans create payment returned status 503"))
	h.payments.outcome[domain.ProviderDoku] = ok("", "https://doku.example/pay")

	tok, err := h.svc.IssueToken(context.Background(), TokenInput{
		OrderID: "ord-3", MandateID: "mdt-ord-3", MerchantID: "mer-1", CartHash: "cart-ord-3", Amount: 99000,
		Method: "credit_card", MandateExpiresAt: h.now.Add(5 * time.Minute), HasClientToken: true, ClientTokenSource: domain.ProviderMidtrans,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-3", 99000), CardToken: "tok-abc", CardTokenProvider: domain.ProviderMidtrans})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionRequiresReconcile || res.Provider != domain.ProviderMidtrans {
		t.Fatalf("ambiguous direct card charge must stop for reconcile: %+v", res)
	}
	if got := h.payments.providers(); len(got) != 1 {
		t.Fatalf("must not try another gateway, calls=%v", got)
	}
	if h.payments.calls[0].CardToken != "tok-abc" {
		t.Fatal("card token should reach the gateway that issued it")
	}
}

func TestExecuteFallsBackWhenNotSentEvenForDirectCharge(t *testing.T) {
	cfg := router.DefaultConfig()
	cfg.Rules = []router.Rule{{Family: router.FamilyCreditCard, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderDoku}, Optimize: router.OptimizePriority}}
	h := newHarness(t, cfg)
	h.payments.outcome[domain.ProviderMidtrans] = fail(&paymentsvc.PreflightError{Err: errors.New("midtrans account is not configured")})
	h.payments.outcome[domain.ProviderDoku] = ok("", "https://doku.example/3ds")
	tok, err := h.svc.IssueToken(context.Background(), TokenInput{
		OrderID: "ord-4", MandateID: "mdt-ord-4", MerchantID: "mer-1", CartHash: "cart-ord-4", Amount: 10000,
		Method: "credit_card", MandateExpiresAt: h.now.Add(5 * time.Minute), HasClientToken: true, ClientTokenSource: domain.ProviderMidtrans,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-4", 10000), CardToken: "tok", CardTokenProvider: domain.ProviderMidtrans})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionIssued || res.Provider != domain.ProviderDoku || !res.ChallengeRequired || res.NextAction.Type != "redirect" {
		t.Fatalf("preflight failure should fall back to DOKU hosted 3DS: %+v", res)
	}
	if h.payments.calls[1].CardToken != "" {
		t.Fatal("a Midtrans card token must never be sent to another gateway")
	}
	if snap := h.svc.Router().Health().Snapshot(domain.ProviderMidtrans); snap.Samples != 0 {
		t.Fatalf("local preflight errors must not hurt gateway health: %+v", snap)
	}
}

func TestExecuteRejectedByGatewayDoesNotFallBack(t *testing.T) {
	h := newHarness(t, vaConfig())
	h.payments.outcome[domain.ProviderMidtrans] = fail(errors.New("midtrans create payment returned status 400: invalid"))
	tok := h.token(t, "ord-5", "va_bca", 20000)
	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-5", 20000)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionFailed || len(h.payments.calls) != 1 {
		t.Fatalf("4xx should fail without fallback: %+v", res)
	}

	// Failed is retryable with a fresh token.
	h.payments.outcome[domain.ProviderMidtrans] = ok("1234", "")
	retry := h.token(t, "ord-5", "va_bca", 20000)
	res, err = h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: retry.Token, Mandate: h.mandateFor("ord-5", 20000)})
	if err != nil || res.Status != domain.RouteExecutionIssued || res.Replayed {
		t.Fatalf("new token should re-execute a failed order: %+v err=%v", res, err)
	}
}

func TestExecuteMandateMismatchNeverCallsGateway(t *testing.T) {
	h := newHarness(t, vaConfig())
	h.payments.outcome[domain.ProviderMidtrans] = ok("1", "")
	tok := h.token(t, "ord-6", "va_bca", 150000)

	tampered := h.mandateFor("ord-6", 150000)
	tampered.Amount = 1500000 // user signed a different amount than the token pins
	tampered.CartHash = "other-cart"
	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: tampered})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.RouteExecutionRejected || len(res.Violations) < 2 || len(h.payments.calls) != 0 {
		t.Fatalf("mandate mismatch must block before any gateway call: %+v calls=%d", res, len(h.payments.calls))
	}
}

func TestExecuteRejectsTamperedOrForeignToken(t *testing.T) {
	h := newHarness(t, vaConfig())
	tok := h.token(t, "ord-7", "va_bca", 150000)
	parts := strings.Split(tok.Token, ".")
	parts[1] = strings.Replace(parts[1], parts[1][5:9], "AAAA", 1)
	_, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: strings.Join(parts, "."), Mandate: h.mandateFor("ord-7", 150000)})
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token must be rejected, got %v", err)
	}

	prod := New(h.svc.router, h.svc.tokens, h.payments, h.store, nil, Config{Environment: domain.EnvironmentProduction})
	if _, err := prod.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-7", 150000)}); !errors.Is(err, ErrEnvironment) {
		t.Fatalf("sandbox token must not execute in production, got %v", err)
	}
}

func TestStaleInProgressBecomesRequiresReconcile(t *testing.T) {
	h := newHarness(t, vaConfig())
	tok := h.token(t, "ord-8", "va_bca", 150000)
	if _, acquired, err := h.store.BeginRouteExecution(context.Background(), domain.RouteExecution{OrderID: "ord-8", TokenID: tok.Claims.ID, Environment: domain.EnvironmentSandbox, Method: "va_bca", Amount: 150000, Currency: "IDR"}); err != nil || !acquired {
		t.Fatalf("seed lock: %v", err)
	}
	in := ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-8", 150000)}
	if _, err := h.svc.Execute(context.Background(), in); !errors.Is(err, ErrInProgress) {
		t.Fatalf("fresh lock should report in progress, got %v", err)
	}
	h.svc.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	res, err := h.svc.Execute(context.Background(), in)
	if err != nil || res.Status != domain.RouteExecutionRequiresReconcile || len(h.payments.calls) != 0 {
		t.Fatalf("stale lock should require reconcile without charging: %+v err=%v", res, err)
	}
}

func TestOpenCircuitSkipsGateway(t *testing.T) {
	h := newHarness(t, vaConfig())
	for i := 0; i < 3; i++ {
		h.svc.Router().Health().Record(domain.ProviderMidtrans, time.Millisecond, false)
	}
	h.payments.outcome[domain.ProviderIPaymu] = ok("55", "")
	tok := h.token(t, "ord-9", "va_bca", 10000)
	if len(tok.Claims.Providers) != 1 || tok.Claims.Providers[0] != domain.ProviderIPaymu {
		t.Fatalf("open circuit should drop midtrans from the plan: %v", tok.Claims.Providers)
	}
	res, err := h.svc.Execute(context.Background(), ExecuteInput{RoutingToken: tok.Token, Mandate: h.mandateFor("ord-9", 10000)})
	if err != nil || res.Provider != domain.ProviderIPaymu {
		t.Fatalf("expected ipaymu: %+v %v", res, err)
	}
}

func TestIssueTokenRequiresLiveMandate(t *testing.T) {
	h := newHarness(t, vaConfig())
	_, err := h.svc.IssueToken(context.Background(), TokenInput{OrderID: "o", MandateID: "m", MerchantID: "x", CartHash: "c", Amount: 1000, Method: "va_bca", MandateExpiresAt: h.now.Add(-time.Second)})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expired mandate must not get a token: %v", err)
	}
}
