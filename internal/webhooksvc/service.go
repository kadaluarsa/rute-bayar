package webhooksvc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/forwarding"
	"github.com/pendig/rute-bayar/internal/metrics"
	"github.com/pendig/rute-bayar/internal/provider"
)

type Recorder interface {
	RecordWebhookEvent(context.Context, domain.WebhookEvent) (string, error)
}

type Reconciler interface {
	Recorder
	GetPaymentIntentByExternalRef(context.Context, string) (domain.PaymentIntent, error)
	UpsertPaymentIntent(context.Context, domain.PaymentIntent) (string, error)
}

type RefundReconciler interface {
	ReconcileRefundByProviderIdentifiers(context.Context, domain.ProviderCode, []string, domain.PaymentStatus, json.RawMessage) (domain.Refund, error)
}

type WebhookEventLookup interface {
	GetWebhookEventByProviderEventID(context.Context, domain.ProviderCode, string) (domain.WebhookEvent, error)
}

type Forwarder interface {
	Forward(context.Context, forwarding.InboundWebhook) error
}

// EventSink receives normalized payment events for Wishpull (the outbox).
type EventSink interface {
	EnqueueOutboundEvent(context.Context, domain.OutboundEvent) (string, error)
}

type Service struct {
	recorder    Recorder
	forwarder   Forwarder
	handlers    map[domain.ProviderCode]provider.Adapter
	now         func() time.Time
	events      EventSink
	environment domain.Environment
	metrics     *metrics.Set
}

// WithEvents makes the service emit signed, normalized payment events to
// Wishpull through the outbox after every state change it reconciles.
func (s *Service) WithEvents(sink EventSink, environment domain.Environment) *Service {
	s.events = sink
	s.environment = environment
	return s
}

func (s *Service) WithMetrics(m *metrics.Set) *Service {
	s.metrics = m
	return s
}

// reconcileOutcome carries what the outbox needs about a reconciliation.
type reconcileOutcome struct {
	status         string
	intent         domain.PaymentIntent
	previous       domain.PaymentStatus
	incoming       domain.PaymentStatus
	reportedAmount int64
	amountVerified bool
	refund         bool
	paymentRef     string
}

type Input struct {
	Provider domain.ProviderCode
	Request  provider.WebhookRequest
}

type Result struct {
	StatusCode int
	Body       map[string]any
	EventID    string
}

func New(recorder Recorder, forwarder Forwarder, handlers map[domain.ProviderCode]provider.Adapter) *Service {
	if handlers == nil {
		handlers = make(map[domain.ProviderCode]provider.Adapter)
	}
	return &Service{
		recorder:  recorder,
		forwarder: forwarder,
		handlers:  handlers,
		now:       time.Now,
	}
}

func (s *Service) Process(ctx context.Context, input Input) (Result, error) {
	if s == nil {
		return Result{StatusCode: http.StatusInternalServerError, Body: map[string]any{"error": "webhook service is not configured"}}, fmt.Errorf("webhook service is not configured")
	}

	providerCode := domain.ProviderCode(strings.TrimSpace(string(input.Provider)))
	if providerCode == "" {
		return Result{StatusCode: http.StatusBadRequest, Body: map[string]any{"error": "provider is required"}}, nil
	}

	request := provider.WebhookRequest{
		Headers:    cloneHeaders(input.Request.Headers),
		Body:       cloneBytes(input.Request.Body),
		TargetPath: strings.TrimSpace(input.Request.TargetPath),
	}
	event := domain.WebhookEvent{
		ID:               newWebhookEventID(),
		ProviderCode:     providerCode,
		EventType:        "unknown",
		SignatureValid:   false,
		PayloadJSON:      json.RawMessage(request.Body),
		HeadersJSON:      marshalHeaders(request.Headers),
		ReceivedAt:       s.now().UTC(),
		ProcessingStatus: "received",
	}

	handler, handlerFound := s.handlers[providerCode]
	if handlerFound {
		if err := handler.VerifyWebhook(ctx, request); err != nil {
			event.EventType = "verification_failed"
			event.ProcessingStatus = "rejected"
			_, _ = s.recordEvent(ctx, event)
			return Result{
				StatusCode: http.StatusBadRequest,
				Body: map[string]any{
					"status": "rejected",
					"error":  err.Error(),
				},
			}, nil
		}

		event.SignatureValid = true
		parsedEvent, parseErr := handler.ParseWebhook(ctx, request)
		if parseErr != nil {
			event.EventType = "parse_error"
			event.ProcessingStatus = "parse_failed"

			eventID, recordErr := s.recordEvent(ctx, event)
			if recordErr != nil {
				return Result{
					StatusCode: http.StatusInternalServerError,
					Body: map[string]any{
						"error": "failed to record webhook event",
					},
				}, recordErr
			}

			return Result{
				StatusCode: http.StatusAccepted,
				Body: map[string]any{
					"status":  "accepted",
					"warning": "webhook verification passed but parsing failed",
				},
				EventID: eventID,
			}, nil
		}

		event.EventType = parsedEvent.EventType
		event.ProviderEventID = parsedEvent.ProviderEventID

		duplicate, err := s.isDuplicateWebhookEvent(ctx, providerCode, parsedEvent.ProviderEventID)
		if err != nil {
			event.ProcessingStatus = "duplicate_lookup_failed"
			eventID, recordErr := s.recordEvent(ctx, event)
			if recordErr != nil {
				return Result{
					StatusCode: http.StatusInternalServerError,
					Body: map[string]any{
						"error": "failed to record webhook event",
					},
				}, recordErr
			}

			return Result{
				StatusCode: http.StatusInternalServerError,
				Body: map[string]any{
					"status":  "error",
					"error":   err.Error(),
					"message": "webhook duplicate lookup failed",
				},
				EventID: eventID,
			}, err
		}
		if duplicate {
			event.ProcessingStatus = "duplicate"
			eventID, recordErr := s.recordEvent(ctx, event)
			if recordErr != nil {
				return Result{
					StatusCode: http.StatusInternalServerError,
					Body: map[string]any{
						"error": "failed to record webhook event",
					},
				}, recordErr
			}

			return Result{
				StatusCode: http.StatusAccepted,
				Body: map[string]any{
					"status":  "accepted",
					"warning": "duplicate webhook event ignored",
				},
				EventID: eventID,
			}, nil
		}

		outcome, err := s.reconcilePaymentIntent(ctx, providerCode, parsedEvent, event.ID)
		event.ProcessingStatus = outcome.status
		if err != nil {
			event.ProcessingStatus = "reconcile_failed"

			eventID, recordErr := s.recordEvent(ctx, event)
			if recordErr != nil {
				return Result{
					StatusCode: http.StatusInternalServerError,
					Body: map[string]any{
						"error": "failed to record webhook event",
					},
				}, recordErr
			}

			return Result{
				StatusCode: http.StatusInternalServerError,
				Body: map[string]any{
					"status":  "error",
					"error":   err.Error(),
					"message": "webhook reconciliation failed",
				},
				EventID: eventID,
			}, err
		}
	}

	eventID, err := s.recordEvent(ctx, event)
	if err != nil {
		return Result{
			StatusCode: http.StatusInternalServerError,
			Body: map[string]any{
				"error": "failed to record webhook event",
			},
		}, err
	}
	s.countWebhook(providerCode, event.ProcessingStatus)

	inbound := forwarding.InboundWebhook{
		WebhookEventID: eventID,
		Provider:       providerCode,
		Headers:        request.Headers.Clone(),
		Body:           request.Body,
	}
	if s.forwarder != nil {
		if err := s.forwarder.Forward(ctx, inbound); err != nil {
			return Result{
				StatusCode: http.StatusAccepted,
				Body: map[string]any{
					"status":  "accepted",
					"warning": fmt.Sprintf("forwarding failed: %v", err),
				},
				EventID: eventID,
			}, nil
		}
	}

	return Result{
		StatusCode: http.StatusAccepted,
		Body: map[string]any{
			"status": "accepted",
		},
		EventID: eventID,
	}, nil
}

func (s *Service) recordEvent(ctx context.Context, event domain.WebhookEvent) (string, error) {
	if s.recorder == nil {
		return "", nil
	}

	if event.ProcessedAt == nil {
		processedAt := s.now().UTC()
		event.ProcessedAt = &processedAt
	}

	id, err := s.recorder.RecordWebhookEvent(ctx, event)
	if err != nil {
		return "", fmt.Errorf("record webhook event: %w", err)
	}
	return id, nil
}

func (s *Service) isDuplicateWebhookEvent(ctx context.Context, providerCode domain.ProviderCode, providerEventID string) (bool, error) {
	if s == nil || s.recorder == nil {
		return false, nil
	}

	providerEventID = strings.TrimSpace(providerEventID)
	if providerEventID == "" {
		return false, nil
	}

	lookup, ok := s.recorder.(WebhookEventLookup)
	if !ok {
		return false, nil
	}

	previous, err := lookup.GetWebhookEventByProviderEventID(ctx, providerCode, providerEventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lookup webhook event by provider event id: %w", err)
	}
	// A delivery that failed to process must be retryable: the gateway's
	// redelivery is the only way to recover it.
	switch previous.ProcessingStatus {
	case "reconcile_failed", "duplicate_lookup_failed":
		return false, nil
	}
	return true, nil
}

func (s *Service) reconcilePaymentIntent(ctx context.Context, providerCode domain.ProviderCode, parsedEvent provider.WebhookEvent, webhookEventID string) (reconcileOutcome, error) {
	outcome := reconcileOutcome{incoming: parsedEvent.Status, reportedAmount: parsedEvent.Amount, paymentRef: strings.TrimSpace(parsedEvent.PaymentRef)}
	if isRefundWebhook(parsedEvent) {
		outcome.refund = true
		if status, err := s.reconcileRefund(ctx, providerCode, parsedEvent); status != "" || err != nil {
			outcome.status = status
			if err == nil && status == "reconciled" {
				if enqueueErr := s.enqueue(ctx, providerCode, webhookEventID, outcome); enqueueErr != nil {
					outcome.status = "reconcile_failed"
					return outcome, enqueueErr
				}
			}
			return outcome, err
		}
	}

	reconciler, ok := s.recorder.(Reconciler)
	if !ok {
		outcome.status = "processed"
		return outcome, nil
	}

	reference := strings.TrimSpace(parsedEvent.PaymentRef)
	if reference == "" {
		outcome.status = "unmatched"
		return outcome, nil
	}

	intent, err := reconciler.GetPaymentIntentByExternalRef(ctx, reference)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			outcome.status = "unmatched"
			return outcome, nil
		}
		outcome.status = "reconcile_failed"
		return outcome, err
	}
	outcome.intent = intent
	outcome.previous = intent.Status

	// After a routing fallback the intent belongs to the gateway that
	// issued the instrument; events from an abandoned gateway are ignored.
	if intent.ProviderCode != providerCode {
		outcome.status = "unmatched"
		return outcome, nil
	}

	if isSuccessStatus(parsedEvent.Status) && parsedEvent.Amount > 0 {
		if parsedEvent.Amount != intent.Amount {
			// Never mark an order paid for a different amount (EX-3).
			outcome.status = "amount_mismatch"
			if err := s.enqueue(ctx, providerCode, webhookEventID, outcome); err != nil {
				outcome.status = "reconcile_failed"
				return outcome, err
			}
			return outcome, nil
		}
		outcome.amountVerified = true
	}

	if intent.Status == parsedEvent.Status {
		outcome.status = "duplicate"
		return outcome, nil
	}
	if !statusTransitionAllowed(intent.Status, parsedEvent.Status) {
		// Out-of-order delivery (e.g. "pending" after "settlement").
		outcome.status = "stale"
		return outcome, nil
	}

	intent.Status = parsedEvent.Status
	outcome.intent = intent
	outcome.status = "reconciled"
	if err := s.persistReconciled(ctx, reconciler, providerCode, webhookEventID, outcome); err != nil {
		outcome.status = "reconcile_failed"
		return outcome, err
	}
	return outcome, nil
}

// AtomicReconciler commits an intent change and its Wishpull event together.
type AtomicReconciler interface {
	UpsertPaymentIntentWithOutboundEvent(context.Context, domain.PaymentIntent, domain.OutboundEvent) error
}

func (s *Service) persistReconciled(ctx context.Context, reconciler Reconciler, providerCode domain.ProviderCode, webhookEventID string, outcome reconcileOutcome) error {
	if s.events == nil {
		_, err := reconciler.UpsertPaymentIntent(ctx, outcome.intent)
		return err
	}
	event, err := s.buildEvent(providerCode, webhookEventID, outcome)
	if err != nil {
		return err
	}
	if atomic, ok := s.recorder.(AtomicReconciler); ok {
		return atomic.UpsertPaymentIntentWithOutboundEvent(ctx, outcome.intent, event)
	}
	// Without a transaction, enqueue first: a failure then yields a
	// duplicate event on redelivery (Wishpull dedupes) instead of a lost one.
	if _, err := s.events.EnqueueOutboundEvent(ctx, event); err != nil {
		return err
	}
	_, err = reconciler.UpsertPaymentIntent(ctx, outcome.intent)
	return err
}

func (s *Service) enqueue(ctx context.Context, providerCode domain.ProviderCode, webhookEventID string, outcome reconcileOutcome) error {
	if s.events == nil {
		return nil
	}
	event, err := s.buildEvent(providerCode, webhookEventID, outcome)
	if err != nil {
		return err
	}
	_, err = s.events.EnqueueOutboundEvent(ctx, event)
	return err
}

func isSuccessStatus(status domain.PaymentStatus) bool {
	switch status {
	case domain.PaymentStatusPaid, domain.PaymentStatusSettled, domain.PaymentStatusCaptured:
		return true
	default:
		return false
	}
}

// statusRank orders payment states so webhooks can only move an intent
// forward. Expired/failed/cancelled may still become paid: money that
// actually arrived late must be recorded.
func statusRank(status domain.PaymentStatus) int {
	switch status {
	case domain.PaymentStatusPending, "":
		return 0
	case domain.PaymentStatusExpired, domain.PaymentStatusFailed, domain.PaymentStatusCancelled:
		return 1
	case domain.PaymentStatusAuthorized:
		return 2
	case domain.PaymentStatusPaid, domain.PaymentStatusCaptured:
		return 3
	case domain.PaymentStatusSettled:
		return 4
	case domain.PaymentStatusPartialRefunded:
		return 5
	case domain.PaymentStatusRefunded:
		return 6
	default:
		return 0
	}
}

func statusTransitionAllowed(current, next domain.PaymentStatus) bool {
	if next == "" {
		return false
	}
	cur, nxt := statusRank(current), statusRank(next)
	if cur == 1 && nxt == 1 {
		return true // e.g. expired -> cancelled
	}
	return nxt > cur
}

func (s *Service) countWebhook(providerCode domain.ProviderCode, status string) {
	if s.metrics != nil {
		s.metrics.Webhooks.Inc(string(providerCode), status)
	}
}

// buildEvent renders the normalized event Wishpull receives for a state
// change it must act on (TRD v0.2 §5.2 step 8: signed payment callback).
func (s *Service) buildEvent(providerCode domain.ProviderCode, webhookEventID string, outcome reconcileOutcome) (domain.OutboundEvent, error) {
	eventType := "payment.status_changed"
	switch {
	case outcome.status == "amount_mismatch":
		eventType = "payment.amount_mismatch"
	case outcome.refund:
		eventType = "refund.status_changed"
	}
	orderID := outcome.intent.ExternalRef
	if orderID == "" {
		orderID = outcome.paymentRef
	}
	data := map[string]any{
		"order_id":         orderID,
		"gateway":          providerCode,
		"status":           outcome.incoming,
		"previous_status":  outcome.previous,
		"expected_amount":  outcome.intent.Amount,
		"reported_amount":  outcome.reportedAmount,
		"amount_verified":  outcome.amountVerified,
		"currency":         outcome.intent.Currency,
		"webhook_event_id": webhookEventID,
	}
	if outcome.refund {
		delete(data, "amount_verified")
	}
	payload, err := json.Marshal(map[string]any{
		"type":           eventType,
		"schema_version": 1,
		"environment":    s.environment,
		"occurred_at":    s.now().UTC().Format(time.RFC3339Nano),
		"data":           data,
	})
	if err != nil {
		return domain.OutboundEvent{}, err
	}
	return domain.OutboundEvent{
		EventType: eventType, OrderID: orderID, ProviderCode: providerCode,
		WebhookEventID: webhookEventID, PayloadJSON: payload,
	}, nil
}

func newWebhookEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "webhook_" + hex.EncodeToString(b[:])
}

func (s *Service) reconcileRefund(ctx context.Context, providerCode domain.ProviderCode, parsedEvent provider.WebhookEvent) (string, error) {
	reconciler, ok := s.recorder.(RefundReconciler)
	if !ok {
		return "", nil
	}

	status := parsedEvent.Status
	if status == "" {
		return "unmatched", nil
	}

	identifiers := refundWebhookIdentifiers(parsedEvent)
	if len(identifiers) == 0 {
		return "unmatched", nil
	}

	_, err := reconciler.ReconcileRefundByProviderIdentifiers(ctx, providerCode, identifiers, status, parsedEvent.RawPayloadJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "unmatched", nil
		}
		return "reconcile_failed", err
	}

	return "reconciled", nil
}

func isRefundWebhook(event provider.WebhookEvent) bool {
	eventType := strings.ToLower(strings.TrimSpace(event.EventType))
	return strings.HasPrefix(eventType, "refund.") || strings.HasSuffix(eventType, ".refund")
}

func isRefundStatus(status domain.PaymentStatus) bool {
	return status == domain.PaymentStatusRefunded || status == domain.PaymentStatusPartialRefunded
}

func refundWebhookIdentifiers(event provider.WebhookEvent) []string {
	candidates := []string{event.PaymentRef, event.ProviderEventID}
	if _, value, ok := strings.Cut(event.ProviderEventID, ":"); ok {
		candidates = append(candidates, value)
	}

	seen := make(map[string]struct{}, len(candidates))
	identifiers := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		identifiers = append(identifiers, trimmed)
	}
	return identifiers
}

func cloneHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func cloneBytes(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	return append([]byte(nil), body...)
}

func marshalHeaders(headers http.Header) json.RawMessage {
	raw, err := json.Marshal(headers)
	if err != nil {
		return nil
	}
	return raw
}
