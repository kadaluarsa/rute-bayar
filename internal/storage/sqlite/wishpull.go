package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

func randomID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return newID(prefix)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func rawOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// BeginRouteExecution takes the per-order execution lock.
//
// It returns acquired=true when the caller now owns the execution: either no
// record existed, or the previous execution failed definitively and the
// caller presents a different routing token. Otherwise it returns the
// existing record untouched so the caller can answer idempotently.
func (s *Store) BeginRouteExecution(ctx context.Context, exec domain.RouteExecution) (domain.RouteExecution, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RouteExecution{}, false, fmt.Errorf("begin route execution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanRouteExecution(tx.QueryRowContext(ctx, routeExecutionSelect+` WHERE order_id = ?`, exec.OrderID))
	now := formatTime(time.Now())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO route_executions (order_id, token_id, environment, method, amount, currency, status, provider_code, result_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)`,
			exec.OrderID, exec.TokenID, string(exec.Environment), exec.Method, exec.Amount, exec.Currency, string(domain.RouteExecutionInProgress), now, now); err != nil {
			return domain.RouteExecution{}, false, fmt.Errorf("insert route execution: %w", err)
		}
	case err != nil:
		return domain.RouteExecution{}, false, err
	case retryable(existing.Status) && existing.TokenID != exec.TokenID:
		if _, err := tx.ExecContext(ctx, `
			UPDATE route_executions SET token_id = ?, environment = ?, method = ?, amount = ?, currency = ?, status = ?, provider_code = '', result_json = '{}', updated_at = ?
			WHERE order_id = ?`,
			exec.TokenID, string(exec.Environment), exec.Method, exec.Amount, exec.Currency, string(domain.RouteExecutionInProgress), now, exec.OrderID); err != nil {
			return domain.RouteExecution{}, false, fmt.Errorf("restart route execution: %w", err)
		}
	default:
		return existing, false, nil
	}
	if err := tx.Commit(); err != nil {
		return domain.RouteExecution{}, false, fmt.Errorf("commit route execution: %w", err)
	}
	exec.Status = domain.RouteExecutionInProgress
	return exec, true, nil
}

// retryable statuses mean no gateway holds a live payment for the order.
func retryable(status domain.RouteExecutionStatus) bool {
	return status == domain.RouteExecutionFailed || status == domain.RouteExecutionRejected
}

// FinishRouteExecution stores the final outcome.
func (s *Store) FinishRouteExecution(ctx context.Context, exec domain.RouteExecution) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE route_executions SET status = ?, provider_code = ?, result_json = ?, updated_at = ?
		WHERE order_id = ? AND token_id = ?`,
		string(exec.Status), string(exec.ProviderCode), rawOrEmptyObject(exec.ResultJSON), formatTime(time.Now()), exec.OrderID, exec.TokenID)
	if err != nil {
		return fmt.Errorf("finish route execution: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("finish route execution: %w: order %s token %s", sql.ErrNoRows, exec.OrderID, exec.TokenID)
	}
	return nil
}

func (s *Store) GetRouteExecution(ctx context.Context, orderID string) (domain.RouteExecution, error) {
	return scanRouteExecution(s.db.QueryRowContext(ctx, routeExecutionSelect+` WHERE order_id = ?`, orderID))
}

const routeExecutionSelect = `SELECT order_id, token_id, environment, method, amount, currency, status, provider_code, result_json, created_at, updated_at FROM route_executions`

type rowScanner interface{ Scan(...any) error }

func scanRouteExecution(row rowScanner) (domain.RouteExecution, error) {
	var (
		exec                   domain.RouteExecution
		env, status, prov      string
		result                 string
		createdRaw, updatedRaw string
	)
	if err := row.Scan(&exec.OrderID, &exec.TokenID, &env, &exec.Method, &exec.Amount, &exec.Currency, &status, &prov, &result, &createdRaw, &updatedRaw); err != nil {
		return domain.RouteExecution{}, err
	}
	exec.Environment = domain.Environment(env)
	exec.Status = domain.RouteExecutionStatus(status)
	exec.ProviderCode = domain.ProviderCode(prov)
	exec.ResultJSON = json.RawMessage(result)
	exec.CreatedAt = parseTime(createdRaw)
	exec.UpdatedAt = parseTime(updatedRaw)
	return exec, nil
}

// UpsertPaymentIntentWithOutboundEvent updates the intent and appends the
// Wishpull event in one transaction, so a reconciled status change is
// never committed without the event that tells Wishpull about it.
func (s *Store) UpsertPaymentIntentWithOutboundEvent(ctx context.Context, intent domain.PaymentIntent, event domain.OutboundEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin intent+event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := upsertPaymentIntent(ctx, tx, intent); err != nil {
		return err
	}
	if _, err := enqueueOutboundEvent(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit intent+event: %w", err)
	}
	return nil
}

// EnqueueOutboundEvent appends an event to the outbox.
func (s *Store) EnqueueOutboundEvent(ctx context.Context, event domain.OutboundEvent) (string, error) {
	return enqueueOutboundEvent(ctx, s.db, event)
}

func enqueueOutboundEvent(ctx context.Context, db sqlExecer, event domain.OutboundEvent) (string, error) {
	id := strings.TrimSpace(event.ID)
	if id == "" {
		id = randomID("evt")
	}
	now := time.Now().UTC()
	next := event.NextAttemptAt
	if next.IsZero() {
		next = now
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO outbound_events (id, event_type, order_id, provider_code, webhook_event_id, payload_json, status, attempts, next_attempt_at, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)`,
		id, event.EventType, event.OrderID, string(event.ProviderCode), event.WebhookEventID, rawOrEmptyObject(event.PayloadJSON), string(domain.OutboundEventPending), formatTime(next), formatTime(now), formatTime(now))
	if err != nil {
		return "", fmt.Errorf("enqueue outbound event: %w", err)
	}
	return id, nil
}

// ClaimDueOutboundEvents leases up to limit pending events whose next
// attempt is due, pushing their next_attempt_at forward by lease so a
// crashed dispatcher does not lose them and a live one does not double-send.
func (s *Store) ClaimDueOutboundEvents(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]domain.OutboundEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim outbound events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, outboundEventSelect+` WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at ASC LIMIT ?`,
		string(domain.OutboundEventPending), formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("query due outbound events: %w", err)
	}
	var events []domain.OutboundEvent
	for rows.Next() {
		event, err := scanOutboundEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	leaseUntil := formatTime(now.Add(lease))
	for _, event := range events {
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_events SET next_attempt_at = ?, updated_at = ? WHERE id = ?`, leaseUntil, formatTime(now), event.ID); err != nil {
			return nil, fmt.Errorf("lease outbound event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit outbound claim: %w", err)
	}
	return events, nil
}

func (s *Store) MarkOutboundEventDelivered(ctx context.Context, id string, attempts int) error {
	now := formatTime(time.Now())
	_, err := s.db.ExecContext(ctx, `UPDATE outbound_events SET status = ?, attempts = ?, last_error = '', delivered_at = ?, updated_at = ? WHERE id = ?`,
		string(domain.OutboundEventDelivered), attempts, now, now, id)
	if err != nil {
		return fmt.Errorf("mark outbound event delivered: %w", err)
	}
	return nil
}

// MarkOutboundEventFailed records a failed attempt. dead=true parks the
// event (status dead) for manual requeue; otherwise it is retried at next.
func (s *Store) MarkOutboundEventFailed(ctx context.Context, id string, attempts int, next time.Time, lastError string, dead bool) error {
	status := domain.OutboundEventPending
	if dead {
		status = domain.OutboundEventDead
	}
	if len(lastError) > 1000 {
		lastError = lastError[:1000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outbound_events SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(status), attempts, formatTime(next), lastError, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("mark outbound event failed: %w", err)
	}
	return nil
}

// RequeueOutboundEvent moves a dead (or pending) event back to pending now.
func (s *Store) RequeueOutboundEvent(ctx context.Context, id string) error {
	now := formatTime(time.Now())
	result, err := s.db.ExecContext(ctx, `UPDATE outbound_events SET status = ?, next_attempt_at = ?, updated_at = ? WHERE id = ? AND status != ?`,
		string(domain.OutboundEventPending), now, now, id, string(domain.OutboundEventDelivered))
	if err != nil {
		return fmt.Errorf("requeue outbound event: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: outbound event %s not found or already delivered", sql.ErrNoRows, id)
	}
	return nil
}

func (s *Store) ListOutboundEvents(ctx context.Context, status domain.OutboundEventStatus, orderID string, limit int) ([]domain.OutboundEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := outboundEventSelect + ` WHERE 1=1`
	args := []any{}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	if strings.TrimSpace(orderID) != "" {
		query += ` AND order_id = ?`
		args = append(args, strings.TrimSpace(orderID))
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list outbound events: %w", err)
	}
	defer rows.Close()
	var events []domain.OutboundEvent
	for rows.Next() {
		event, err := scanOutboundEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) CountOutboundEvents(ctx context.Context, status domain.OutboundEventStatus) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE status = ?`, string(status)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count outbound events: %w", err)
	}
	return count, nil
}

const outboundEventSelect = `SELECT id, event_type, order_id, provider_code, webhook_event_id, payload_json, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at FROM outbound_events`

func scanOutboundEvent(row rowScanner) (domain.OutboundEvent, error) {
	var (
		event                           domain.OutboundEvent
		prov, payload, status           string
		nextRaw, createdRaw, updatedRaw string
		deliveredRaw                    sql.NullString
	)
	if err := row.Scan(&event.ID, &event.EventType, &event.OrderID, &prov, &event.WebhookEventID, &payload, &status, &event.Attempts, &nextRaw, &event.LastError, &createdRaw, &updatedRaw, &deliveredRaw); err != nil {
		return domain.OutboundEvent{}, err
	}
	event.ProviderCode = domain.ProviderCode(prov)
	event.PayloadJSON = json.RawMessage(payload)
	event.Status = domain.OutboundEventStatus(status)
	event.NextAttemptAt = parseTime(nextRaw)
	event.CreatedAt = parseTime(createdRaw)
	event.UpdatedAt = parseTime(updatedRaw)
	if deliveredRaw.Valid {
		t := parseTime(deliveredRaw.String)
		event.DeliveredAt = &t
	}
	return event, nil
}
