# Manual test guide (local)

> **About the frontend — read this first if you came from the public repo**
>
> Every `cd ../saas-platform-console` step below refers to the **admin console**, a
> React + Vite SPA that has lived in its own repository since
> 2026-08-30. **That repository is not open
> source and there is no plan to open it**, so those steps will stop at "no such
> directory".
>
> **You do not need it.** The console is just *one* client of this backend. Nothing
> between them is private — it speaks the two interfaces this repository documents
> in full:
>
> - **GraphQL BFF** (`apps/bff`, :4000) — schema under `apps/bff/graph/`, playground
>   at http://localhost:4000/playground. Open to any frontend.
> - **Domain REST API** (`cmd/server`, :8080) — endpoints and curl examples in
>   Level B below, and in [`GETTING_STARTED.md`](GETTING_STARTED.md).
>
> So: **skip every `pnpm` step — the backend runs and verifies on its own.** Point
> your own frontend at it when you want a UI. `BFF_CORS_ORIGINS` in `.env.example`
> allows `:5173` / `:4173` / `:4174`, which are there for the console and its
> Playwright preview; swap in your own origin.

## Prerequisites

1. `cp .env.example .env`
2. Sibling repos: `../saas-platform-console` (the console — ADR-016 moved it out of this repo)
   and `../design-engine-kit` (UI kit + themes, which the console consumes)
3. `docker compose up --build` (in **this** repo)
4. `cd ../saas-platform-console && pnpm install && pnpm dev` (Vite on **5173**)

| URL | Service |
|-----|---------|
| http://localhost:5173 | Platform UI (Vite) — `saas-platform-console` |
| http://localhost:4000/playground | BFF GraphQL |
| http://localhost:8080/health | Domain API |

## Sign-in (no default account)

There is **no pre-seeded user**. After `docker compose down -v`, create one:

**Browser:** http://localhost:5173/register → then http://localhost:5173/login

**API:**

- `runbook@example.com` / `SecurePass123!` — register it first; the two curls are in [Level B](#level-b--live-api)
- `alice@example.com` / `password12` — same, or run `./scripts/seed-demo.sh`, which creates accounts and prints their credentials

**UI-only (no BFF):** On the login page, click **Demo: skip sign-in (UI only, no BFF)** (dev mode only).

## Level A — UI shell

1. Demo skip sign-in or register + sign in
2. Platform: `/admin/data`, `/admin/billing`, `/admin/reports`, `/admin/notifications`, `/admin/permissions`
3. Tenant mock: `/admin/app_demo_001/data`, `.../settings`, `.../permissions`
4. `/theme-lab`, `/register`

## Level B — Live API

```bash
curl -s http://localhost:8080/health

# Register. There is no default account — a fresh volume has no users.
curl -s -X POST http://localhost:8080/api/v1/users \
  -H 'Content-Type: application/json' \
  -d '{"username":"runbook","email":"runbook@example.com","password":"SecurePass123!"}'

# Log in; the access token comes back as data.access_token.
curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"runbook@example.com","password":"SecurePass123!"}'

export TOKEN="<access_token>"
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/platform/apps?limit=20"
```

GraphQL (Playground → HTTP Headers):

```json
{ "Authorization": "Bearer <access_token>" }
```

```graphql
query { platform { apps(limit: 5) { items { id name status } total } } }
```

## Level C — Playwright

Playwright lives in `../saas-platform-console` (ADR-016), not here.

```bash
# Terminal 1 (this repo):            docker compose up
# Terminal 2 (../saas-platform-console): pnpm dev
# Terminal 3 (../saas-platform-console):
pnpm test:e2e:dev
E2E_LIVE=1 E2E_EMAIL=you@example.com E2E_PASSWORD='your-password' pnpm test:e2e:live
```

## Troubleshooting

| Issue | Fix |
|-------|-----|
| Browser cannot connect | Use **5173**, not 8080; run `pnpm dev` in `../saas-platform-console` |
| Login fails | BFF + app running; register first after volume reset |
| `relation ... does not exist` | `docker compose down -v && docker compose up --build` |
| CORS | `BFF_CORS_ORIGINS` includes `http://localhost:5173` (and `:4173` / `:4174` for Playwright preview) |
