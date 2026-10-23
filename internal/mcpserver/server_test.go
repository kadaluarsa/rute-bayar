package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routetoken"
	"github.com/pendig/rute-bayar/internal/routingsvc"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mcp.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	signer, _ := routetoken.ParseKeys("k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	svc := routingsvc.New(router.New(router.DefaultConfig(), nil, nil), routetoken.NewIssuer(signer, 0), nil, store, nil, routingsvc.Config{Environment: domain.EnvironmentSandbox})
	return New(svc, store, nil, "test")
}

func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	var resp map[string]any
	if err := json.Unmarshal(s.Handle(context.Background(), raw), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	s := newServer(t)
	res := call(t, s, "initialize", map[string]any{"protocolVersion": "2025-03-26"})["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Fatalf("should echo a supported version: %v", res)
	}
	res = call(t, s, "initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"].(map[string]any)
	if res["protocolVersion"] != latestProtocol {
		t.Fatalf("unknown version should get latest: %v", res)
	}
	if s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Fatal("notifications must not get a response")
	}
}

func TestToolListHasNoMoneyMovingTool(t *testing.T) {
	s := newServer(t)
	tools := call(t, s, "tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	names := []string{}
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		names = append(names, name)
		for _, banned := range []string{"execute", "charge", "capture", "refund", "pay"} {
			if strings.Contains(name, banned) && !strings.HasPrefix(name, "get_") && !strings.HasPrefix(name, "list_payment") {
				t.Fatalf("tool %q looks money-moving", name)
			}
		}
	}
	want := "get_optimized_payment_route,generate_secure_routing_token,get_gateway_health,get_payment_status,list_payment_methods"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v", names)
	}
}

func TestQuoteAndTokenTools(t *testing.T) {
	s := newServer(t)
	res := call(t, s, "tools/call", map[string]any{"name": "get_optimized_payment_route", "arguments": map[string]any{"amount": 150000, "preferred_method": "qris"}})["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("quote failed: %v", res)
	}
	quote := res["structuredContent"].(map[string]any)
	if quote["recommended_gateway"] == "" || quote["routing_token_required"] != true {
		t.Fatalf("unexpected quote: %v", quote)
	}

	// Fractional amounts are refused instead of rounded (TRD v0.3 §3.1).
	res = call(t, s, "tools/call", map[string]any{"name": "get_optimized_payment_route", "arguments": map[string]any{"amount": 150000.5, "preferred_method": "qris"}})["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("fractional amount must be an error: %v", res)
	}

	res = call(t, s, "tools/call", map[string]any{"name": "generate_secure_routing_token", "arguments": map[string]any{
		"order_id": "ord-1", "mandate_id": "m-1", "merchant_id": "mer-1", "cart_hash": "h", "amount": 150000,
		"preferred_method": "va_bca", "mandate_expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}})["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("token failed: %v", res)
	}
	tok := res["structuredContent"].(map[string]any)
	if !strings.HasPrefix(tok["routing_token"].(string), "rbt1.") || len(tok["pinned_gateways"].([]any)) == 0 {
		t.Fatalf("unexpected token: %v", tok)
	}

	res = call(t, s, "tools/call", map[string]any{"name": "get_payment_status", "arguments": map[string]any{"order_id": "ord-1"}})["result"].(map[string]any)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["found"] != false {
		t.Fatalf("unknown order should report found=false: %v", res)
	}

	// Unknown arguments are rejected, so a confused agent learns early.
	res = call(t, s, "tools/call", map[string]any{"name": "get_gateway_health", "arguments": map[string]any{"execute": true}})["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("unknown argument should fail: %v", res)
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	s := newServer(t)
	if call(t, s, "tools/call", map[string]any{"name": "execute_payment"})["error"] == nil {
		t.Fatal("unknown tool must be a JSON-RPC error")
	}
	if call(t, s, "resources/list", nil)["error"] == nil {
		t.Fatal("unsupported method must be a JSON-RPC error")
	}
}

func TestStdioTransport(t *testing.T) {
	s := newServer(t)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `"result":{}`) {
		t.Fatalf("stdio output = %q", out.String())
	}
}
