package xendit

import (
	"context"
	"testing"

	"github.com/pendig/rute-bayar/internal/provider"
)

func TestParseWebhookReportsPaymentAmountButNotRefundAmount(t *testing.T) {
	adapter := New(WithSecretKey("secret"))
	event, err := adapter.ParseWebhook(context.Background(), provider.WebhookRequest{Body: []byte(`{"event":"payment_session.completed","data":{"payment_session_id":"ps-1","reference_id":"ord-1","status":"COMPLETED","amount":1500000}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if event.Amount != 1500000 {
		t.Fatalf("payment amount = %d", event.Amount)
	}
	refund, err := adapter.ParseWebhook(context.Background(), provider.WebhookRequest{Body: []byte(`{"event":"refund.succeeded","data":{"id":"rfd-1","reference_id":"ord-1","status":"SUCCEEDED","amount":5000}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if refund.Amount != 0 {
		t.Fatalf("refund events must not report a payment amount, got %d", refund.Amount)
	}
}
