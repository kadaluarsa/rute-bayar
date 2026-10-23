package midtrans

import (
	"context"
	"testing"

	"github.com/pendig/rute-bayar/internal/provider"
)

func TestParseWebhookReportsGrossAmount(t *testing.T) {
	adapter := New(WithServerKey("server_key"), WithBaseURL("https://example.com"))
	event, err := adapter.ParseWebhook(context.Background(), provider.WebhookRequest{Body: []byte(`{"order_id":"ord-1","transaction_id":"t1","transaction_status":"settlement","gross_amount":"150000.00"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if event.Amount != 150000 {
		t.Fatalf("amount = %d", event.Amount)
	}
}
