---
title: CLI API mode
description: Panggil endpoint resmi provider langsung dari rutebayar untuk debugging, eksperimen, dan operasi yang belum punya adapter khusus.
lang: id
order: 4
---

`rutebayar api <provider>` adalah mode raw API untuk memanggil endpoint resmi provider dari CLI yang sama dengan kredensial onboarding Rute Bayar.

Gunakan mode ini saat kamu perlu:

- mengecek response provider sebelum menambah fitur adapter;
- debugging payload, header, atau query tertentu;
- menjalankan endpoint resmi yang belum punya command khusus seperti `pay create/status`;
- membandingkan perilaku sandbox dan production dengan kredensial yang sudah tersimpan.

## Prasyarat

Onboard provider dulu agar header auth otomatis terisi:

```bash
rutebayar onboard midtrans --merchant-id "$MIDTRANS_MERCHANT_ID" --client-key "$MIDTRANS_CLIENT_KEY" --server-key "$MIDTRANS_SERVER_KEY" --environment sandbox
rutebayar onboard xendit --secret-key "$XENDIT_SECRET_KEY" --environment sandbox
rutebayar onboard doku --client-id "$DOKU_CLIENT_ID" --secret-key "$DOKU_SECRET_KEY" --environment sandbox
rutebayar onboard ipaymu --va "$IPAYMU_VA" --api-key "$IPAYMU_API_KEY" --environment sandbox
rutebayar provider accounts
```

Pakai environment yang sama dengan kredensial:

```bash
rutebayar api midtrans --environment sandbox --operation status --path-param order_id=rb-demo-001
```

## Struktur Command

```bash
rutebayar api <provider> \
  --operation <alias> \
  --method <GET|POST|PATCH|DELETE> \
  --path-param key=value \
  --query key=value \
  --header "X-Debug=1" \
  --body '<json>'
```

Gunakan `--operation` untuk alias yang sudah dipetakan dari command manual atau spec provider. Gunakan `--path` jika endpoint belum punya alias.

```bash
rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=INV-001
rutebayar api doku --method GET --path /orders/v1/status/INV-001
```

Lihat daftar operation yang tersedia:

```bash
rutebayar api --help
rutebayar api midtrans --help
rutebayar api xendit --help
rutebayar api doku --help
rutebayar api ipaymu --help
```

## Contoh provider

Midtrans status dan Snap:

```bash
rutebayar api midtrans --operation status --path-param order_id=rb-demo-001
rutebayar api midtrans --operation snap-transaction --method POST --body '<midtrans_snap_payload_json>'
```

Xendit balance dan Payment Sessions:

```bash
rutebayar api xendit --operation auth-balance
rutebayar api xendit --operation session-status --path-param session_id=ps-123
```

DOKU Checkout dan order status:

```bash
rutebayar api doku --operation checkout-payment --method POST --body '<doku_checkout_payload_json>'
rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=INV-001
rutebayar api doku --operation post-direct-debit-v1-token --method POST --body '<doku_direct_debit_payload_json>'
```

Kanal dan transaksi iPaymu:

```bash
rutebayar api ipaymu --operation payment-channels --method GET
rutebayar api ipaymu --operation transaction --method POST --body '<ipaymu_transaction_payload_json>'
```

## Pemetaan Cepat

| Provider | Operation | Method | Path |
| - | - | - | - |
| midtrans | status / check-status | GET | /v2/{order_id}/status |
| midtrans | charge / create | POST | /v2/charge |
| midtrans | snap / snap-transaction | POST | /snap/v1/transactions |
| xendit | auth-balance / balance | GET | /balance |
| xendit | session-create / create | POST | /sessions |
| xendit | session-status / status | GET | /sessions/{session_id} |
| doku | checkout / checkout-payment | POST | /checkout/v1/payment |
| doku | order-status / status | GET | /orders/v1/status/{invoice_number_or_request_id} |
| doku | direct-debit-v1-token / post-direct-debit-v1-token | POST | /direct-debit/v1/token |
| ipaymu | payment-channels / channels | GET | /api/v2/payment-channels |
| ipaymu | transaction | POST | /api/v2/transaction |

## Troubleshooting

- `401` atau `403`: cek kredensial, environment, dan base URL.
- `400`: payload belum memenuhi schema provider.
- `invalid_signature` atau `invalid_client_id`: biasanya kredensial DOKU, timestamp, base URL, atau body JSON tidak cocok.
- Operation tidak ditemukan: jalankan `rutebayar api <provider> --help`, atau pakai `--path`.
- Endpoint berhasil tapi status transaksi belum berubah: cek lifecycle provider; mode API hanya memanggil endpoint resmi, bukan memaksa status payment lokal.

Untuk flow payment umum, tetap gunakan `rutebayar pay create`, `rutebayar pay status`, dan `rutebayar reconcile`. API mode adalah jalur raw untuk inspeksi dan endpoint tambahan.
