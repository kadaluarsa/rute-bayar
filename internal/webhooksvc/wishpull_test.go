package webhooksvc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/provider"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
)

type captureSink struct{ events []domain.OutboundEvent }

func (c *captureSink) EnqueueOutboundEvent(_ context.Context, e domain.OutboundEvent) (string, error) {
	c.events = append(c.events, e)
	return "evt", nil
}

func processOne(t *testing.T, intent domain.PaymentIntent, event provider.WebhookEvent) (*stubWebhookRecorder, *captureSink) {
	t.Helper()
	recorder := &stubWebhookRecorder{intents: map[string]domain.PaymentIntent{intent.ExternalRef: intent}}
	sink := &captureSink{}
	svc := New(recorder, nil, map[domain.ProviderCode]provider.Adapter{
		domain.ProviderMidtrans: &testWebhookAdapter{
			verifyFn: func(context.Context, provider.WebhookRequest) error { return nil },
			parseFn:  func(context.Context, provider.WebhookRequest) (provider.WebhookEvent, error) { return event, nil },
		},
	}).WithEvents(sink, domain.EnvironmentSandbox)
	if _, err := svc.Process(context.Background(), Input{Provider: domain.ProviderMidtrans, Request: provider.WebhookRequest{Body: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	return recorder, sink
}

func pendingIntent() domain.PaymentIntent {
	return domain.PaymentIntent{ID: "i1", ExternalRef: "ord-1", ProviderCode: domain.ProviderMidtrans, Amount: 150000, Currency: "IDR", Status: domain.PaymentStatusPending}
}

func TestPaidWithMatchingAmountEmitsVerifiedEvent(t *testing.T) {
	recorder, sink := processOne(t, pendingIntent(), provider.WebhookEvent{ProviderEventID: "e1", EventType: "settlement", PaymentRef: "ord-1", Status: domain.PaymentStatusSettled, Amount: 150000})
	if recorder.intents["ord-1"].Status != domain.PaymentStatusSettled {
		t.Fatalf("intent should be settled: %+v", recorder.intents["ord-1"])
	}
	if len(sink.events) != 1 || sink.events[0].EventType != "payment.status_changed" || sink.events[0].OrderID != "ord-1" {
		t.Fatalf("expected one status event: %+v", sink.events)
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(sink.events[0].PayloadJSON, &payload)
	if payload.Data["amount_verified"] != true || payload.Data["previous_status"] != "pending" {
		t.Fatalf("payload = %+v", payload.Data)
	}
}

func TestPaidWithWrongAmountIsNotTrusted(t *testing.T) {
	recorder, sink := processOne(t, pendingIntent(), provider.WebhookEvent{ProviderEventID: "e2", EventType: "settlement", PaymentRef: "ord-1", Status: domain.PaymentStatusSettled, Amount: 1500})
	if recorder.intents["ord-1"].Status != domain.PaymentStatusPending {
		t.Fatal("a wrong amount must never mark the order paid")
	}
	if recorder.events[0].ProcessingStatus != "amount_mismatch" {
		t.Fatalf("processing status = %q", recorder.events[0].ProcessingStatus)
	}
	if len(sink.events) != 1 || sink.events[0].EventType != "payment.amount_mismatch" {
		t.Fatalf("ops must be told about the mismatch: %+v", sink.events)
	}
}

func TestOutOfOrderWebhookCannotRegressPaidOrder(t *testing.T) {
	intent := pendingIntent()
	intent.Status = domain.PaymentStatusSettled
	recorder, sink := processOne(t, intent, provider.WebhookEvent{ProviderEventID: "e3", EventType: "pending", PaymentRef: "ord-1", Status: domain.PaymentStatusPending})
	if recorder.intents["ord-1"].Status != domain.PaymentStatusSettled || recorder.events[0].ProcessingStatus != "stale" || len(sink.events) != 0 {
		t.Fatalf("stale event must be ignored: intent=%+v status=%q events=%d", recorder.intents["ord-1"], recorder.events[0].ProcessingStatus, len(sink.events))
	}
}

func TestLatePaymentAfterExpiryIsRecorded(t *testing.T) {
	intent := pendingIntent()
	intent.Status = domain.PaymentStatusExpired
	recorder, _ := processOne(t, intent, provider.WebhookEvent{ProviderEventID: "e4", EventType: "settlement", PaymentRef: "ord-1", Status: domain.PaymentStatusSettled, Amount: 150000})
	if recorder.intents["ord-1"].Status != domain.PaymentStatusSettled {
		t.Fatal("money that arrived after expiry must still be recorded")
	}
}

func TestWebhookFromAbandonedGatewayIsIgnored(t *testing.T) {
	intent := pendingIntent()
	intent.ProviderCode = domain.ProviderIPaymu // fallback moved the order
	recorder, sink := processOne(t, intent, provider.WebhookEvent{ProviderEventID: "e5", EventType: "expire", PaymentRef: "ord-1", Status: domain.PaymentStatusExpired})
	if recorder.intents["ord-1"].Status != domain.PaymentStatusPending || len(sink.events) != 0 {
		t.Fatal("events from a gateway the order no longer uses must not change it")
	}
}

type failingSink struct{ fail bool }

func (f *failingSink) EnqueueOutboundEvent(context.Context, domain.OutboundEvent) (string, error) {
	if f.fail {
		return "", errors.New("outbox unavailable")
	}
	return "evt", nil
}

func TestFailedEnqueueIsRecoveredByGatewayRedelivery(t *testing.T) {
	sqliteStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "wh.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	ctx := context.Background()
	if _, err := sqliteStore.UpsertPaymentIntent(ctx, pendingIntent()); err != nil {
		t.Fatal(err)
	}
	event := provider.WebhookEvent{ProviderEventID: "settlement:ord-1", EventType: "settlement", PaymentRef: "ord-1", Status: domain.PaymentStatusSettled, Amount: 150000}
	adapter := &testWebhookAdapter{
		verifyFn: func(context.Context, provider.WebhookRequest) error { return nil },
		parseFn:  func(context.Context, provider.WebhookRequest) (provider.WebhookEvent, error) { return event, nil },
	}
	handlers := map[domain.ProviderCode]provider.Adapter{domain.ProviderMidtrans: adapter}
	in := Input{Provider: domain.ProviderMidtrans, Request: provider.WebhookRequest{Body: []byte(`{}`)}}

	if _, err := sqliteStore.EnqueueOutboundEvent(ctx, domain.OutboundEvent{ID: "evt_taken", EventType: "seed", PayloadJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.MarkOutboundEventDelivered(ctx, "evt_taken", 1); err != nil {
		t.Fatal(err)
	}

	// The outbox insert fails inside the transaction: nothing is committed.
	broken := New(&brokenOutboxStore{sqliteStore}, nil, handlers).WithEvents(&failingSink{}, domain.EnvironmentSandbox)
	if res, _ := broken.Process(ctx, in); res.StatusCode != 500 {
		t.Fatalf("enqueue failure must ask the gateway to retry, got %d", res.StatusCode)
	}
	if intent, _ := sqliteStore.GetPaymentIntentByExternalRef(ctx, "ord-1"); intent.Status != domain.PaymentStatusPending {
		t.Fatalf("intent must roll back with the failed event, got %s", intent.Status)
	}

	// Gateway redelivers the same event; this time it goes through.
	healthy := New(sqliteStore, nil, handlers).WithEvents(sqliteStore, domain.EnvironmentSandbox)
	if res, err := healthy.Process(ctx, in); err != nil || res.StatusCode != 202 {
		t.Fatalf("redelivery should be processed, got %d %v", res.StatusCode, err)
	}
	if intent, _ := sqliteStore.GetPaymentIntentByExternalRef(ctx, "ord-1"); intent.Status != domain.PaymentStatusSettled {
		t.Fatalf("intent should now be settled, got %s", intent.Status)
	}
	if n, _ := sqliteStore.CountOutboundEvents(ctx, domain.OutboundEventPending); n != 1 {
		t.Fatalf("exactly one Wishpull event expected, got %d", n)
	}
	// A third delivery is a true duplicate.
	if res, _ := healthy.Process(ctx, in); res.Body["warning"] != "duplicate webhook event ignored" {
		t.Fatalf("third delivery should be a duplicate: %v", res.Body)
	}
}

// brokenOutboxStore makes the outbox half of the atomic write fail by
// reusing an existing outbox primary key, so the real transaction aborts.
type brokenOutboxStore struct{ *sqlite.Store }

func (b *brokenOutboxStore) UpsertPaymentIntentWithOutboundEvent(ctx context.Context, intent domain.PaymentIntent, event domain.OutboundEvent) error {
	event.ID = "evt_taken"
	return b.Store.UpsertPaymentIntentWithOutboundEvent(ctx, intent, event)
}
