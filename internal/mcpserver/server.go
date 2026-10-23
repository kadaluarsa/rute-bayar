// Package mcpserver exposes Wishpull's payment routing plane to AI agents
// over the Model Context Protocol (wishpull-indonesia-pg-router-mcp, TRD
// v0.3 §3.1).
//
// Only safe tools exist here: quoting a route, issuing a routing token,
// reading gateway health and reading a redacted payment status. No tool can
// create, capture or refund a payment. Execution is reachable only through
// the executor-key REST endpoint used by the Temporal worker (EX-1), so a
// prompt-injected agent has no path to move money.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routingsvc"
)

const (
	latestProtocol = "2025-06-18"
	maxBodyBytes   = 1 << 20
)

var supportedProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// IntentLookup reads the webhook-reconciled payment status.
type IntentLookup interface {
	GetPaymentIntentByExternalRef(context.Context, string) (domain.PaymentIntent, error)
}

type Server struct {
	routing *routingsvc.Service
	intents IntentLookup
	metrics *metrics.Set
	version string
	tools   []tool
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	handler     func(context.Context, json.RawMessage) (any, error)
}

func New(routing *routingsvc.Service, intents IntentLookup, m *metrics.Set, version string) *Server {
	if m == nil {
		m = &metrics.Set{}
	}
	s := &Server{routing: routing, intents: intents, metrics: m, version: version}
	s.tools = s.buildTools()
	return s
}

// --- JSON-RPC plumbing -------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Handle processes one JSON-RPC message or batch. It returns nil when the
// input contained only notifications.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeInvalidRequest, Message: "empty request"}})
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "invalid JSON-RPC batch"}})
		}
		var out []json.RawMessage
		for _, item := range batch {
			if resp := s.handleOne(ctx, item); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustJSON(out)
	}
	return s.handleOne(ctx, trimmed)
}

func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) []byte {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "parse error"}})
	}
	isNotification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if isNotification {
			return nil
		}
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request"}})
	}
	result, rpcErr := s.dispatch(ctx, req)
	if isNotification {
		return nil
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	return mustJSON(resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := latestProtocol
		if supportedProtocols[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "wishpull-indonesia-pg-router-mcp", "title": "Rute Bayar payment router", "version": s.version},
			"instructions": "Indonesian payment-gateway router for Wishpull. Use get_optimized_payment_route to choose a gateway and fee, " +
				"then generate_secure_routing_token once the cart is merchant-signed and the user's mandate exists. " +
				"You cannot execute, capture or refund payments: Wishpull's deterministic workflow does that after verifying the mandate. " +
				"Amounts are whole Indonesian rupiah (IDR) integers; never round or estimate them.",
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
		}
		for _, t := range s.tools {
			if t.Name == params.Name {
				return s.callTool(ctx, t, params.Arguments), nil
			}
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", params.Name)}
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)}
	}
}

func (s *Server) callTool(ctx context.Context, t tool, args json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	value, err := t.handler(ctx, args)
	if err != nil {
		s.metrics.MCPCalls.Inc(t.Name, "error")
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	s.metrics.MCPCalls.Inc(t.Name, "ok")
	text, _ := json.Marshal(value)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": value,
		"isError":           false,
	}
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return raw
}

// ServeHTTP implements the Streamable HTTP transport with JSON responses
// (no server-initiated streams, no sessions).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP endpoint accepts POST only (no SSE stream)", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	resp := s.Handle(r.Context(), body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ServeStdio implements the stdio transport: one JSON-RPC message per line.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxBodyBytes)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := s.Handle(ctx, line); resp != nil {
			if _, err := out.Write(append(resp, '\n')); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// --- tools -------------------------------------------------------------

func decodeArgs(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func readOnly(idempotent bool) map[string]any {
	return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": idempotent, "openWorldHint": false}
}

var methodDescription = "Payment method in Wishpull wire format: qris, va_<bank> (e.g. va_bca, va_mandiri), " +
	"direct_debit_<bank> (e.g. direct_debit_bca), credit_card, or ewallet_<wallet> (e.g. ewallet_ovo)."

type quoteArgs struct {
	Amount              int64  `json:"amount"`
	PreferredMethod     string `json:"preferred_method"`
	Currency            string `json:"currency,omitempty"`
	UserID              string `json:"user_id,omitempty"`
	HasClientToken      bool   `json:"has_client_token,omitempty"`
	ClientTokenProvider string `json:"client_token_provider,omitempty"`
}

type tokenArgs struct {
	quoteArgs
	OrderID          string `json:"order_id"`
	MandateID        string `json:"mandate_id"`
	MerchantID       string `json:"merchant_id"`
	CartHash         string `json:"cart_hash"`
	MandateExpiresAt string `json:"mandate_expires_at"`
}

func quoteProperties() map[string]any {
	return map[string]any{
		"amount":                map[string]any{"type": "integer", "minimum": 1, "description": "Exact total in whole rupiah, never rounded (e.g. 150000)."},
		"preferred_method":      map[string]any{"type": "string", "description": methodDescription},
		"currency":              map[string]any{"type": "string", "enum": []string{"IDR"}, "default": "IDR"},
		"user_id":               map[string]any{"type": "string", "description": "Wishpull user id, for audit."},
		"has_client_token":      map[string]any{"type": "boolean", "description": "True when the app already holds a gateway card token from that gateway's SDK."},
		"client_token_provider": map[string]any{"type": "string", "enum": []string{"midtrans", "xendit", "doku", "ipaymu"}},
	}
}

func (s *Server) buildTools() []tool {
	tokenProps := quoteProperties()
	for k, v := range map[string]any{
		"order_id":           map[string]any{"type": "string", "description": "Wishpull order id; becomes the gateway reference."},
		"mandate_id":         map[string]any{"type": "string", "description": "AP2 checkout mandate id the user signed."},
		"merchant_id":        map[string]any{"type": "string"},
		"cart_hash":          map[string]any{"type": "string", "description": "Canonical hash of the merchant-signed cart."},
		"mandate_expires_at": map[string]any{"type": "string", "format": "date-time", "description": "Mandate expiry (RFC 3339). The token never outlives it."},
	} {
		tokenProps[k] = v
	}

	return []tool{
		{
			Name:  "get_optimized_payment_route",
			Title: "Get optimized payment route",
			Description: "Recommend the licensed Indonesian payment gateway (Midtrans, Xendit, DOKU, iPaymu) for a payment method and exact amount, " +
				"with the estimated fee, gateway health and the fallback plan. Read-only: it creates nothing and charges nothing.",
			InputSchema: map[string]any{"type": "object", "properties": quoteProperties(), "required": []string{"amount", "preferred_method"}, "additionalProperties": false},
			Annotations: readOnly(false),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a quoteArgs
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				decision, err := s.routing.Quote(ctx, routingsvc.QuoteInput{Amount: a.Amount, Currency: a.Currency, Method: a.PreferredMethod, HasClientToken: a.HasClientToken, ClientTokenSource: domain.ProviderCode(a.ClientTokenProvider)})
				if err != nil {
					return nil, err
				}
				return routingsvc.QuoteView(decision), nil
			},
		},
		{
			Name:  "generate_secure_routing_token",
			Title: "Generate secure routing token",
			Description: "Pin the route for one order into an HMAC-signed routing token bound to the order, mandate, merchant, cart hash, exact amount and method. " +
				"Pass the token to Wishpull's checkout workflow. The token holds no credentials and cannot execute a payment by itself: " +
				"execution also requires the user's verified mandate and is done by deterministic code, not by you.",
			InputSchema: map[string]any{"type": "object", "properties": tokenProps, "required": []string{"order_id", "mandate_id", "merchant_id", "cart_hash", "amount", "preferred_method", "mandate_expires_at"}, "additionalProperties": false},
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false},
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a tokenArgs
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				expires, err := time.Parse(time.RFC3339, strings.TrimSpace(a.MandateExpiresAt))
				if err != nil {
					return nil, errors.New("mandate_expires_at must be RFC 3339, e.g. 2026-10-23T10:15:00Z")
				}
				result, err := s.routing.IssueToken(ctx, routingsvc.TokenInput{
					OrderID: a.OrderID, MandateID: a.MandateID, MerchantID: a.MerchantID, CartHash: a.CartHash, UserID: a.UserID,
					Amount: a.Amount, Currency: a.Currency, Method: a.PreferredMethod, MandateExpiresAt: expires,
					HasClientToken: a.HasClientToken, ClientTokenSource: domain.ProviderCode(a.ClientTokenProvider),
				})
				if err != nil {
					return nil, err
				}
				return routingsvc.TokenView(result), nil
			},
		},
		{
			Name:        "get_gateway_health",
			Title:       "Get gateway health",
			Description: "Circuit-breaker state, success rate and p95 latency for each payment gateway as observed by this router.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			Annotations: readOnly(true),
			handler: func(_ context.Context, raw json.RawMessage) (any, error) {
				var a struct{}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				return routingsvc.HealthView(s.routing.Router()), nil
			},
		},
		{
			Name:  "get_payment_status",
			Title: "Get payment status",
			Description: "Status of an order's payment as known to the router (execution outcome plus the gateway-webhook status). " +
				"Payment instruments and challenge/redirect URLs are deliberately omitted; the Wishpull app shows those to the user.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"order_id": map[string]any{"type": "string"}}, "required": []string{"order_id"}, "additionalProperties": false},
			Annotations: readOnly(true),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					OrderID string `json:"order_id"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				orderID := strings.TrimSpace(a.OrderID)
				if orderID == "" {
					return nil, errors.New("order_id is required")
				}
				view, err := s.routing.GetExecution(ctx, orderID)
				if err != nil {
					return nil, err
				}
				var intent *domain.PaymentIntent
				if s.intents != nil {
					found, err := s.intents.GetPaymentIntentByExternalRef(ctx, orderID)
					if err == nil {
						intent = &found
					} else if !errors.Is(err, sql.ErrNoRows) {
						return nil, err
					}
				}
				return routingsvc.AgentStatusView(orderID, view, intent), nil
			},
		},
		{
			Name:        "list_payment_methods",
			Title:       "List payment methods",
			Description: "Payment method families this router can route, with example method keys and candidate gateways.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			Annotations: readOnly(true),
			handler: func(_ context.Context, raw json.RawMessage) (any, error) {
				var a struct{}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				return methodCatalog(s.routing.Router().Config()), nil
			},
		},
	}
}

func methodCatalog(cfg router.Config) map[string]any {
	examples := map[router.Family][]string{
		router.FamilyQRIS:        {"qris"},
		router.FamilyVA:          {"va_bca", "va_bni", "va_bri", "va_mandiri", "va_permata"},
		router.FamilyDirectDebit: {"direct_debit_bca"},
		router.FamilyCreditCard:  {"credit_card"},
		router.FamilyEWallet:     {"ewallet_ovo", "ewallet_dana", "ewallet_shopeepay", "ewallet_linkaja"},
	}
	families := []map[string]any{}
	for _, rule := range cfg.Rules {
		if rule.Channel != "" {
			continue
		}
		families = append(families, map[string]any{
			"family":     rule.Family,
			"examples":   examples[rule.Family],
			"candidates": rule.Candidates,
			"optimize":   rule.Optimize,
		})
	}
	return map[string]any{"families": families}
}
