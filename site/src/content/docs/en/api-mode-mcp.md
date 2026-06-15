---
title: API mode QA and MCP mapping
description: API mode validation checklist and safe MCP tool mapping for AI Agents.
lang: en
order: 5
---

This page explains how to use `rutebayar api <provider>` as a QA path and AI Agent/MCP tool call surface without disturbing the main adapter flow.

API mode is raw CLI API mode. It calls official provider endpoints with credentials stored through onboarding. The HTTP API daemon remains a separate track.

## Quick QA checklist

```bash
rutebayar api --help
rutebayar api midtrans --help
rutebayar api xendit --help
rutebayar api doku --help
rutebayar api ipaymu --help
rutebayar provider accounts
```

Start with read-only operations:

```bash
rutebayar api xendit --environment sandbox --operation auth-balance
rutebayar api midtrans --environment sandbox --operation status --path-param order_id=rb-demo-001
rutebayar api doku --environment sandbox --operation order-status --path-param invoice_number_or_request_id=RB-DEMO-001
rutebayar api ipaymu --environment sandbox --operation payment-channels --method GET
```

For create/refund/cancel/expire calls, use sandbox, unique references, and manual approval:

```bash
rutebayar api midtrans --environment sandbox --operation snap-transaction --method POST --body '<midtrans_snap_payload_json>'
rutebayar api xendit --environment sandbox --operation session-create --method POST --body '<xendit_session_payload_json>'
rutebayar api doku --environment sandbox --operation checkout-payment --method POST --body '<doku_checkout_payload_json>'
```

## MCP mapping

| MCP tool | CLI command | Notes |
| - | - | - |
| `provider_auth_check` | `rutebayar api xendit --operation auth-balance` | Prefer non-mutating operations when available. |
| `payment_status` | `rutebayar api midtrans --operation status --path-param order_id=<id>` | Direct provider status. |
| `payment_status` | `rutebayar api xendit --operation session-status --path-param session_id=<id>` | For Payment Sessions. |
| `payment_status` | `rutebayar api doku --operation order-status --path-param invoice_number_or_request_id=<id>` | Accepts invoice number or request id. |
| `create_checkout` | `rutebayar api midtrans --operation snap-transaction --method POST --body '<json>'` | Mutating; approval required. |
| `create_checkout` | `rutebayar api xendit --operation session-create --method POST --body '<json>'` | Mutating; use a unique reference. |
| `create_checkout` | `rutebayar api doku --operation checkout-payment --method POST --body '<json>'` | Mutating; default to sandbox. |
| `payment_channels` | `rutebayar api ipaymu --operation payment-channels --method GET` | Read-only channel discovery. |
| `raw_provider_call` | `rutebayar api <provider> --method <METHOD> --path <path> --query k=v --header "K=V" --body '<json>'` | Fallback for endpoints without aliases. |

## Agent guardrails

- Do not log secret keys, client keys, server keys, callback tokens, or full signatures.
- Default to `--environment sandbox` unless the user explicitly requests production.
- Ask for approval before create payment, refund, cancel, expire, approve, deny, or any other mutating operation.
- Use unique references or idempotency keys such as `rb-mcp-<timestamp>`.
- Store redacted transcripts with provider, operation, request id, status code, and short error summaries.
- If the provider succeeds but local state has not changed, follow up with `rutebayar pay status` or `rutebayar reconcile`.

## When to keep using adapters

For normal payment flows, use adapter commands:

```bash
rutebayar pay create --provider <provider> ...
rutebayar pay status --provider <provider> --reference <reference>
rutebayar pay refund --provider <provider> ...
rutebayar reconcile --provider <provider> ...
```

API mode is best for provider inspection, endpoint experiments, and preparing mappings before they are promoted into the main command surface.
