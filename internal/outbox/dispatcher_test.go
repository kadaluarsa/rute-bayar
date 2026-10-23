package outbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
	"github.com/pendig/rute-bayar/internal/storage/sqlite"
)

const secret = "0123456789abcdef0123456789abcdef"

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "outbox.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDispatchSignsDeliversAndRetries(t *testing.T) {
	store := openStore(t)
	var calls atomic.Int32
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(HeaderSignature)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	id, err := store.EnqueueOutboundEvent(context.Background(), domain.OutboundEvent{EventType: "payment.status_changed", OrderID: "ord-1", PayloadJSON: json.RawMessage(`{"type":"payment.status_changed","data":{"order_id":"ord-1","status":"paid"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDispatcher(store, Config{URL: srv.URL, Secret: secret, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if delivered, failed, err := d.DispatchOnce(context.Background()); err != nil || delivered != 0 || failed != 1 {
		t.Fatalf("first attempt should fail: delivered=%d failed=%d err=%v", delivered, failed, err)
	}
	// Leased/backed-off events are not due immediately; move the clock.
	d.now = func() time.Time { return time.Now().Add(time.Hour) }
	if delivered, _, err := d.DispatchOnce(context.Background()); err != nil || delivered != 1 {
		t.Fatalf("retry should deliver: delivered=%d err=%v", delivered, err)
	}
	if err := Verify(secret, gotSig, gotBody, d.now(), 5*time.Minute); err != nil {
		t.Fatalf("receiver-side verification failed: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"`+id+`"`) {
		t.Fatalf("body must carry the event id: %s", gotBody)
	}
	if err := Verify(secret, gotSig, append(gotBody, ' '), d.now(), 5*time.Minute); err == nil {
		t.Fatal("tampered body must fail verification")
	}
	events, _ := store.ListOutboundEvents(context.Background(), domain.OutboundEventDelivered, "ord-1", 10)
	if len(events) != 1 || events[0].Attempts != 2 {
		t.Fatalf("delivered event should record 2 attempts: %+v", events)
	}
	if _, _, err := d.DispatchOnce(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("delivered events must not be resent, calls=%d", calls.Load())
	}
}

func TestDispatchParksDeadEventsForRequeue(t *testing.T) {
	store := openStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	defer srv.Close()
	id, _ := store.EnqueueOutboundEvent(context.Background(), domain.OutboundEvent{EventType: "payment.status_changed", OrderID: "ord-2", PayloadJSON: json.RawMessage(`{"data":{}}`)})
	d, _ := NewDispatcher(store, Config{URL: srv.URL, Secret: secret, MaxAttempts: 2, BaseBackoff: time.Millisecond}, srv.Client(), nil)
	future := time.Now()
	for i := 0; i < 3; i++ {
		future = future.Add(time.Hour)
		at := future
		d.now = func() time.Time { return at }
		if _, _, err := d.DispatchOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	dead, _ := store.ListOutboundEvents(context.Background(), domain.OutboundEventDead, "", 10)
	if len(dead) != 1 || dead[0].Attempts != 2 || !strings.Contains(dead[0].LastError, "400") {
		t.Fatalf("event should be dead after 2 attempts: %+v", dead)
	}
	if err := store.RequeueOutboundEvent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountOutboundEvents(context.Background(), domain.OutboundEventPending); n != 1 {
		t.Fatalf("requeue should make it pending, got %d", n)
	}
}

func TestVerifyRejectsOldTimestamps(t *testing.T) {
	body := []byte(`{}`)
	header := Sign(secret, time.Now().Add(-time.Hour).Unix(), body)
	if err := Verify(secret, header, body, time.Now(), 5*time.Minute); err == nil {
		t.Fatal("replayed old signature must be rejected")
	}
}
