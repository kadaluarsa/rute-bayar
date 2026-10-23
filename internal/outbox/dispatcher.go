// Package outbox delivers normalized payment events to Wishpull.
//
// The webhook service writes events to the outbound_events table in the
// same database as the payment intent; this dispatcher leases due rows,
// POSTs them to Wishpull's webhook ingress with an HMAC signature and
// retries with exponential backoff. Because the queue is a table, events
// survive restarts and deploys, which is the delivery guarantee the
// Temporal workflow relies on for its paymentCallback signal.
package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/metrics"
)

const (
	HeaderSignature = "X-RuteBayar-Signature"
	HeaderEventID   = "X-RuteBayar-Event-Id"
	HeaderEventType = "X-RuteBayar-Event-Type"
)

type Store interface {
	ClaimDueOutboundEvents(context.Context, time.Time, int, time.Duration) ([]domain.OutboundEvent, error)
	MarkOutboundEventDelivered(context.Context, string, int) error
	MarkOutboundEventFailed(context.Context, string, int, time.Time, string, bool) error
}

type Config struct {
	URL         string
	Secret      string
	Interval    time.Duration
	BatchSize   int
	Lease       time.Duration
	Timeout     time.Duration
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Lease <= 0 {
		c.Lease = c.Timeout + 20*time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 15
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Minute
	}
	return c
}

type Dispatcher struct {
	store   Store
	cfg     Config
	client  *http.Client
	metrics *metrics.Set
	now     func() time.Time
	logf    func(string, ...any)
}

func NewDispatcher(store Store, cfg Config, client *http.Client, m *metrics.Set) (*Dispatcher, error) {
	cfg = cfg.withDefaults()
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("outbox URL is required")
	}
	if len(cfg.Secret) < 32 {
		return nil, errors.New("outbox signing secret must be at least 32 characters")
	}
	if client == nil {
		client = &http.Client{}
	}
	if m == nil {
		m = &metrics.Set{}
	}
	return &Dispatcher{store: store, cfg: cfg, client: client, metrics: m, now: time.Now, logf: log.Printf}, nil
}

// Run dispatches until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, _, err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
			d.logf("outbox dispatch error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DispatchOnce sends one batch of due events.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (delivered, failed int, err error) {
	events, err := d.store.ClaimDueOutboundEvents(ctx, d.now().UTC(), d.cfg.BatchSize, d.cfg.Lease)
	if err != nil {
		return 0, 0, err
	}
	for _, event := range events {
		attempts := event.Attempts + 1
		sendErr := d.send(ctx, event)
		if sendErr == nil {
			if err := d.store.MarkOutboundEventDelivered(ctx, event.ID, attempts); err != nil {
				return delivered, failed, err
			}
			d.metrics.OutboxDeliveries.Inc("delivered")
			delivered++
			continue
		}
		failed++
		dead := attempts >= d.cfg.MaxAttempts
		next := d.now().UTC().Add(d.backoff(attempts))
		if dead {
			d.metrics.OutboxDeliveries.Inc("dead")
			d.logf("outbox event %s (%s order=%s) is dead after %d attempts: %v", event.ID, event.EventType, event.OrderID, attempts, sendErr)
		} else {
			d.metrics.OutboxDeliveries.Inc("retry")
		}
		if err := d.store.MarkOutboundEventFailed(ctx, event.ID, attempts, next, sendErr.Error(), dead); err != nil {
			return delivered, failed, err
		}
	}
	return delivered, failed, nil
}

func (d *Dispatcher) backoff(attempts int) time.Duration {
	wait := d.cfg.BaseBackoff
	for i := 1; i < attempts && wait < d.cfg.MaxBackoff; i++ {
		wait *= 2
	}
	if wait > d.cfg.MaxBackoff {
		wait = d.cfg.MaxBackoff
	}
	// ±20% jitter so many events do not retry in lockstep.
	jitter := time.Duration(float64(wait) * (rand.Float64()*0.4 - 0.2))
	return wait + jitter
}

// Body renders the delivered JSON: the stored payload plus the event id.
func Body(event domain.OutboundEvent) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("decode outbound payload: %w", err)
	}
	payload["id"] = event.ID
	return json.Marshal(payload)
}

func (d *Dispatcher) send(ctx context.Context, event domain.OutboundEvent) error {
	body, err := Body(event)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rute-bayar-outbox/1")
	req.Header.Set(HeaderEventID, event.ID)
	req.Header.Set(HeaderEventType, event.EventType)
	req.Header.Set(HeaderSignature, Sign(d.cfg.Secret, d.now().Unix(), body))
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wishpull ingress returned status %d", resp.StatusCode)
	}
	return nil
}

// Sign returns "t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>.<body>")>".
func Sign(secret string, timestamp int64, body []byte) string {
	ts := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header; receivers should use the same logic
// (see docs/wishpull.md for a TypeScript version).
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return errors.New("signature timestamp is invalid")
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return errors.New("signature header is malformed")
	}
	if tolerance > 0 {
		if age := now.Sub(time.Unix(ts, 0)); age > tolerance || age < -tolerance {
			return errors.New("signature timestamp is outside the tolerance window")
		}
	}
	expected := Sign(secret, ts, body)
	_, want, _ := strings.Cut(expected, ",v1=")
	for _, sig := range sigs {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}
