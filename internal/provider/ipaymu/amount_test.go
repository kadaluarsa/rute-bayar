package ipaymu

import (
	"context"
	"testing"

	"github.com/pendig/rute-bayar/internal/provider"
)

func TestParseWebhookReportsAmountWhenPresent(t *testing.T) {
	adapter := New(WithVA("va"), WithAPIKey("key"))
	event, err := adapter.ParseWebhook(context.Background(), provider.WebhookRequest{Body: []byte("trx_id=1&reference_id=ord-1&status=berhasil&status_code=1&amount=51000")})
	if err != nil {
		t.Fatal(err)
	}
	if event.Amount != 51000 {
		t.Fatalf("amount = %d", event.Amount)
	}
}
