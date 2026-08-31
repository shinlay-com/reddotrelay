# Webhook consumer contract

RedDotRelay delivers each event with `POST`, `Content-Type: application/json`,
and an `Idempotency-Key` equal to the body’s deterministic `eventId`. Delivery is
at-least-once: persist the idempotency key with the side effect in one consumer
transaction and return any `2xx` response only after that transaction commits.

When HMAC is configured, authenticate the request before decoding or logging the
body:

1. Read the exact raw body bytes with a strict size limit.
2. Parse `RedDotRelay-Timestamp` as canonical Unix seconds and reject requests
   outside a small clock-skew window, such as five minutes.
3. Require `RedDotRelay-Signature` to be `v1=` plus 64 lowercase hexadecimal
   characters.
4. Calculate HMAC-SHA256 over `timestamp + "." + rawBody` using the shared key.
5. Decode the received digest and compare it with `hmac.Equal` or an equivalent
   constant-time comparison.
6. If configured, use `RedDotRelay-Key-Id` only to select a key; it is public and
   is not authentication by itself.

The compiling Go example is [`examples/hmacverify`](../examples/hmacverify).
Its test uses production-format headers and proves that changed bodies,
signatures, and stale timestamps are rejected. Preserve raw bytes: parsing and
re-serializing JSON before verification changes the signed message.

During key rotation, accept the old and new key IDs for a bounded overlap while
RedDotRelay is updated to the new referenced key. Remove the old key after all
queued deliveries using it have completed or their snapshot authentication has
been intentionally requeued under an approved procedure.

Never log signing keys or full credential-bearing headers. Rate-limit before
expensive processing, require HTTPS, bound request bodies, and monitor rejected
timestamps/signatures without recording protected values.
