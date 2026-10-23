// Package e2e drives the Wishpull v0.3 flow over real HTTP: fake gateway,
// real adapters, real SQLite, real API/MCP/webhook handlers and the outbox.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/api"
	"github.com/pendig/rute-bayar/internal/daemon"
	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/mcpserver"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/outbox"
	"github.com/pendig/rute-bayar/internal/paymentsvc"
	"github.com/pendig/rute-bayar/internal/providerfactory"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routetoken"
	"github.com/pendig/rute-bayar/internal/routingsvc"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
	"github.com/pendig/rute-bayar/internal/webhooksvc"
)

const (
	adminKey    = "admin-key-0123456789abcdef"
	agentKey    = "agent-key-0123456789abcdef"
	executorKey = "executor-key-0123456789abc"
	serverKey   = "SB-Mid-server-test"
	eventSecret = "wishpull-events-secret-0123456789"
)

type env struct {
	t        *testing.T
	api      *httptest.Server
	charges  atomic.Int32
	store    *sqlite.Store
	ingress  chan ingressCall
	dispatch *outbox.Dispatcher
}

type ingressCall struct {
	body []byte
	sig  string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ingress: make(chan ingressCall, 10)}
	ctx := context.Background()

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "e2e.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.store = store
	if _, err := store.UpsertProviderAccount(ctx, domain.ProviderAccount{
		ProviderCode: domain.ProviderMidtrans, Environment: domain.EnvironmentSandbox, DisplayName: "midtrans",
		CredentialJSON: json.RawMessage(`{"server_key":"` + serverKey + `","client_key":"c","merchant_id":"m"}`), ConfigJSON: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/charge" {
			http.NotFound(w, r)
			return
		}
		e.charges.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		order := body["transaction_details"].(map[string]any)["order_id"].(string)
		_, _ = io.WriteString(w, `{"status_code":"201","transaction_id":"tx-`+order+`","order_id":"`+order+`","payment_type":"bank_transfer","transaction_status":"pending","fraud_status":"accept","va_numbers":[{"bank":"bca","va_number":"8808001234"}],"expiry_time":"2026-10-24 10:00:00 +0700"}`)
	}))
	t.Cleanup(gateway.Close)

	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.ingress <- ingressCall{body: body, sig: r.Header.Get(outbox.HeaderSignature)}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ingress.Close)

	m := metrics.NewSet()
	cfg := router.DefaultConfig()
	cfg.Rules = []router.Rule{{Family: router.FamilyVA, Candidates: []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderDoku}, Optimize: router.OptimizePriority}}
	factory := providerfactory.New(store)
	available := func(ctx context.Context, code domain.ProviderCode, environment domain.Environment) bool {
		_, err := store.GetProviderAccount(ctx, code, environment)
		return err == nil
	}
	signer, _ := routetoken.ParseKeys("k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	payments := paymentsvc.New(store, factory)
	routing := routingsvc.New(router.New(cfg, nil, available), routetoken.NewIssuer(signer, 0), payments, store, m,
		routingsvc.Config{Environment: domain.EnvironmentSandbox, BaseURLs: map[domain.ProviderCode]string{domain.ProviderMidtrans: gateway.URL}})

	apiServer := api.NewServer(api.Config{
		Version: "e2e", APIKey: adminKey, AgentAPIKey: agentKey, ExecutorAPIKey: executorKey, RequireMandate: true,
		RateLimitPerMinute: 1000, AuditSink: store, Store: store, PaymentService: payments,
		DefaultEnvironment: domain.EnvironmentSandbox, Routing: routing, Outbox: store, Metrics: m,
		MCP: mcpserver.New(routing, store, m, "e2e"),
	})
	handlers, err := factory.WebhookHandlers(ctx, domain.EnvironmentSandbox)
	if err != nil {
		t.Fatal(err)
	}
	processor := webhooksvc.New(store, nil, handlers).WithMetrics(m).WithEvents(store, domain.EnvironmentSandbox)
	srv := daemon.NewServer(":0", store, nil, handlers).WithProcessor(processor).WithAPIHandler(apiServer.Handler())
	e.api = httptest.NewServer(srv.Handler())
	t.Cleanup(e.api.Close)

	e.dispatch, err = outbox.NewDispatcher(store, outbox.Config{URL: ingress.URL, Secret: eventSecret}, ingress.Client(), m)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(method, path, key string, body any) (int, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, reader)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if out["data"] == nil && out["error_code"] == nil && len(raw) > 0 {
		out["raw"] = string(raw)
	}
	return resp.StatusCode, out
}

func data(m map[string]any) map[string]any {
	d, _ := m["data"].(map[string]any)
	return d
}

func TestWishpullPaymentFlowEndToEnd(t *testing.T) {
	e := setup(t)
	mandateExpiry := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)

	// 1. RP-1 quote with the agent key.
	code, quote := e.do("POST", "/api/v1/routes/quote", agentKey, map[string]any{"amount": 150000, "preferred_method": "va_bca"})
	if code != 200 || data(quote)["recommended_gateway"] != "midtrans" {
		t.Fatalf("quote: %d %v", code, quote)
	}
	// DOKU has no credentials, so it is reported as rejected, not offered.
	if rejected := data(quote)["rejected"].([]any); len(rejected) != 1 || !strings.Contains(rejected[0].(map[string]any)["reason"].(string), "not_configured") {
		t.Fatalf("doku should be not_configured: %v", data(quote)["rejected"])
	}

	// 2. RP-2 routing token with the agent key.
	code, tok := e.do("POST", "/api/v1/routes/token", agentKey, map[string]any{
		"order_id": "WP-1001", "mandate_id": "mdt-1001", "merchant_id": "mer-7", "cart_hash": "sha256:abc",
		"amount": 150000, "preferred_method": "va_bca", "mandate_expires_at": mandateExpiry,
	})
	if code != 200 {
		t.Fatalf("token: %d %v", code, tok)
	}
	token := data(tok)["routing_token"].(string)

	execBody := map[string]any{
		"routing_token": token,
		"mandate":       map[string]any{"mandate_id": "mdt-1001", "order_id": "WP-1001", "merchant_id": "mer-7", "cart_hash": "sha256:abc", "amount": 150000, "currency": "IDR", "expires_at": mandateExpiry},
		"customer":      map[string]any{"name": "Budi", "email": "budi@example.test"},
	}

	// 3. The agent key can never execute; neither can admin.
	if code, _ := e.do("POST", "/api/v1/routes/execute", agentKey, execBody); code != http.StatusForbidden {
		t.Fatalf("agent execute must be 403, got %d", code)
	}
	if code, _ := e.do("POST", "/api/v1/routes/execute", adminKey, execBody); code != http.StatusForbidden {
		t.Fatalf("admin execute must be 403, got %d", code)
	}
	if code, _ := e.do("POST", "/api/v1/routes/execute", "", execBody); code != http.StatusUnauthorized {
		t.Fatalf("anonymous execute must be 401, got %d", code)
	}
	// RequireMandate closes the legacy direct-charge endpoint.
	if code, _ := e.do("POST", "/api/v1/payments", adminKey, map[string]any{"provider": "midtrans", "reference": "x", "amount": 1000, "method": "bank_transfer"}); code != http.StatusForbidden {
		t.Fatalf("direct payment must be disabled, got %d", code)
	}

	// 4. A tampered mandate is refused before Midtrans is called.
	bad := map[string]any{}
	for k, v := range execBody {
		bad[k] = v
	}
	bad["mandate"] = map[string]any{"mandate_id": "mdt-1001", "order_id": "WP-1001", "merchant_id": "mer-7", "cart_hash": "sha256:abc", "amount": 1500000, "currency": "IDR", "expires_at": mandateExpiry}
	code, rejected := e.do("POST", "/api/v1/routes/execute", executorKey, bad)
	if code != 200 || data(rejected)["status"] != "mandate_rejected" || e.charges.Load() != 0 {
		t.Fatalf("mandate mismatch: %d %v charges=%d", code, rejected, e.charges.Load())
	}

	// 5. Executor runs the token with a matching mandate -> VA issued.
	// The rejected attempt was for the same token, so issue a fresh one.
	_, tok = e.do("POST", "/api/v1/routes/token", agentKey, map[string]any{
		"order_id": "WP-1001", "mandate_id": "mdt-1001", "merchant_id": "mer-7", "cart_hash": "sha256:abc",
		"amount": 150000, "preferred_method": "va_bca", "mandate_expires_at": mandateExpiry,
	})
	execBody["routing_token"] = data(tok)["routing_token"]
	code, executed := e.do("POST", "/api/v1/routes/execute", executorKey, execBody)
	res := data(executed)
	if code != 200 || res["status"] != "issued" || res["provider"] != "midtrans" || res["instrument"].(map[string]any)["va_number"] != "8808001234" {
		t.Fatalf("execute: %d %v", code, executed)
	}
	// Temporal retries the activity: same answer, no second charge.
	_, again := e.do("POST", "/api/v1/routes/execute", executorKey, execBody)
	if data(again)["replayed"] != true || e.charges.Load() != 1 {
		t.Fatalf("replay must not charge again: %v charges=%d", again, e.charges.Load())
	}

	// 6. Agent reads a redacted status over MCP (bearer auth).
	mcpReq, _ := http.NewRequest("POST", e.api.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_payment_status","arguments":{"order_id":"WP-1001"}}}`))
	mcpReq.Header.Set("Authorization", "Bearer "+agentKey)
	mcpResp, err := http.DefaultClient.Do(mcpReq)
	if err != nil {
		t.Fatal(err)
	}
	mcpRaw, _ := io.ReadAll(mcpResp.Body)
	mcpResp.Body.Close()
	if mcpResp.StatusCode != 200 || !strings.Contains(string(mcpRaw), `"execution_status":"issued"`) || strings.Contains(string(mcpRaw), "8808001234") {
		t.Fatalf("MCP status should be redacted: %d %s", mcpResp.StatusCode, mcpRaw)
	}
	noKey, _ := http.Post(e.api.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if noKey.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP without key must be 401, got %d", noKey.StatusCode)
	}
	noKey.Body.Close()

	// 7. Midtrans settles: signed webhook -> intent paid -> outbox -> Wishpull.
	statusCode, gross := "200", "150000.00"
	sum := sha512.Sum512([]byte("WP-1001" + statusCode + gross + serverKey))
	webhook := `{"order_id":"WP-1001","transaction_id":"tx-WP-1001","status_code":"200","gross_amount":"150000.00","transaction_status":"settlement","fraud_status":"accept","signature_key":"` + hex.EncodeToString(sum[:]) + `"}`
	wresp, err := http.Post(e.api.URL+"/webhooks/midtrans", "application/json", strings.NewReader(webhook))
	if err != nil {
		t.Fatal(err)
	}
	wresp.Body.Close()
	if wresp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status %d", wresp.StatusCode)
	}
	if delivered, _, err := e.dispatch.DispatchOnce(context.Background()); err != nil || delivered != 1 {
		t.Fatalf("outbox delivery: delivered=%d err=%v", delivered, err)
	}
	var call ingressCall
	select {
	case call = <-e.ingress:
	case <-time.After(2 * time.Second):
		t.Fatal("Wishpull ingress never received the event")
	}
	if err := outbox.Verify(eventSecret, call.sig, call.body, time.Now(), 5*time.Minute); err != nil {
		t.Fatalf("Wishpull-side signature check failed: %v", err)
	}
	var event struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(call.body, &event)
	if event.Type != "payment.status_changed" || event.Data["order_id"] != "WP-1001" || event.Data["status"] != "settled" || event.Data["amount_verified"] != true {
		t.Fatalf("unexpected event: %s", call.body)
	}

	// 8. Observability: Prometheus metrics reflect the flow.
	resp, _ := http.Get(e.api.URL + "/metrics")
	metricsText, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`rutebayar_route_executions_total{status="issued"} 1`,
		`rutebayar_route_executions_total{status="mandate_rejected"} 1`,
		`rutebayar_gateway_calls_total{provider="midtrans",outcome="issued"} 1`,
		`rutebayar_webhooks_total{provider="midtrans",processing_status="reconciled"} 1`,
		`rutebayar_outbox_deliveries_total{result="delivered"} 1`,
	} {
		if !strings.Contains(string(metricsText), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// TestConcurrentExecuteChargesOnce fires parallel executions of the same
// order: exactly one may reach the gateway.
func TestConcurrentExecuteChargesOnce(t *testing.T) {
	e := setup(t)
	expiry := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	_, tok := e.do("POST", "/api/v1/routes/token", agentKey, map[string]any{
		"order_id": "WP-2", "mandate_id": "m2", "merchant_id": "mer", "cart_hash": "h", "amount": 50000, "preferred_method": "va_bca", "mandate_expires_at": expiry,
	})
	body := map[string]any{
		"routing_token": data(tok)["routing_token"],
		"mandate":       map[string]any{"mandate_id": "m2", "order_id": "WP-2", "merchant_id": "mer", "cart_hash": "h", "amount": 50000, "currency": "IDR", "expires_at": expiry},
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := e.do("POST", "/api/v1/routes/execute", executorKey, body)
			statuses <- code
		}()
	}
	wg.Wait()
	close(statuses)
	for code := range statuses {
		if code != 200 && code != http.StatusConflict {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if n := e.charges.Load(); n != 1 {
		t.Fatalf("gateway charged %d times, want exactly 1", n)
	}
}
