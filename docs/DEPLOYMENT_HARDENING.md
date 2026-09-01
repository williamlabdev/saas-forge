# Deployment Hardening Checklist

A concrete, do-this list that complements [`SECURITY.md`](../SECURITY.md).
`SECURITY.md` explains *why* the development defaults are unsafe; this document is
the checklist for turning them off.

Everything below is real: each variable is read in
`internal/pkg/config/runtime.go`, `internal/pkg/config/config.go`,
`apps/delivery/internal/config/config.go` or
`apps/cmsmcp/internal/config/config.go`, and a copyable template lives in
[`.env.production.example`](../.env.production.example).

Work top to bottom before exposing anything.

---

## 0. `APP_ENV` — the switch that arms the rest

- [ ] **Leave `APP_ENV` unset, or set it to `production`.** It defaults to
      `production`, and that default is deliberate: an operator who never heard of
      this variable gets the guards, and the person who wanted them off had to say so.

With `APP_ENV=production`, `ValidateRuntime` **refuses to start** — not warns — on
any of these:

| Refusal | Why |
|---|---|
| `AUTH_DEV_HEADERS=true` | `X-User-*` / `X-Tenant-*` headers would grant forgeable identities |
| `AUTHZ_MODE=allow` (or empty) | every authorization check would pass |
| `JWT_SECRET_HEX` = the `.env.example` throwaway | the key is published in this repository; anyone can sign a token |
| `DELIVERY_JWT_SECRET_HEX` = `JWT_SECRET_HEX` | an identical key defeats the whole point of the separation |
| `DELIVERY_JWT_SECRET_HEX` = the `.env.example` throwaway | same as above |
| `ENCRYPTION_KEY_HEX` / `BLIND_INDEX_PEPPER_HEX` = the throwaways | anyone can decrypt the PII at rest |

The length checks (`JWT_SECRET_HEX` ≥ 32 bytes, `ENCRYPTION_KEY_HEX` exactly 32
bytes, `BLIND_INDEX_PEPPER_HEX` ≥ 16 bytes) apply in **every** environment.

`docker-compose.yml` sets `APP_ENV=development` explicitly, so local work is
unaffected — and the declaration is visible in the file rather than implied by
a default.

---

## 1. Authorization mode

- [ ] **Set `AUTHZ_MODE=rbac` or `AUTHZ_MODE=opa`.** `allow` makes *every* check
      pass; production refuses to boot with it (§0).
- [ ] For each of your own domains, add rules for its actions (e.g.
      `invoice:list|read|create|update|delete`) in
      `internal/pkg/authz/rbac_authorizer.go` and/or the Rego under
      `internal/pkg/authz/policies/`. Both engines deny by default —
      `rbac_authorizer.go` falls through to forbidden, and `authz.rego` starts at
      `default allow := false` — so a domain with no rules is closed, not open.
- [ ] **Do not treat an action rule as record-level protection.** An authorizer is
      asked "may this subject read tickets?", never "may this subject read *this*
      ticket" — it is not handed the record. Per-owner or per-tenant checks belong
      in the service, next to the row it just loaded.
- [ ] Keep authorization as defence in depth: every domain service re-checks
      through its `Authorizer`. Do not remove those in favour of a gateway-only gate.

## 2. Authentication

- [ ] **Set `AUTH_DEV_HEADERS=false`.** When `true`, `X-User-Id` / `X-User-Roles` /
      `X-Tenant-*` are trusted **without a JWT** (`internal/pkg/authn/middleware.go`).
      Production refuses to boot with it (§0).
- [ ] Generate a fresh `JWT_SECRET_HEX` (`openssl rand -hex 32`). Identity then
      comes only from a verified JWT.
- [ ] Set `JWT_ACCESS_TTL_MINUTES` and `JWT_REFRESH_TTL_DAYS` to values you can
      live with. An access token is not revocable before it expires; a refresh
      token is (revoking it is what ends a session).
- [ ] Set `AUTH_LOGIN_RATE_LIMIT` / `AUTH_LOGIN_RATE_WINDOW_SEC`. The limiter is
      **in-process**, so the effective ceiling is *replicas × limit* — size it
      against your replica count, and put a shared limiter at the gateway if you
      need a real global cap.

## 3. Secrets

Generate each with `openssl rand -hex 32`. Never reuse the values from
[`.env.example`](../.env.example) or `docker-compose.yml` — they are in this
repository, which means they are public.

- [ ] `JWT_SECRET_HEX` — signs access and refresh tokens (≥ 32 bytes).
- [ ] `ENCRYPTION_KEY_HEX` — AES-256-GCM key for PII at rest, **exactly 32 bytes**.
      Rotating it re-keys existing PII: plan a migration, do not rotate casually.
- [ ] `BLIND_INDEX_PEPPER_HEX` — pepper for blind-index lookups (≥ 16 bytes).
      Rotating it invalidates every existing lookup hash, which is a data
      migration, not a config change.
- [ ] `DELIVERY_JWT_SECRET_HEX` — see §5. Required only if you run the delivery edge.
- [ ] `GATEWAY_SECRET` — see §4.
- [ ] `METRICS_BEARER_TOKEN` — requires a `Bearer` token on `GET /metrics`
      (`METRICS_ENABLED=true` by default). Or set `METRICS_ENABLED=false` to
      remove the endpoint. Unauthenticated metrics leak tenant and volume shape.
- [ ] `BOOTSTRAP_ADMIN_USER_ID` — one-time admin bootstrap. Set it once, then
      **unset it**. It is a standing grant while it is set.
- [ ] Supply all of these through your platform's secret store, **not** a committed
      `.env`. `docker-compose.yml` demands the critical ones via `${VAR:?}` and
      fails fast when one is missing: there is no well-known fallback baked in.
- [ ] Use a `DATABASE_URL` with `sslmode=require`, pointing at a managed or private
      Postgres — not the compose value's `sslmode=disable`.

## 4. Network topology

The domain API on `:8080` is **not** meant to face the internet. Each edge in
front of it authenticates its own callers and forwards a verified identity.

```text
                        Internet
              ┌────────────┼────────────┐
              ▼            ▼            ▼
     ┌────────────┐ ┌────────────┐ ┌────────────┐
     │ BFF :4000  │ │ Delivery   │ │ CMS MCP    │   TLS terminates here.
     │  GraphQL   │ │   :4100    │ │   :4200    │   Each authenticates its
     └─────┬──────┘ └─────┬──────┘ └─────┬──────┘   own callers.
           └──────────────┼──────────────┘
                          │  private network + X-Gateway-Secret
                          ▼
                 ┌──────────────────┐   APP_ENV=production,
                 │   Domain API     │   AUTHZ_MODE=rbac|opa,
                 │      :8080       │   AUTH_DEV_HEADERS=false.
                 └────────┬─────────┘   Never bound to a public interface.
                          │  private network only
                          ▼
                 ┌──────────────────┐
                 │    Postgres      │   Private subnet, sslmode=require.
                 └──────────────────┘
```

- [ ] **Do not publish `:8080`.** Restrict it to the edges on a private network.
- [ ] **Set `GATEWAY_SECRET`** — the same value on the domain API and on every edge
      (`apps/bff`, `apps/delivery`, `apps/cmsmcp`). The guard in
      `internal/pkg/authn/gateway.go` then rejects anything reaching `:8080`
      without a matching `X-Gateway-Secret`, with a constant-time comparison.
      Health probes are exempt, because they cannot carry it.
      **An empty `GATEWAY_SECRET` disables the guard entirely** — that is intended
      only for deployments where mTLS or a hard network boundary does the same job.
      The server logs a `WARN` while it is empty.
      Your gateway must **strip any client-supplied copy** of the header before
      setting its own; otherwise a caller can present the secret themselves.
- [ ] Put **Postgres on a private subnet**. The `POSTGRES_PORT` mapping in compose
      is for local development only.
- [ ] Leave `TRUST_PROXY_HEADERS` unset unless a proxy you control strips
      client-supplied `X-Forwarded-For`. With it on, a client can choose the key
      its own rate limiting and audit entries are recorded under.
- [ ] Set `MCP_BASE_URL` to your real outbox consumer over TLS. Left empty, outbox
      deliveries go to a no-op client — events look delivered and are not.
- [ ] Set `BFF_CORS_ORIGINS` to your own origins, and `BFF_PLAYGROUND=false`.
      The shipped values are development ports.

## 5. The public delivery edge (`apps/delivery`)

Skip this section if you do not serve public content — the feature is off when
`DELIVERY_JWT_SECRET_HEX` is unset.

- [ ] **`DELIVERY_JWT_SECRET_HEX` must be its own key**, ≥ 32 bytes, different from
      `JWT_SECRET_HEX`. The edge is the internet-facing process, so whatever key it
      holds should only be able to express *delivery* claims. It refuses to start
      without one, and production refuses to start if the two keys match.
- [ ] `DELIVERY_RATE_LIMIT` must be `> 0` — the edge refuses to start otherwise. A
      public endpoint keyed by a caller-supplied tenant is a ready-made way to burn
      someone else's quota.
- [ ] `DELIVERY_CACHE_MAX_AGE_SECONDS` defaults to `0`, meaning `public, no-cache`:
      still cacheable, but revalidated against a strong ETag on every use, so an
      unpublish takes effect on the next request. Any positive value is you
      accepting that much staleness in caches you cannot purge — pair it with a
      consumer of the `content.*` webhooks.
- [ ] `DELIVERY_TOKEN_TTL_SECONDS` (default 120) bounds how long a minted
      credential is useful if it leaks into a log or a proxy. Tokens are minted per
      request, so short is cheap.

The domain API still authenticates every request and still enforces
published-only itself. The edge's restraint is a convenience, not the control.

## 6. Agents (`apps/cmsmcp`)

- [ ] **`CMS_AGENT_TOKEN` must be an *agent* credential**, minted at
      `POST /api/v1/auth/agent-tokens`. Handing this process an ordinary human
      access token makes every agent-specific control inert — scope refusals, the
      allowed-types whitelist and agent provenance all stop applying — and the
      server still looks like it works. Nothing in the code can detect this, which
      is why it is written here.
- [ ] Agent tokens are revocable: `DELETE /api/v1/auth/agent-tokens/{id}` stops one
      on its next request. That revocability is what makes the long
      `AGENT_TOKEN_TTL_DAYS` (default 30) issuable at all — keep a way to revoke.
- [ ] **Set `AGENT_RATE_LIMIT`** (with `AGENT_RATE_WINDOW_SEC`). Unset means one
      agent credential may call the domain API as fast as it likes; the server logs
      a `WARN`. The limiter is in-process, so the effective ceiling is again
      *replicas × limit*.
- [ ] In HTTP mode (`CMS_MCP_HTTP_ADDR` set), `CMS_AGENT_TOKEN` **must not** be set.
      Each request carries its own credential there, and a process-wide token would
      silently answer for callers that sent none — the server refuses the
      combination rather than pick a winner.

## 7. Media storage

- [ ] Point `MEDIA_S3_ENDPOINT` / `MEDIA_S3_BUCKET` / `MEDIA_S3_REGION` at real
      object storage, with `MEDIA_S3_USE_SSL=true`.
- [ ] **The bucket must be private.** Neither upload nor download passes through
      the API: clients receive short-lived presigned URLs and talk to storage
      directly. A public bucket makes every constraint in the signature irrelevant.
- [ ] `MEDIA_S3_ACCESS_KEY` / `MEDIA_S3_SECRET_KEY` belong in the secret store with
      everything in §3, scoped to that one bucket.

## 8. Verify at boot

A clean production boot logs **none** of these. Treat any of them in production
logs as a misconfiguration to fix, not as noise:

```
WARN: AUTHZ_MODE=...            all authorization checks pass
WARN: AUTH_DEV_HEADERS=true     X-User-* trusted without a JWT
WARN: TRUST_PROXY_HEADERS=true  X-Forwarded-For is client-controlled
WARN: MCP_BASE_URL is empty     outbox deliveries go nowhere
WARN: AGENT_RATE_LIMIT is unset one agent credential is uncapped
WARN: GATEWAY_SECRET is empty   :8080 accepts direct requests
```

The first two cannot appear at all under `APP_ENV=production` — the process would
have refused to start (§0). If you see them, that process is not running as
production, whatever the deployment is called.

- [ ] Confirm `GET /health` is reachable from your orchestrator and **not** from
      the internet. It is the one path the gateway guard exempts, so it is also the
      one path that answers without `X-Gateway-Secret`.
- [ ] Confirm `GET /metrics` demands its bearer token, from outside the cluster.
- [ ] Watch `outbox_lag_seconds` and `outbox_pending`. A rising lag means events
      are accepted and not delivered — the failure this architecture is built to
      make visible rather than silent.

---

*Pre-1.0. Every claim here is checkable against the code in this repository;
where the two disagree, the code is right and this file is a bug.*
