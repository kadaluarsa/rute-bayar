# API Mode QA and MCP Mapping

Panduan ini menjadikan `rutebayar api <provider>` aman dipakai untuk QA, debugging, dan tool call dari AI Agent/MCP tanpa mencampuradukkan mode CLI raw API dengan rencana HTTP API daemon.

## Ruang Lingkup

- Berlaku untuk CLI raw API mode: `rutebayar api <provider>`.
- Menggunakan kredensial provider yang sudah tersimpan lewat `rutebayar onboard`.
- Cocok untuk smoke test provider, validasi payload, dan operasi resmi provider yang belum punya command adapter khusus.
- Tidak menggantikan HTTP API daemon yang masih menjadi track terpisah.

## Checklist QA

Jalankan help dan daftar akun lebih dulu:

```bash
rutebayar api --help
rutebayar api midtrans --help
rutebayar api xendit --help
rutebayar api doku --help
rutebayar api ipaymu --help
rutebayar provider accounts
```

Gunakan operasi read-only untuk smoke test awal:

```bash
rutebayar api xendit --environment sandbox --operation auth-balance
rutebayar api midtrans --environment sandbox --operation status --path-param order_id=rb-demo-001
rutebayar api doku --environment sandbox --operation order-status --path-param invoice_number_or_request_id=RB-DEMO-001
rutebayar api ipaymu --environment sandbox --operation payment-channels --method GET
```

Untuk endpoint create/refund/expire/cancel, pakai reference unik dan approval manual:

```bash
rutebayar api midtrans --environment sandbox --operation snap-transaction --method POST --body '<midtrans_snap_payload_json>'
rutebayar api xendit --environment sandbox --operation session-create --method POST --body '<xendit_session_payload_json>'
rutebayar api doku --environment sandbox --operation checkout-payment --method POST --body '<doku_checkout_payload_json>'
```

## Mapping MCP yang Disarankan

| Tool MCP | Command CLI | Catatan |
| - | - | - |
| `provider_auth_check` | `rutebayar api xendit --operation auth-balance` | Gunakan operasi non-mutating jika tersedia. |
| `payment_status` | `rutebayar api midtrans --operation status --path-param order_id=<id>` | Status provider langsung, bukan state lokal SQLite. |
| `payment_status` | `rutebayar api xendit --operation session-status --path-param session_id=<id>` | Cocok untuk Payment Sessions. |
| `payment_status` | `rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=<id>` | Pakai invoice number atau request id DOKU. |
| `create_checkout` | `rutebayar api midtrans --operation snap-transaction --method POST --body '<json>'` | Mutating; minta konfirmasi user. |
| `create_checkout` | `rutebayar api xendit --operation session-create --method POST --body '<json>'` | Mutating; gunakan reference unik. |
| `create_checkout` | `rutebayar api doku --operation checkout-payment --method POST --body '<json>'` | Mutating; sandbox default. |
| `payment_channels` | `rutebayar api ipaymu --operation payment-channels --method GET` | Read-only channel discovery. |
| `raw_provider_call` | `rutebayar api <provider> --method <METHOD> --path <path> --query k=v --header "K=V" --body '<json>'` | Fallback saat alias belum ada. |

## Guardrail untuk Agent

- Jangan tampilkan secret key, client key, server key, token callback, atau signature penuh di log publik.
- Default ke `--environment sandbox` kecuali user eksplisit meminta production.
- Wajib minta approval user sebelum create payment, refund, cancel, expire, approve, deny, atau endpoint mutating lain.
- Gunakan reference/idempotency key unik per percobaan, misalnya prefix `rb-mcp-<timestamp>`.
- Simpan transcript yang sudah diredaksi untuk debugging, terutama request id, operation, provider, status code, dan response error ringkas.
- Jika response provider sukses tetapi status lokal belum berubah, jalankan `rutebayar pay status` atau `rutebayar reconcile` untuk alur adapter lokal.

## Kapan Memakai Command Adapter

Gunakan command adapter jika alurnya sudah didukung:

```bash
rutebayar pay create --provider <provider> ...
rutebayar pay status --provider <provider> --reference <reference>
rutebayar pay refund --provider <provider> ...
rutebayar reconcile --provider <provider> ...
```

Gunakan API mode saat perlu membandingkan payload resmi provider, mengecek endpoint tambahan, atau menyiapkan mapping sebelum fitur adapter dipromosikan ke command utama.
