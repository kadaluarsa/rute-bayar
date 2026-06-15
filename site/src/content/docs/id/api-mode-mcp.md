---
title: QA API mode dan mapping MCP
description: Checklist validasi API mode serta mapping tool MCP yang aman untuk AI Agent.
lang: id
order: 5
---

Halaman ini menjelaskan cara memakai `rutebayar api <provider>` sebagai jalur QA dan tool call AI Agent/MCP tanpa merusak alur adapter utama.

API mode adalah CLI raw API. Ia memanggil endpoint resmi provider memakai kredensial yang sudah tersimpan lewat onboarding. HTTP API daemon tetap track berbeda.

## Checklist QA cepat

```bash
rutebayar api --help
rutebayar api midtrans --help
rutebayar api xendit --help
rutebayar api doku --help
rutebayar api ipaymu --help
rutebayar provider accounts
```

Mulai dari operasi read-only:

```bash
rutebayar api xendit --environment sandbox --operation auth-balance
rutebayar api midtrans --environment sandbox --operation status --path-param order_id=rb-demo-001
rutebayar api doku --environment sandbox --operation order-status --path-param invoice_number_or_request_id=RB-DEMO-001
rutebayar api ipaymu --environment sandbox --operation payment-channels --method GET
```

Untuk create/refund/cancel/expire, gunakan sandbox, reference unik, dan konfirmasi manual:

```bash
rutebayar api midtrans --environment sandbox --operation snap-transaction --method POST --body '<midtrans_snap_payload_json>'
rutebayar api xendit --environment sandbox --operation session-create --method POST --body '<xendit_session_payload_json>'
rutebayar api doku --environment sandbox --operation checkout-payment --method POST --body '<doku_checkout_payload_json>'
```

## Mapping MCP

| Tool MCP | Command CLI | Catatan |
| - | - | - |
| `provider_auth_check` | `rutebayar api xendit --operation auth-balance` | Pakai operasi non-mutating jika tersedia. |
| `payment_status` | `rutebayar api midtrans --operation status --path-param order_id=<id>` | Status langsung dari provider. |
| `payment_status` | `rutebayar api xendit --operation session-status --path-param session_id=<id>` | Untuk Payment Sessions. |
| `payment_status` | `rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=<id>` | Bisa invoice number atau request id. |
| `create_checkout` | `rutebayar api midtrans --operation snap-transaction --method POST --body '<json>'` | Mutating; perlu approval. |
| `create_checkout` | `rutebayar api xendit --operation session-create --method POST --body '<json>'` | Mutating; pakai reference unik. |
| `create_checkout` | `rutebayar api doku --operation checkout-payment --method POST --body '<json>'` | Mutating; sandbox default. |
| `payment_channels` | `rutebayar api ipaymu --operation payment-channels --method GET` | Discovery channel read-only. |
| `raw_provider_call` | `rutebayar api <provider> --method <METHOD> --path <path> --query k=v --header "K=V" --body '<json>'` | Fallback untuk endpoint tanpa alias. |

## Guardrail agent

- Jangan log secret key, client key, server key, token callback, atau signature penuh.
- Default ke `--environment sandbox` kecuali user eksplisit meminta production.
- Minta approval sebelum create payment, refund, cancel, expire, approve, deny, atau operasi mutating lain.
- Pakai reference/idempotency key unik seperti `rb-mcp-<timestamp>`.
- Simpan transcript yang sudah diredaksi: provider, operation, request id, status code, dan ringkasan error.
- Jika provider sukses tapi status lokal belum berubah, lanjutkan dengan `rutebayar pay status` atau `rutebayar reconcile`.

## Kapan tetap memakai adapter

Untuk flow payment umum, gunakan command adapter:

```bash
rutebayar pay create --provider <provider> ...
rutebayar pay status --provider <provider> --reference <reference>
rutebayar pay refund --provider <provider> ...
rutebayar reconcile --provider <provider> ...
```

API mode paling cocok untuk inspeksi provider, eksperimen endpoint tambahan, dan menyiapkan mapping sebelum dipromosikan ke command utama.
