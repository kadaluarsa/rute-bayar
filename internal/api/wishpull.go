package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/mandate"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routingsvc"
)

// role is the capability tier of the presented API key.
type role int

const (
	roleNone role = iota
	// roleAgent: AI agents (hosted LangGraph agent, external MCP clients).
	// Read and quote only; can never trigger a charge.
	roleAgent
	// roleExecutor: Wishpull's deterministic Temporal worker (EX-1).
	roleExecutor
	// roleAdmin: operators; the original RUTE_BAYAR_API_KEY.
	roleAdmin
)

func (r role) String() string {
	switch r {
	case roleAgent:
		return "agent"
	case roleExecutor:
		return "executor"
	case roleAdmin:
		return "admin"
	default:
		return "anonymous"
	}
}

// presentedKey reads X-API-Key, or an Authorization bearer token (the form
// MCP clients send).
func presentedKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func presentedKeyDigest(r *http.Request) string {
	digest := sha256.Sum256([]byte(presentedKey(r)))
	return hex.EncodeToString(digest[:8])
}

func keyMatches(presented, configured string) bool {
	if presented == "" || configured == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

// resolveRole maps the presented key to a role. Keys are compared in
// constant time; an empty configured key never matches.
func (s *Server) resolveRole(r *http.Request) role {
	key := presentedKey(r)
	switch {
	case keyMatches(key, s.apiKey):
		return roleAdmin
	case keyMatches(key, s.executorKey):
		return roleExecutor
	case keyMatches(key, s.agentKey):
		return roleAgent
	default:
		return roleNone
	}
}

func roleAllowed(got role, allowed []role) bool {
	for _, r := range allowed {
		if r == got {
			return true
		}
	}
	return false
}

// wrapRoles is wrap with a role check instead of the admin-only check.
func (s *Server) wrapRoles(fn endpointFunc, allowed ...role) http.HandlerFunc {
	inner := s.wrap(fn, false)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			got := s.resolveRole(r)
			if !roleAllowed(got, allowed) {
				requestID := strings.TrimSpace(r.Header.Get(requestIDHeader))
				if requestID == "" {
					requestID = newRequestID()
				}
				s.writeCORSHeaders(w)
				w.Header().Set(requestIDHeader, requestID)
				status, code, message := http.StatusUnauthorized, errUnauthorized, "missing or invalid API key"
				if got != roleNone {
					status, code, message = http.StatusForbidden, errForbidden, "this API key ("+got.String()+") may not call this endpoint"
				}
				s.writeJSONError(w, r, requestID, status, code, message)
				s.audit(r, requestID, status, 0)
				return
			}
		}
		inner(w, r)
	}
}

func (s *Server) requireRouting() (*routingsvc.Service, error) {
	if s.routing == nil {
		return nil, NewError(http.StatusServiceUnavailable, errInternal, "routing plane is not configured")
	}
	return s.routing, nil
}

// routingError maps routingsvc errors to HTTP errors.
func routingError(err error) error {
	var noRoute *router.ErrNoRoute
	switch {
	case errors.As(err, &noRoute):
		reasons := make([]string, 0, len(noRoute.Rejected))
		for _, rej := range noRoute.Rejected {
			reasons = append(reasons, string(rej.Provider)+": "+rej.Reason)
		}
		msg := err.Error()
		if len(reasons) > 0 {
			msg += " (" + strings.Join(reasons, "; ") + ")"
		}
		return NewError(http.StatusUnprocessableEntity, "no_route", msg)
	case errors.Is(err, routingsvc.ErrInvalidToken):
		return NewError(http.StatusUnprocessableEntity, "invalid_routing_token", err.Error())
	case errors.Is(err, routingsvc.ErrInProgress):
		return NewError(http.StatusConflict, errConflict, err.Error())
	case errors.Is(err, routingsvc.ErrEnvironment):
		return NewError(http.StatusConflict, "environment_mismatch", err.Error())
	case errors.Is(err, routingsvc.ErrNotConfigured):
		return NewError(http.StatusServiceUnavailable, errInternal, err.Error())
	case errors.Is(err, routingsvc.ErrInvalidArgument):
		return NewError(http.StatusBadRequest, errBadRequest, err.Error())
	default:
		return NewError(http.StatusInternalServerError, errInternal, err.Error())
	}
}

type routeQuotePayload struct {
	Amount              int64  `json:"amount"`
	Currency            string `json:"currency"`
	PreferredMethod     string `json:"preferred_method"`
	Method              string `json:"method"`
	UserID              string `json:"user_id"`
	HasClientToken      bool   `json:"has_client_token"`
	ClientTokenProvider string `json:"client_token_provider"`
}

func (p routeQuotePayload) method() string {
	if strings.TrimSpace(p.PreferredMethod) != "" {
		return p.PreferredMethod
	}
	return p.Method
}

// routeQuoteHandler implements RP-1 get_optimized_payment_route.
func (s *Server) routeQuoteHandler(r *http.Request) (any, error) {
	svc, err := s.requireRouting()
	if err != nil {
		return nil, err
	}
	var payload routeQuotePayload
	if err := decodeJSONBody(r, &payload); err != nil {
		return nil, NewError(http.StatusBadRequest, errBadRequest, err.Error())
	}
	decision, err := svc.Quote(r.Context(), routingsvc.QuoteInput{
		Amount: payload.Amount, Currency: payload.Currency, Method: payload.method(),
		HasClientToken: payload.HasClientToken, ClientTokenSource: domain.ProviderCode(strings.ToLower(strings.TrimSpace(payload.ClientTokenProvider))),
	})
	if err != nil {
		return nil, routingError(err)
	}
	return routingsvc.QuoteView(decision), nil
}

type routeTokenPayload struct {
	routeQuotePayload
	OrderID          string `json:"order_id"`
	MandateID        string `json:"mandate_id"`
	MerchantID       string `json:"merchant_id"`
	CartHash         string `json:"cart_hash"`
	MandateExpiresAt string `json:"mandate_expires_at"`
}

// TokenInputFromJSON is shared with the MCP tool.
func (p routeTokenPayload) toInput() (routingsvc.TokenInput, error) {
	expires, err := parseRFC3339(p.MandateExpiresAt, "mandate_expires_at")
	if err != nil {
		return routingsvc.TokenInput{}, err
	}
	return routingsvc.TokenInput{
		OrderID: p.OrderID, MandateID: p.MandateID, MerchantID: p.MerchantID, CartHash: p.CartHash,
		UserID: p.UserID, Amount: p.Amount, Currency: p.Currency, Method: p.method(),
		MandateExpiresAt: expires, HasClientToken: p.HasClientToken,
		ClientTokenSource: domain.ProviderCode(strings.ToLower(strings.TrimSpace(p.ClientTokenProvider))),
	}, nil
}

func parseRFC3339(raw, field string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, NewError(http.StatusBadRequest, errBadRequest, field+" is required (RFC 3339)")
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, NewError(http.StatusBadRequest, errBadRequest, field+" must be RFC 3339, e.g. 2026-10-23T10:15:00Z")
	}
	return t, nil
}

// routeTokenHandler implements RP-2 generate_secure_routing_token.
func (s *Server) routeTokenHandler(r *http.Request) (any, error) {
	svc, err := s.requireRouting()
	if err != nil {
		return nil, err
	}
	var payload routeTokenPayload
	if err := decodeJSONBody(r, &payload); err != nil {
		return nil, NewError(http.StatusBadRequest, errBadRequest, err.Error())
	}
	input, err := payload.toInput()
	if err != nil {
		return nil, err
	}
	result, err := svc.IssueToken(r.Context(), input)
	if err != nil {
		return nil, routingError(err)
	}
	return routingsvc.TokenView(result), nil
}

type mandatePayload struct {
	MandateID  string `json:"mandate_id"`
	OrderID    string `json:"order_id"`
	MerchantID string `json:"merchant_id"`
	CartHash   string `json:"cart_hash"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	ExpiresAt  string `json:"expires_at"`
	Revoked    bool   `json:"revoked"`
}

type routeExecutePayload struct {
	RoutingToken      string         `json:"routing_token"`
	Mandate           mandatePayload `json:"mandate"`
	CardToken         string         `json:"card_token"`
	CardTokenProvider string         `json:"card_token_provider"`
	NotificationURL   string         `json:"notification_url"`
	Customer          struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"customer"`
}

// routeExecuteHandler is the deterministic money path (EX-1/EX-2/EX-3).
// Only the executor key reaches it.
func (s *Server) routeExecuteHandler(r *http.Request) (any, error) {
	svc, err := s.requireRouting()
	if err != nil {
		return nil, err
	}
	var payload routeExecutePayload
	if err := decodeJSONBody(r, &payload); err != nil {
		return nil, NewError(http.StatusBadRequest, errBadRequest, err.Error())
	}
	if strings.TrimSpace(payload.RoutingToken) == "" {
		return nil, NewError(http.StatusBadRequest, errBadRequest, "routing_token is required")
	}
	expires, err := parseRFC3339(payload.Mandate.ExpiresAt, "mandate.expires_at")
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(payload.Mandate.Currency))
	if currency == "" {
		currency = "IDR"
	}
	result, err := svc.Execute(r.Context(), routingsvc.ExecuteInput{
		RoutingToken: payload.RoutingToken,
		Mandate: mandate.Snapshot{
			MandateID: strings.TrimSpace(payload.Mandate.MandateID), OrderID: strings.TrimSpace(payload.Mandate.OrderID),
			MerchantID: strings.TrimSpace(payload.Mandate.MerchantID), CartHash: strings.TrimSpace(payload.Mandate.CartHash),
			Amount: payload.Mandate.Amount, Currency: currency, ExpiresAt: expires, Revoked: payload.Mandate.Revoked,
		},
		CardToken:         payload.CardToken,
		CardTokenProvider: domain.ProviderCode(strings.ToLower(strings.TrimSpace(payload.CardTokenProvider))),
		CustomerName:      strings.TrimSpace(payload.Customer.Name),
		CustomerEmail:     strings.TrimSpace(payload.Customer.Email),
		CustomerPhone:     strings.TrimSpace(payload.Customer.Phone),
		NotificationURL:   strings.TrimSpace(payload.NotificationURL),
	})
	if err != nil {
		return nil, routingError(err)
	}
	return result, nil
}

func (s *Server) routeExecutionGetHandler(r *http.Request) (any, error) {
	svc, err := s.requireRouting()
	if err != nil {
		return nil, err
	}
	orderID := strings.TrimSpace(r.PathValue("order_id"))
	view, err := svc.GetExecution(r.Context(), orderID)
	if err != nil {
		return nil, routingError(err)
	}
	if !view.Found {
		return nil, NewError(http.StatusNotFound, errNotFound, "no execution for order "+orderID)
	}
	return map[string]any{
		"order_id":   view.Record.OrderID,
		"token_id":   view.Record.TokenID,
		"status":     view.Record.Status,
		"provider":   view.Record.ProviderCode,
		"created_at": formatTime(view.Record.CreatedAt),
		"updated_at": formatTime(view.Record.UpdatedAt),
		"result":     view.Result,
	}, nil
}

func (s *Server) gatewayHealthHandler(r *http.Request) (any, error) {
	svc, err := s.requireRouting()
	if err != nil {
		return nil, err
	}
	return routingsvc.HealthView(svc.Router()), nil
}

// OutboxStore exposes the Wishpull event outbox to operators.
type OutboxStore interface {
	ListOutboundEvents(context.Context, domain.OutboundEventStatus, string, int) ([]domain.OutboundEvent, error)
	RequeueOutboundEvent(context.Context, string) error
}

func (s *Server) outboundEventsListHandler(r *http.Request) (any, error) {
	if s.outbox == nil {
		return nil, NewError(http.StatusServiceUnavailable, errInternal, "outbox is not configured")
	}
	q := r.URL.Query()
	events, err := s.outbox.ListOutboundEvents(r.Context(), domain.OutboundEventStatus(strings.TrimSpace(q.Get("status"))), q.Get("order_id"), parseListLimit(q.Get("limit"), 50))
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, errInternal, err.Error())
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		item := map[string]any{
			"id": e.ID, "event_type": e.EventType, "order_id": e.OrderID, "provider": e.ProviderCode,
			"webhook_event_id": e.WebhookEventID, "status": e.Status, "attempts": e.Attempts,
			"next_attempt_at": formatTime(e.NextAttemptAt), "last_error": e.LastError,
			"created_at": formatTime(e.CreatedAt), "payload": parseRawJSON(e.PayloadJSON),
		}
		if e.DeliveredAt != nil {
			item["delivered_at"] = formatTime(*e.DeliveredAt)
		}
		items = append(items, item)
	}
	return map[string]any{"items": items, "count": len(items)}, nil
}

func (s *Server) outboundEventRequeueHandler(r *http.Request) (any, error) {
	if s.outbox == nil {
		return nil, NewError(http.StatusServiceUnavailable, errInternal, "outbox is not configured")
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.outbox.RequeueOutboundEvent(r.Context(), id); err != nil {
		return nil, NewError(http.StatusNotFound, errNotFound, err.Error())
	}
	return map[string]any{"id": id, "status": domain.OutboundEventPending}, nil
}

// metricsHandler serves Prometheus text format.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if s.metricsToken != "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(auth) <= 7 || !strings.EqualFold(auth[:7], "bearer ") || !keyMatches(strings.TrimSpace(auth[7:]), s.metricsToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.metrics == nil {
		return
	}
	var buf bytes.Buffer
	s.metrics.Registry.WriteText(&buf)
	_, _ = w.Write(buf.Bytes())
}

// mcpHandler guards /mcp with the agent/executor/admin keys, the shared
// rate limiter and the audit log.
func (s *Server) mcpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if requestID == "" {
			requestID = newRequestID()
		}
		s.writeCORSHeaders(w)
		w.Header().Set(requestIDHeader, requestID)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.allowRate(r, r.RemoteAddr) {
			http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"rate limit exceeded"}}`, http.StatusTooManyRequests)
			s.audit(r, requestID, http.StatusTooManyRequests, time.Since(start).Milliseconds())
			return
		}
		if !roleAllowed(s.resolveRole(r), []role{roleAgent, roleExecutor, roleAdmin}) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rute-bayar-mcp"`)
			http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"missing or invalid API key"}}`, http.StatusUnauthorized)
			s.audit(r, requestID, http.StatusUnauthorized, time.Since(start).Milliseconds())
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		s.mcp.ServeHTTP(rec, r)
		s.audit(r, requestID, rec.status, time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
