package doku

import (
	"context"
	"testing"

	"github.com/pendig/rute-bayar/internal/provider"
)

func TestParseWebhookReportsOrderAmount(t *testing.T) {
	adapter := New(WithClientID("client-id"), WithSecretKey("secret"))
	event, err := adapter.ParseWebhook(context.Background(), provider.WebhookRequest{Body: []byte(`{"order":{"invoice_number":"rb-1","amount":1500000},"transaction":{"status":"SUCCESS"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if event.Amount != 1500000 {
		t.Fatalf("amount = %d", event.Amount)
	}
}
