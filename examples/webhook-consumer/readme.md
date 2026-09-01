# Reference webhook consumer

An executable definition of the receiving end of the content events (ADR-011).
A tenant's real consumer will be a different program in a different language;
this one exists so the contract has something that runs and something that
fails when the contract is broken.

It is **not** a platform service and is not in `docker-compose.yml`.

## The contract it implements

| Header | Meaning |
|---|---|
| `X-Webhook-Event` | `content.entry.created` / `.updated` / `.deleted` / `.published` / `.unpublished` |
| `X-Webhook-Delivery` | The outbox row id. A retry re-sends the **same** id — this is your dedup key. |
| `X-Webhook-Signature` | `sha256=<hex>` — HMAC-SHA256 over the **raw body bytes**, keyed with the secret from registration. |

Body:

```json
{"tenant_id":"acme","entry_id":"…","content_type":"article","locale":"default"}
```

The payload is thin on purpose. It names *what* changed and never what it
says: shipping the document would hand every receiver a second read path
around the field-level permissions and the draft/published distinction
(ADR-009). To get the content, read it back — see `mirror.go`.

## The four things a receiver must get right

1. **Verify before parsing.** `verify()` recomputes the HMAC over the exact
   bytes received and compares in constant time. Parsing first means running a
   parser on bytes from anyone who found the URL.
2. **Deduplicate on the delivery id.** Delivery is at-least-once: a delivery
   whose response never came back is re-sent with the same id. Recording the
   id *before* acting makes the action at-most-once; after, at-least-once.
   Pick per action — a cache purge is idempotent, an email is not.
3. **Answer fast, then work.** The sender times out at 10s, and the retry
   budget is shared by every endpoint the tenant registered, so a slow receiver
   spends other receivers' retries. This one acks and hands the work to a
   goroutine.
4. **Fetch through the edge, with an ETag.** The edge serves published content
   only, so a draft cannot leak through it. Its default posture is
   `public, no-cache` + strong ETag: send `If-None-Match` and unchanged
   entries come back `304` with no body.

Two consequences worth stating, because both look like bugs the first time:

- A `404` from the edge after `created`/`updated` is **normal** — the entry is
  a draft. It is not an error.
- `unpublished` and `deleted` drop the local copy **without asking the edge**.
  A takedown that waited for a cache to agree it was stale is precisely the
  failure ADR-011 removed.

## Run it against the dev stack

```bash
# 1. Register the webhook. The secret is in the 201 and appears ONCE.
curl -s -X POST http://localhost:8080/api/v1/content/webhooks \
  -H 'Content-Type: application/json' \
  -H 'X-User-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Tenant-Id: acme' -H 'X-Tenant-Role: owner' \
  -d '{"url":"http://host.docker.internal:9999/webhook","description":"reference consumer"}'

# 2. Run the consumer with that secret.
WEBHOOK_SECRET=<secret from the 201> \
DELIVERY_EDGE_URL=http://localhost:4100 \
  go run ./examples/webhook-consumer

# 3. Publish something. The consumer logs the mirror update; publish again
#    without changing the content and it logs a 304 instead.
```

`host.docker.internal` is how the containers reach a consumer running on the
host. A consumer running in compose would use its service name.

## Tests

```bash
go test ./examples/... -race
```

The tests cover the refusals (tampered body, foreign secret, malformed and
missing signature, verification-before-parsing), the at-least-once behaviour
(same id twice acts once; the dedup log stays bounded), that the response is
sent before the work finishes, and the edge interaction (ETag revalidation,
404-means-draft, takedown without a fetch, a failed refresh leaving the mirror
intact).
