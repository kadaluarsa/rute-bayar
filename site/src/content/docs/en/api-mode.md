---
title: CLI API mode
description: Call official provider endpoints directly from rutebayar for debugging, experiments, and operations that do not have dedicated adapters yet.
lang: en
order: 4
---

`rutebayar api <provider>` is raw API mode. It calls official provider endpoints from the same CLI surface and uses credentials already onboarded into Rute Bayar.

Use this mode when you need to:

- inspect provider responses before adding adapter features;
- debug a specific payload, header, or query;
- call an official endpoint that does not have a dedicated command like `pay create/status`;
- compare sandbox and production behavior with stored credentials.

## Prerequisites

Onboard the provider first so auth headers can be filled automatically:

```bash
rutebayar onboard midtrans --merchant-id "$MIDTRANS_MERCHANT_ID" --client-key "$MIDTRANS_CLIENT_KEY" --server-key "$MIDTRANS_SERVER_KEY" --environment sandbox
rutebayar onboard xendit --secret-key "$XENDIT_SECRET_KEY" --environment sandbox
rutebayar onboard doku --client-id "$DOKU_CLIENT_ID" --secret-key "$DOKU_SECRET_KEY" --environment sandbox
rutebayar onboard ipaymu --va "$IPAYMU_VA" --api-key "$IPAYMU_API_KEY" --environment sandbox
rutebayar provider accounts
```

Use the same environment as the stored credential:

```bash
rutebayar api midtrans --environment sandbox --operation status --path-param order_id=rb-demo-001
```

## Command Shape

```bash
rutebayar api <provider> \
  --operation <alias> \
  --method <GET|POST|PATCH|DELETE> \
  --path-param key=value \
  --query key=value \
  --header "X-Debug=1" \
  --body '<json>'
```

Use `--operation` for aliases mapped from manual commands or provider specs. Use `--path` when an endpoint does not have an alias yet.

```bash
rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=INV-001
rutebayar api doku --method GET --path /orders/v1/status/INV-001
```

View available operations:

```bash
rutebayar api --help
rutebayar api midtrans --help
rutebayar api xendit --help
rutebayar api doku --help
rutebayar api ipaymu --help
```

## Provider Examples

Midtrans status and Snap:

```bash
rutebayar api midtrans --operation status --path-param order_id=rb-demo-001
rutebayar api midtrans --operation snap-transaction --method POST --body '<midtrans_snap_payload_json>'
```

Xendit balance and Payment Sessions:

```bash
rutebayar api xendit --operation auth-balance
rutebayar api xendit --operation session-status --path-param session_id=ps-123
```

DOKU Checkout and order status:

```bash
rutebayar api doku --operation checkout-payment --method POST --body '<doku_checkout_payload_json>'
rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=INV-001
rutebayar api doku --operation post-direct-debit-v1-token --method POST --body '<doku_direct_debit_payload_json>'
```

iPaymu channels and transaction:

```bash
rutebayar api ipaymu --operation payment-channels --method GET
rutebayar api ipaymu --operation transaction --method POST --body '<ipaymu_transaction_payload_json>'
```

## Quick Mapping

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

- `401` or `403`: check credentials, environment, and base URL.
- `400`: the payload does not match the provider schema yet.
- `invalid_signature` or `invalid_client_id`: usually DOKU credentials, timestamp, base URL, or JSON body mismatch.
- Operation not found: run `rutebayar api <provider> --help`, or use `--path`.
- Endpoint succeeds but transaction status does not change: check the provider lifecycle; API mode calls the official endpoint but does not force local payment state.

For normal payment flows, keep using `rutebayar pay create`, `rutebayar pay status`, and `rutebayar reconcile`. API mode is the raw path for inspection and additional provider endpoints.
