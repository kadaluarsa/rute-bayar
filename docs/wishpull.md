# Rute Bayar untuk Wishpull (PRD/TRD v0.3)

Dokumen ini menjelaskan cara Rute Bayar dipakai sebagai **Indonesia Universal Payment Gateway MCP Router** untuk Wishpull: komponen apa saja yang ditambahkan, kontrak API/MCP, cara Temporal worker memanggilnya, dan batasan yang masih ada.

## 1. Posisi dalam arsitektur Wishpull

```
 Wishpull app / agen eksternal
          │  (MCP, agent key)                         ┌──────────────────────────┐
          ▼                                           │  Rute Bayar (Go, 1 pod)  │
 LangGraph / hosted agent ── get_optimized_payment_route ──▶ router  (RP-1, RP-3) │
          │                 generate_secure_routing_token ─▶ routetoken (RP-2)   │
          │ token                                     │                          │
          ▼                                           │                          │
 Temporal OrderWorkflow ─ POST /routes/execute ──────────▶ mandateCovers (EX-3)  │
   (executor key, EX-1)                               │   └▶ gateway + fallback  │──▶ Midtrans / Xendit /
          ▲                                           │       (EX-2)             │    DOKU / iPaymu
          │ signal paymentCallback                    │                          │
 Wishpull webhook ingress ◀── event bertanda tangan ──── outbox ◀── webhook ◀──────── callback PG
                              (HMAC, retry backoff)   └──────────────────────────┘
```

Prinsip yang dijaga kode ini:

| Prinsip PRD v0.3 | Penerapan |
| --- | --- |
| AI vs ledger terpisah | Agen hanya punya `agent key`: quote, token, health, status teredaksi. Tidak ada tool MCP yang bisa membuat/menangkap/me-refund pembayaran. |
| Deterministic money path (EX-1) | Hanya `executor key` (dipegang Temporal worker) yang bisa memanggil `POST /api/v1/routes/execute`. `RUTE_BAYAR_REQUIRE_MANDATE=true` menutup `POST /api/v1/payments`. |
| mandateCovers (EX-3) | Sebelum gateway dipanggil: order, merchant, cart hash, nominal, mata uang, metode, gateway, kedaluwarsa dan pencabutan mandat harus cocok persis dengan token. |
| Vendor agnostic (EX-2) | Circuit breaker per gateway + fallback yang aman terhadap double-charge (lihat §5). |
| Anti-tampering token | Token `rbt1.*` di-HMAC-SHA256, mendukung rotasi `kid`, umur dibatasi oleh kedaluwarsa mandat. |
| Trusted Surface (TR-6) | URL 3DS/OTP/hosted page hanya ada di respons `execute` (untuk Temporal → app native), tidak pernah di respons MCP. |

## 2. Peran API key

Ketiga key wajib berbeda dan minimal 24 karakter (daemon menolak start jika tidak).

| Env | Pemegang | Boleh |
| --- | --- | --- |
| `RUTE_BAYAR_AGENT_API_KEY` | Agen AI (hosted, MCP eksternal) | `POST /api/v1/routes/quote`, `POST /api/v1/routes/token`, `GET /api/v1/gateways/health`, `POST /mcp` |
| `RUTE_BAYAR_EXECUTOR_API_KEY` | Temporal worker Wishpull | semua milik agen + `POST /api/v1/routes/execute`, `GET /api/v1/routes/executions/{order_id}` |
| `RUTE_BAYAR_API_KEY` | Operator | endpoint lama (provider account, refund, forwarding, reconcile) + `GET /api/v1/outbound-events`, `POST /api/v1/outbound-events/{id}/requeue`. **Tidak** bisa execute. |

Key dikirim lewat `X-API-Key` atau `Authorization: Bearer …` (bentuk yang dipakai klien MCP).

## 3. Alur pembayaran

1. **Quote (RP-1).** Agen memanggil `get_optimized_payment_route` dengan nominal utuh dan metode.
2. **Token (RP-2).** Setelah cart merchant-signed dan mandat AP2 ada, agen memanggil `generate_secure_routing_token`. Router menghitung ulang rute di server (rute dari klien tidak dipercaya) lalu mengunci daftar gateway ke dalam token.
3. **Execute (EX-1..3).** Temporal activity mengirim token + snapshot mandat yang sudah diverifikasi mandate service. Hasilnya idempoten per `order_id`.
4. **Challenge (TR-6).** Bila `challenge_required=true`, workflow mengirim `next_action.url` ke app (secure web view) lalu menunggu signal.
5. **Callback.** Webhook PG diverifikasi tanda tangannya, nominal dicek terhadap intent, status hanya boleh maju, lalu event ternormalisasi masuk outbox dan dikirim ke ingress Wishpull dengan HMAC.

### Contoh

```bash
# 1. Quote
curl -s localhost:8080/api/v1/routes/quote -H "X-API-Key: $AGENT_KEY" \
  -d '{"amount":150000,"preferred_method":"va_mandiri"}'
# → data.recommended_gateway, estimated_fee, gateway_status, fallbacks[], rejected[], fallback_policy

# 2. Token
curl -s localhost:8080/api/v1/routes/token -H "X-API-Key: $AGENT_KEY" -d '{
  "order_id":"WP-1001","mandate_id":"mdt-1001","merchant_id":"mer-7",
  "cart_hash":"sha256:…","amount":150000,"preferred_method":"va_mandiri",
  "mandate_expires_at":"2026-10-23T08:30:00Z"}'
# → data.routing_token, pinned_gateways, expires_at

# 3. Execute (hanya executor key)
curl -s localhost:8080/api/v1/routes/execute -H "X-API-Key: $EXECUTOR_KEY" -d '{
  "routing_token":"rbt1.…",
  "mandate":{"mandate_id":"mdt-1001","order_id":"WP-1001","merchant_id":"mer-7",
             "cart_hash":"sha256:…","amount":150000,"currency":"IDR",
             "expires_at":"2026-10-23T08:30:00Z","revoked":false},
  "customer":{"name":"Budi","email":"budi@example.com"}}'
```

Metode (`preferred_method`): `qris`, `va_<bank>` (`va_bca`, `va_bni`, `va_bri`, `va_mandiri`, `va_permata`, …), `direct_debit_<bank>`, `credit_card`, `ewallet_<wallet>` (`ewallet_ovo`, `ewallet_dana`, `ewallet_shopeepay`, `ewallet_linkaja`). Nominal harus bilangan bulat rupiah; pecahan ditolak, tidak dibulatkan.

### Hasil `execute`

Selalu HTTP 200 dengan `data.status`:

| `status` | Arti | Tindakan Temporal |
| --- | --- | --- |
| `issued` | Gateway menerima; `instrument` + `next_action` tersedia | `show_va`/`show_qr` → tampilkan; `redirect` → kirim URL ke app, tunggu signal |
| `mandate_rejected` | mandateCovers gagal, **tidak ada gateway yang dipanggil**; `violations[]` berisi semua alasan | Non-retryable. Kembalikan order ke Awaiting Approval / batalkan |
| `failed` | Semua gateway menolak secara definitif (4xx) atau circuit terbuka | Boleh coba lagi dengan **token baru** |
| `requires_reconcile` | Gateway mungkin sudah membuat pembayaran tanpa konfirmasi; `reconcile_providers[]` | Jangan retry. Jalankan `POST /api/v1/reconcile/{provider}/{order_id}` |

Error HTTP: `422 invalid_routing_token` (palsu/kedaluwarsa), `409 conflict` (eksekusi lain untuk order yang sama sedang berjalan — retry dengan backoff), `409 environment_mismatch`, `503` (token key belum diset).

Pemanggilan ulang dengan token yang sama (retry activity) mengembalikan hasil tersimpan dengan `replayed: true` tanpa memanggil gateway lagi. Token lain untuk order yang sudah `issued` juga hanya mendapat hasil lama — satu order tidak pernah menghasilkan dua tagihan.

## 4. MCP (`wishpull-indonesia-pg-router-mcp`)

Transport: Streamable HTTP di `POST /mcp` (respons JSON, tanpa SSE) dan stdio (`rutebayar mcp`). Versi protokol: `2025-06-18`, `2025-03-26`, `2024-11-05`.

| Tool | Sifat |
| --- | --- |
| `get_optimized_payment_route` | read-only |
| `generate_secure_routing_token` | membuat token; tidak memindahkan uang |
| `get_gateway_health` | read-only |
| `get_payment_status` | read-only, teredaksi (tanpa nomor VA, QR, URL 3DS) |
| `list_payment_methods` | read-only |

Contoh konfigurasi klien MCP (HTTP):

```json
{
  "mcpServers": {
    "wishpull-pg-router": {
      "type": "http",
      "url": "https://rute-bayar.internal/mcp",
      "headers": { "Authorization": "Bearer ${RUTE_BAYAR_AGENT_API_KEY}" }
    }
  }
}
```

Lokal (stdio): `rutebayar mcp --environment sandbox --db ./rute-bayar.sqlite3`.

## 5. Kebijakan fallback (EX-2)

| Jenis kegagalan | Contoh | Metode push (VA, QRIS, hosted page) | Charge langsung (kartu dengan token SDK, direct debit langsung) |
| --- | --- | --- | --- |
| `not_sent` | DNS, connection refused, circuit open, error validasi lokal | fallback | fallback |
| `rejected` | HTTP 4xx dari gateway | berhenti (`failed`) | berhenti (`failed`) |
| `ambiguous` | timeout, 5xx, koneksi putus setelah kirim | fallback (instrumen pertama tidak pernah ditunjukkan ke user, dibiarkan kedaluwarsa) | **berhenti** → `requires_reconcile` |

- Timeout per percobaan: `latency_budget_ms` (default 3000, sesuai PRD) untuk push, `pull_method_timeout_ms` (default 20000) untuk charge langsung.
- Breaker: 3 kegagalan berturut-turut (termasuk panggilan > 3000 ms) membuka circuit 5 menit, lalu satu probe (half-open).
- Card token Midtrans tidak pernah dikirim ke gateway lain.
- Setelah fallback, webhook dari gateway yang ditinggalkan diabaikan (`unmatched`) karena intent sudah milik gateway baru.

Tabel aturan dan MDR kontrak ada di `deploy/config/routing.example.json` (`RUTE_BAYAR_ROUTING_CONFIG`). MDR default hanya placeholder indikatif — ganti dengan angka kontrak Wishpull. Biaya handling Wishpull (open question PRD) diatur lewat `handling_fee_basis_points` / `handling_fee_fixed_idr`.

## 6. Event ke Wishpull

Diaktifkan dengan `RUTE_BAYAR_WISHPULL_EVENTS_URL` + `RUTE_BAYAR_WISHPULL_EVENTS_SECRET` (≥ 32 karakter). Event disimpan di tabel `outbound_events` (outbox) lalu dikirim dengan retry exponential backoff (2 dtk … 15 mnt, 15 percobaan). Setelah itu event menjadi `dead` dan bisa di-requeue operator.

```http
POST <RUTE_BAYAR_WISHPULL_EVENTS_URL>
X-RuteBayar-Event-Id: evt_…
X-RuteBayar-Event-Type: payment.status_changed
X-RuteBayar-Signature: t=1761210000,v1=<hex hmac-sha256(secret, "<t>.<body>")>

{"id":"evt_…","type":"payment.status_changed","schema_version":1,"environment":"production",
 "occurred_at":"…","data":{"order_id":"WP-1001","gateway":"midtrans","status":"settled",
 "previous_status":"pending","expected_amount":150000,"reported_amount":150000,
 "amount_verified":true,"currency":"IDR","webhook_event_id":"webhook_…"}}
```

Tipe event: `payment.status_changed`, `payment.amount_mismatch` (nominal dari PG ≠ nominal intent; status **tidak** diubah — perlu investigasi ops), `refund.status_changed`.

Verifikasi di ingress Wishpull (TypeScript), lalu teruskan sebagai signal ke workflow:

```ts
import { createHmac, timingSafeEqual } from "node:crypto";

export function verifyRuteBayar(rawBody: Buffer, header: string, secret: string, toleranceSec = 300): boolean {
  const parts = Object.fromEntries(header.split(",").map((p) => p.trim().split("=", 2)));
  const t = Number(parts.t);
  if (!t || Math.abs(Date.now() / 1000 - t) > toleranceSec || !parts.v1) return false;
  const expected = createHmac("sha256", secret).update(`${t}.`).update(rawBody).digest();
  const given = Buffer.from(parts.v1, "hex");
  return given.length === expected.length && timingSafeEqual(given, expected);
}

// handler: dedupe dengan event.id, lalu
// await client.workflow.getHandle(event.data.order_id).signal(paymentCallbackSignal, event.data.status);
```

Catatan: event bisa terkirim lebih dari sekali (at-least-once) — dedupe dengan `id`. Perlakukan `settled`/`paid`/`captured` dengan `amount_verified=true` sebagai lunas; jika `amount_verified=false` (PG tidak melaporkan nominal), lakukan status inquiry sebelum memenuhi order.

## 7. Contoh Temporal activity

```ts
export async function executePaymentActivity(input: { routingToken: string; mandate: VerifiedMandate }) {
  const res = await fetch(`${RUTE_BAYAR_URL}/api/v1/routes/execute`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-API-Key": process.env.RUTE_BAYAR_EXECUTOR_API_KEY! },
    body: JSON.stringify({
      routing_token: input.routingToken,
      mandate: {
        mandate_id: input.mandate.id, order_id: input.mandate.orderId, merchant_id: input.mandate.merchantId,
        cart_hash: input.mandate.cartHash, amount: input.mandate.amount, currency: "IDR",
        expires_at: input.mandate.expiresAt, revoked: input.mandate.revoked,
      },
    }),
  });
  if (res.status === 409) throw new Error("execution in progress"); // retryable
  if (res.status === 422) throw ApplicationFailure.nonRetryable("invalid routing token");
  const { data } = await res.json();
  switch (data.status) {
    case "issued": return data; // next_action: show_va | show_qr | redirect
    case "mandate_rejected": throw ApplicationFailure.nonRetryable(data.violations.join("; "));
    case "failed": throw ApplicationFailure.nonRetryable(data.message, "PaymentFailed");
    case "requires_reconcile": throw ApplicationFailure.nonRetryable(data.message, "RequiresReconcile");
  }
}
```

## 8. Konfigurasi

Semua variabel ada di `.env.example`. Yang wajib untuk produksi:

- `RUTE_BAYAR_ENV=production`, `RUTE_BAYAR_REQUIRE_MANDATE=true`
- tiga API key, `RUTE_BAYAR_ROUTING_TOKEN_KEYS` (`kid:base64`, buat dengan `openssl rand -base64 32`; untuk rotasi taruh kid baru di depan dan pertahankan kid lama sampai token lama kedaluwarsa)
- `RUTE_BAYAR_WISHPULL_EVENTS_URL` + `_SECRET`
- kredensial gateway lewat `RUTE_BAYAR_ACCOUNT_<PROVIDER>_PRODUCTION_CREDENTIAL_JSON` (di-mount dari Secret Manager) agar secret key tidak tersimpan di SQLite.

Daemon mencetak peringatan saat start di produksi bila salah satu hal penting di atas belum diset.

## 9. Deploy

- Image: `Dockerfile` (distroless, non-root, `webhook serve --mode all`).
- Manifest: `deploy/k8s/rute-bayar.yaml` — StatefulSet **1 replika** di GKE Autopilot `asia-southeast2` dengan persistent disk untuk SQLite (residensi data sesuai UU PDP).
- Shutdown halus pada SIGTERM (`RUTE_BAYAR_SHUTDOWN_GRACE`, default 20s).
- Observabilitas: `GET /metrics` (format Prometheus; proteksi opsional `RUTE_BAYAR_METRICS_TOKEN`). Metrik penting: `rutebayar_route_executions_total{status}`, `rutebayar_gateway_calls_total{provider,outcome}`, `rutebayar_gateway_call_duration_seconds`, `rutebayar_gateway_fallbacks_total`, `rutebayar_gateway_breaker_open`, `rutebayar_mandate_rejections_total`, `rutebayar_webhooks_total{processing_status}`, `rutebayar_outbox_events{status}`.
- Alert yang disarankan: `rutebayar_outbox_events{status="dead"} > 0`, kenaikan `rutebayar_webhooks_total{processing_status="amount_mismatch"}`, `rutebayar_route_executions_total{status="requires_reconcile"}`, `rutebayar_gateway_breaker_open == 1`.
- Backup SQLite: jalankan Litestream sebagai sidecar yang mereplikasi ke bucket GCS di `asia-southeast2`.

## 10. Batasan yang masih terbuka

- **Storage masih SQLite** → hanya 1 replika. Kunci eksekusi per order dan outbox ada di database, jadi multi-replika butuh port storage ke Cloud SQL Postgres (langkah berikutnya untuk HA).
- **State breaker per proses**; dengan 1 replika ini tidak masalah.
- **Refund DOKU dan iPaymu** masih nonaktif seperti di upstream; refund Midtrans/Xendit tersedia lewat endpoint admin.
- **Disbursement/payout** (Xendit Disbursement, Midtrans Iris) belum ada. Sesuai PRD v0.3, Wishpull bersifat non-custodial, jadi payout sebaiknya tetap diatur di sisi merchant/PG; tambahkan hanya bila model bisnis berubah.
- **Direct debit BCA OneKlik native (linked account)** belum ada; adapter Xendit memakai Payment Session (hosted page) untuk direct debit.
- **OpenTelemetry tracing** belum ada; korelasi saat ini lewat `X-Request-ID`, `order_id` dan audit log.
- MDR default dan field nominal webhook iPaymu (`amount`) perlu dikonfirmasi dengan kontrak/dokumentasi resmi.
