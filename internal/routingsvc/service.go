// Package routingsvc is Wishpull's payment routing plane (PRD v0.3).
//
// It joins three deterministic pieces:
//
//   - router: which licensed gateway should serve a method, at what fee,
//     and whether falling back is safe (RP-1, RP-3, EX-2);
//   - routetoken: an HMAC token pinning order, mandate, cart hash, amount,
//     method and allowed gateways (RP-2, anti-tampering NFR);
//   - mandate: the gateway-side mandateCovers check (EX-3).
//
// Quote and IssueToken never move money and are safe for AI agents.
// Execute is the only path that creates a charge; it is meant to be called
// by the deterministic Temporal worker with the executor API key (EX-1).
package routingsvc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/mandate"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/paymentsvc"
	"github.com/pendig/rute-bayar/internal/router"
	"github.com/pendig/rute-bayar/internal/routetoken"
)

// PaymentCreator is the subset of paymentsvc the router drives.
type PaymentCreator interface {
	Create(context.Context, paymentsvc.CreateInput) (paymentsvc.CreateResult, error)
}

// ExecutionStore persists the per-order execution lock and outcome.
type ExecutionStore interface {
	BeginRouteExecution(context.Context, domain.RouteExecution) (domain.RouteExecution, bool, error)
	FinishRouteExecution(context.Context, domain.RouteExecution) error
	GetRouteExecution(context.Context, string) (domain.RouteExecution, error)
}

// Config tunes the service.
type Config struct {
	// Environment is the gateway credential environment this instance serves.
	Environment domain.Environment
	// StaleAfter: an in_progress execution older than this is presumed to
	// have crashed mid-call and is reported as requires_reconcile.
	StaleAfter time.Duration
	// BaseURLs overrides gateway base URLs (tests, sandboxes, proxies).
	BaseURLs map[domain.ProviderCode]string
}

type Service struct {
	router   *router.Router
	tokens   *routetoken.Issuer
	payments PaymentCreator
	store    ExecutionStore
	metrics  *metrics.Set
	cfg      Config
	now      func() time.Time
}

func New(r *router.Router, tokens *routetoken.Issuer, payments PaymentCreator, store ExecutionStore, m *metrics.Set, cfg Config) *Service {
	if cfg.Environment == "" {
		cfg.Environment = domain.EnvironmentSandbox
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 2*r.Config().PullMethodTimeout + time.Minute
	}
	if m == nil {
		m = &metrics.Set{}
	}
	return &Service{router: r, tokens: tokens, payments: payments, store: store, metrics: m, cfg: cfg, now: time.Now}
}

func (s *Service) Router() *router.Router          { return s.router }
func (s *Service) Environment() domain.Environment { return s.cfg.Environment }
func (s *Service) TokensConfigured() bool          { return s.tokens != nil }
func (s *Service) SetClock(now func() time.Time)   { s.now = now }

// Typed errors let transports map failures to status codes.
var (
	ErrInvalidToken    = errors.New("invalid routing token")
	ErrInProgress      = errors.New("another execution for this order is in progress")
	ErrNotConfigured   = errors.New("routing service is not fully configured")
	ErrEnvironment     = errors.New("routing token environment does not match this instance")
	ErrInvalidArgument = errors.New("invalid argument")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// QuoteInput is RP-1's GetPaymentRouteArgs.
type QuoteInput struct {
	Amount            int64
	Currency          string
	Method            string
	HasClientToken    bool
	ClientTokenSource domain.ProviderCode
}

func (s *Service) Quote(ctx context.Context, input QuoteInput) (router.Decision, error) {
	decision, err := s.router.Quote(ctx, router.QuoteInput{
		Amount:         input.Amount,
		Currency:       strings.ToUpper(strings.TrimSpace(input.Currency)),
		Method:         input.Method,
		Environment:    s.cfg.Environment,
		HasClientToken: input.HasClientToken,
		TokenProvider:  input.ClientTokenSource,
	})
	family := "unknown"
	if m, perr := router.ParseMethod(input.Method); perr == nil {
		family = string(m.Family)
	}
	if err != nil {
		var noRoute *router.ErrNoRoute
		if errors.As(err, &noRoute) {
			s.metrics.Quotes.Inc(family, "no_route")
			return decision, err
		}
		s.metrics.Quotes.Inc(family, "invalid")
		return decision, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s.metrics.Quotes.Inc(family, "ok")
	return decision, nil
}

// TokenInput is RP-2: everything the token pins.
type TokenInput struct {
	OrderID           string
	MandateID         string
	MerchantID        string
	CartHash          string
	UserID            string
	Amount            int64
	Currency          string
	Method            string
	MandateExpiresAt  time.Time
	HasClientToken    bool
	ClientTokenSource domain.ProviderCode
}

type TokenResult struct {
	Token     string            `json:"routing_token"`
	ExpiresAt time.Time         `json:"expires_at"`
	Claims    routetoken.Claims `json:"claims"`
	Decision  router.Decision   `json:"route"`
}

// IssueToken re-runs the quote server-side (the caller's view of the route
// is never trusted) and pins the resulting plan into a signed token.
func (s *Service) IssueToken(ctx context.Context, input TokenInput) (TokenResult, error) {
	if s.tokens == nil {
		return TokenResult{}, fmt.Errorf("%w: routing token keys are not set (RUTE_BAYAR_ROUTING_TOKEN_KEYS)", ErrNotConfigured)
	}
	if input.MandateExpiresAt.IsZero() {
		return TokenResult{}, invalid("mandate_expires_at is required so the token cannot outlive the mandate")
	}
	if !input.MandateExpiresAt.After(s.now()) {
		return TokenResult{}, invalid("mandate already expired at %s", input.MandateExpiresAt.UTC().Format(time.RFC3339))
	}
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))
	if currency == "" {
		currency = "IDR"
	}
	decision, err := s.Quote(ctx, QuoteInput{Amount: input.Amount, Currency: currency, Method: input.Method, HasClientToken: input.HasClientToken, ClientTokenSource: input.ClientTokenSource})
	if err != nil {
		return TokenResult{}, err
	}
	providers := make([]domain.ProviderCode, 0, len(decision.Fallbacks)+1)
	for _, c := range decision.Plan() {
		providers = append(providers, c.Provider)
	}
	token, claims, err := s.tokens.Issue(routetoken.IssueInput{
		Claims: routetoken.Claims{
			Environment:  s.cfg.Environment,
			OrderID:      strings.TrimSpace(input.OrderID),
			MandateID:    strings.TrimSpace(input.MandateID),
			MerchantID:   strings.TrimSpace(input.MerchantID),
			CartHash:     strings.TrimSpace(input.CartHash),
			UserID:       strings.TrimSpace(input.UserID),
			Amount:       input.Amount,
			Currency:     currency,
			Method:       decision.Method.Key(),
			Providers:    providers,
			EstimatedFee: decision.Recommended.EstimatedFee,
		},
		NotAfter: input.MandateExpiresAt,
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s.metrics.TokensIssued.Inc(string(decision.Method.Family))
	return TokenResult{Token: token, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Claims: claims, Decision: decision}, nil
}

// ExecuteInput is what the Temporal worker sends after the AP2 mandate
// service verified the user's signature.
type ExecuteInput struct {
	RoutingToken string
	Mandate      mandate.Snapshot
	// CardToken is a gateway-specific token produced on the device by the
	// gateway SDK (never raw card data). CardTokenProvider names its gateway.
	CardToken         string
	CardTokenProvider domain.ProviderCode
	CustomerName      string
	CustomerEmail     string
	CustomerPhone     string
	NotificationURL   string
}

// Instrument is what the customer pays with.
type Instrument struct {
	ProviderReference string `json:"provider_reference,omitempty"`
	PaymentType       string `json:"payment_type,omitempty"`
	VANumber          string `json:"va_number,omitempty"`
	BillerCode        string `json:"biller_code,omitempty"`
	QRString          string `json:"qr_string,omitempty"`
	RedirectURL       string `json:"redirect_url,omitempty"`
	ExpiryTime        string `json:"expiry_time,omitempty"`
}

// NextAction tells the trusted surface what to show. Redirect pages are
// rendered by the native app's secure web view, never by the AI (TR-6).
type NextAction struct {
	Type string `json:"type"` // show_va | show_qr | redirect | none
	URL  string `json:"url,omitempty"`
}

// Attempt is one gateway call, kept for the audit trail.
type Attempt struct {
	Provider     domain.ProviderCode `json:"provider"`
	Outcome      string              `json:"outcome"` // issued | failed | skipped
	FailureClass router.FailureClass `json:"failure_class,omitempty"`
	LatencyMs    int64               `json:"latency_ms"`
	Error        string              `json:"error,omitempty"`
	FellBack     bool                `json:"fell_back,omitempty"`
}

type ExecuteResult struct {
	Status            domain.RouteExecutionStatus `json:"status"`
	OrderID           string                      `json:"order_id"`
	TokenID           string                      `json:"token_id"`
	Provider          domain.ProviderCode         `json:"provider,omitempty"`
	Mode              router.ExecutionMode        `json:"mode,omitempty"`
	Method            string                      `json:"method"`
	Amount            int64                       `json:"amount"`
	Currency          string                      `json:"currency"`
	PaymentStatus     domain.PaymentStatus        `json:"payment_status,omitempty"`
	Instrument        *Instrument                 `json:"instrument,omitempty"`
	NextAction        *NextAction                 `json:"next_action,omitempty"`
	ChallengeRequired bool                        `json:"challenge_required"`
	// ReconcileProviders lists gateways that may hold a payment for this
	// order after an ambiguous failure. Reconcile them before any retry.
	ReconcileProviders []domain.ProviderCode `json:"reconcile_providers,omitempty"`
	Violations         []string              `json:"violations,omitempty"`
	Attempts           []Attempt             `json:"attempts"`
	Message            string                `json:"message,omitempty"`
	Replayed           bool                  `json:"replayed"`
}

// Execute runs a routing token. It is idempotent per order: repeating the
// call (e.g. a Temporal activity retry) returns the stored outcome and never
// issues a second charge.
func (s *Service) Execute(ctx context.Context, input ExecuteInput) (ExecuteResult, error) {
	if s.tokens == nil || s.store == nil || s.payments == nil {
		return ExecuteResult{}, ErrNotConfigured
	}
	claims, err := s.tokens.Verify(input.RoutingToken)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Environment != s.cfg.Environment {
		return ExecuteResult{}, fmt.Errorf("%w: token %q, instance %q", ErrEnvironment, claims.Environment, s.cfg.Environment)
	}
	method, err := router.ParseMethod(claims.Method)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	base := ExecuteResult{OrderID: claims.OrderID, TokenID: claims.ID, Method: claims.Method, Amount: claims.Amount, Currency: claims.Currency, Attempts: []Attempt{}}

	current, acquired, err := s.store.BeginRouteExecution(ctx, domain.RouteExecution{
		OrderID: claims.OrderID, TokenID: claims.ID, Environment: claims.Environment,
		Method: claims.Method, Amount: claims.Amount, Currency: claims.Currency,
	})
	if err != nil {
		return ExecuteResult{}, err
	}
	if !acquired {
		return s.replay(ctx, current, claims, base)
	}

	result := s.run(ctx, claims, method, input, base)
	s.metrics.Executions.Inc(string(result.Status))

	raw, _ := json.Marshal(result)
	if err := s.store.FinishRouteExecution(context.WithoutCancel(ctx), domain.RouteExecution{
		OrderID: claims.OrderID, TokenID: claims.ID, Status: result.Status, ProviderCode: result.Provider, ResultJSON: raw,
	}); err != nil {
		return result, fmt.Errorf("payment outcome reached but could not be stored: %w", err)
	}
	return result, nil
}

func (s *Service) replay(ctx context.Context, current domain.RouteExecution, claims routetoken.Claims, base ExecuteResult) (ExecuteResult, error) {
	if current.Status == domain.RouteExecutionInProgress {
		if s.now().Sub(current.UpdatedAt) < s.cfg.StaleAfter {
			return ExecuteResult{}, ErrInProgress
		}
		// The holder crashed mid-call: the gateway may or may not have the
		// payment, so the only safe answer is reconcile.
		stale := base
		stale.TokenID = current.TokenID
		stale.Status = domain.RouteExecutionRequiresReconcile
		stale.Message = "previous execution did not finish; reconcile with every pinned gateway before retrying"
		stale.ReconcileProviders = claims.Providers
		raw, _ := json.Marshal(stale)
		if err := s.store.FinishRouteExecution(ctx, domain.RouteExecution{OrderID: current.OrderID, TokenID: current.TokenID, Status: stale.Status, ResultJSON: raw}); err != nil {
			return ExecuteResult{}, err
		}
		s.metrics.Executions.Inc(string(stale.Status))
		stale.Replayed = true
		return stale, nil
	}
	var stored ExecuteResult
	if err := json.Unmarshal(current.ResultJSON, &stored); err != nil {
		return ExecuteResult{}, fmt.Errorf("decode stored execution: %w", err)
	}
	stored.Replayed = true
	if current.TokenID != claims.ID {
		stored.Message = strings.TrimSpace(stored.Message + " order already has an execution from another routing token; returning it unchanged")
	}
	return stored, nil
}

func (s *Service) run(ctx context.Context, claims routetoken.Claims, method router.Method, input ExecuteInput, result ExecuteResult) ExecuteResult {
	now := s.now()

	// EX-3: provider-independent part of mandateCovers before anything else.
	if err := mandate.Covers(claims, input.Mandate, mandate.Charge{
		ExternalRef: claims.OrderID, Amount: claims.Amount, Currency: claims.Currency,
		Method: claims.Method, Provider: claims.Providers[0],
	}, now); err != nil {
		return s.rejected(result, err)
	}

	decision, err := s.router.Quote(ctx, router.QuoteInput{
		Amount: claims.Amount, Currency: claims.Currency, Method: claims.Method, Environment: claims.Environment,
		HasClientToken: strings.TrimSpace(input.CardToken) != "", TokenProvider: input.CardTokenProvider,
	})
	if err != nil {
		result.Status = domain.RouteExecutionFailed
		result.Message = "no pinned gateway can serve this payment right now: " + err.Error()
		return result
	}
	plan := make([]router.Candidate, 0, len(claims.Providers))
	for _, c := range decision.Plan() {
		if claims.AllowsProvider(c.Provider) {
			plan = append(plan, c)
		}
	}
	if len(plan) == 0 {
		result.Status = domain.RouteExecutionFailed
		result.Message = "none of the gateways pinned by the routing token is available right now"
		return result
	}

	cfg := s.router.Config()
	var lastErr error
	for i, cand := range plan {
		// Per-gateway half of mandateCovers (the token must pin it).
		if err := mandate.Covers(claims, input.Mandate, mandate.Charge{
			ExternalRef: claims.OrderID, Amount: claims.Amount, Currency: claims.Currency,
			Method: claims.Method, Provider: cand.Provider,
		}, s.now()); err != nil {
			return s.rejected(result, err)
		}
		if !s.router.Health().Allow(cand.Provider) {
			result.Attempts = append(result.Attempts, Attempt{Provider: cand.Provider, Outcome: "skipped", FailureClass: router.FailureNotSent, Error: router.ErrCircuitOpen.Error()})
			continue
		}

		timeout := cfg.LatencyBudget
		if router.ChargesImmediately(method, cand.Mode) {
			timeout = cfg.PullMethodTimeout
		}
		cardToken := ""
		if cand.NeedsClientTok && cand.Provider == input.CardTokenProvider {
			cardToken = strings.TrimSpace(input.CardToken)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		started := time.Now()
		created, err := s.payments.Create(attemptCtx, paymentsvc.CreateInput{
			Provider:        cand.Provider,
			Environment:     claims.Environment,
			BaseURL:         s.cfg.BaseURLs[cand.Provider],
			ExternalRef:     claims.OrderID,
			Amount:          claims.Amount,
			Currency:        claims.Currency,
			Method:          cand.AdapterMethod,
			Channel:         cand.AdapterChannel,
			CustomerName:    input.CustomerName,
			CustomerEmail:   input.CustomerEmail,
			CustomerPhone:   input.CustomerPhone,
			CardToken:       cardToken,
			NotificationURL: input.NotificationURL,
		})
		cancel()
		latency := time.Since(started)

		if err == nil {
			s.router.Health().Record(cand.Provider, latency, true)
			s.metrics.GatewayCalls.Inc(string(cand.Provider), "issued")
			s.metrics.GatewayLatency.Observe(latency.Seconds(), string(cand.Provider))
			result.Attempts = append(result.Attempts, Attempt{Provider: cand.Provider, Outcome: "issued", LatencyMs: latency.Milliseconds()})
			return issued(result, method, cand, created)
		}

		lastErr = err
		class := classify(err)
		if !isPreflight(err) {
			// A rejection proves the gateway is up; everything else counts
			// against its health. Local preflight errors never do.
			s.router.Health().Record(cand.Provider, latency, class == router.FailureRejected)
			s.metrics.GatewayLatency.Observe(latency.Seconds(), string(cand.Provider))
		}
		s.metrics.GatewayCalls.Inc(string(cand.Provider), string(class))
		attempt := Attempt{Provider: cand.Provider, Outcome: "failed", FailureClass: class, LatencyMs: latency.Milliseconds(), Error: truncate(err.Error(), 300)}

		if class == router.FailureAmbiguous {
			result.ReconcileProviders = append(result.ReconcileProviders, cand.Provider)
		}
		hasNext := i < len(plan)-1
		if hasNext && router.MayFallBack(method, cand.Mode, class) {
			attempt.FellBack = true
			result.Attempts = append(result.Attempts, attempt)
			s.metrics.Fallbacks.Inc(string(cand.Provider), string(plan[i+1].Provider))
			continue
		}
		result.Attempts = append(result.Attempts, attempt)
		break
	}

	result.Provider = ""
	if len(result.ReconcileProviders) > 0 {
		result.Status = domain.RouteExecutionRequiresReconcile
		result.Provider = result.ReconcileProviders[len(result.ReconcileProviders)-1]
		result.Message = "a gateway may have created the payment without confirming it; reconcile before retrying"
		return result
	}
	result.Status = domain.RouteExecutionFailed
	if lastErr != nil {
		result.Message = "no gateway accepted the payment: " + truncate(lastErr.Error(), 300)
	} else {
		result.Message = "every pinned gateway is unavailable (open circuit)"
	}
	return result
}

func (s *Service) rejected(result ExecuteResult, err error) ExecuteResult {
	result.Status = domain.RouteExecutionRejected
	result.Message = "charge refused before contacting any gateway: mandate does not cover it"
	var v *mandate.Violation
	if errors.As(err, &v) {
		result.Violations = v.Reasons
		for _, reason := range v.Reasons {
			s.metrics.MandateRejections.Inc(violationKind(reason))
		}
	} else {
		result.Violations = []string{err.Error()}
		s.metrics.MandateRejections.Inc("other")
	}
	return result
}

func violationKind(reason string) string {
	for _, k := range []string{"revoked", "expired", "expiry", "amount", "order", "merchant", "cart", "currency", "method", "gateway", "token mandate"} {
		if strings.Contains(reason, k) {
			return strings.ReplaceAll(k, " ", "_")
		}
	}
	return "other"
}

func issued(result ExecuteResult, method router.Method, cand router.Candidate, created paymentsvc.CreateResult) ExecuteResult {
	resp := created.Response
	result.Status = domain.RouteExecutionIssued
	result.Provider = cand.Provider
	result.Mode = cand.Mode
	result.PaymentStatus = resp.Status
	result.ReconcileProviders = nil
	result.Instrument = &Instrument{
		ProviderReference: resp.ProviderReference,
		PaymentType:       resp.PaymentType,
		VANumber:          resp.VANumber,
		BillerCode:        resp.BillerCode,
		QRString:          resp.QRString,
		RedirectURL:       resp.RedirectURL,
		ExpiryTime:        resp.ExpiryTime,
	}
	switch {
	case cand.Mode == router.ModeDirect && method.Family == router.FamilyVA && resp.VANumber != "":
		result.NextAction = &NextAction{Type: "show_va"}
	case cand.Mode == router.ModeDirect && method.Family == router.FamilyQRIS && (resp.QRString != "" || resp.RedirectURL != ""):
		result.NextAction = &NextAction{Type: "show_qr", URL: resp.RedirectURL}
	case resp.RedirectURL != "":
		// Hosted page or 3DS/OTP challenge: the app opens it in its secure
		// web view and the workflow waits for the gateway webhook.
		result.NextAction = &NextAction{Type: "redirect", URL: resp.RedirectURL}
		result.ChallengeRequired = true
	default:
		result.NextAction = &NextAction{Type: "none"}
	}
	return result
}

func classify(err error) router.FailureClass {
	if isPreflight(err) {
		return router.FailureNotSent
	}
	return router.ClassifyFailure(err)
}

func isPreflight(err error) bool {
	var pre *paymentsvc.PreflightError
	return errors.As(err, &pre)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// ExecutionView is the read model for status lookups.
type ExecutionView struct {
	Found  bool
	Record domain.RouteExecution
	Result ExecuteResult
}

func (s *Service) GetExecution(ctx context.Context, orderID string) (ExecutionView, error) {
	if s.store == nil {
		return ExecutionView{}, ErrNotConfigured
	}
	record, err := s.store.GetRouteExecution(ctx, strings.TrimSpace(orderID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionView{}, nil
		}
		return ExecutionView{}, err
	}
	view := ExecutionView{Found: true, Record: record}
	_ = json.Unmarshal(record.ResultJSON, &view.Result)
	return view, nil
}
