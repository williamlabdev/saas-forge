# Getting Started — SaaSForge

**SaaSForge** is a Go **modular-monolith SaaS backend**: a domain REST API plus a GraphQL BFF, backed by Postgres, a transactional outbox, and pluggable authorization (allow / RBAC / OPA). New business domains are scaffolded — and auto-wired — with one command (`make new-domain`).

> Naming: **SaaSForge** is the display/brand name; the Go module path is
> `github.com/williamlabdev/saas-forge`. The binaries keep their own names
> (`cmd/server`, `apps/bff`, `apps/delivery`, `apps/cmsmcp`).

> This repository ships the **backend + BFF** only. The web UI lives in a separate project; the BFF exposes GraphQL for any frontend to consume.

## What's inside

```
cmd/server/            Domain REST API (entrypoint, wire DI)
apps/bff/              GraphQL BFF (gqlgen) — aggregates the domain API
apps/delivery/         Public delivery edge — published content + preview links
apps/cmsmcp/           MCP server exposing the CMS to agents
internal/<domain>/     Each domain: domain / service / repository / handler / migrations
  auth  iam  user  notification  platformops  ...
internal/pkg/          Shared: outbox, authz, authn, crypto, config, response, ...
  authz/policies/      OPA rego, embedded into the binary (AUTHZ_MODE=opa)
scripts/               new-domain generator, demo seeding
```

Architecture details: [`docs/ARCHITECTURE.md`](ARCHITECTURE.md).

## Prerequisites

- **Go 1.26+**
- **Docker** + Docker Compose
- **make**

## Run it (Docker)

```bash
cp .env.example .env
docker compose up --build -d      # postgres + domain API + BFF + mcp-mock
./scripts/seed-demo.sh            # creates a demo login account + sample data
```

Services:

| Service | URL |
|---|---|
| Domain REST API | http://localhost:8080 (`/health`) |
| GraphQL BFF | http://localhost:4000/graphql |
| Postgres | localhost:5433 |

Try the API:

```bash
curl localhost:8080/health
# GraphQL: open http://localhost:4000/playground (dev) or POST to /graphql
```

## Run tests

```bash
make test              # unit tests
make test-integration  # repository tests (needs Docker: testcontainers)
make build             # build domain API + BFF binaries
```

## Add a new domain (the headline feature)

Scaffold a full domain — `domain / service / repository / handler / migrations` — and auto-wire it into the app:

```bash
make new-domain NAME=ticket
make fmt && make wire          # format + regenerate DI
go build ./...
go test ./internal/ticket/...
```

Then apply the generated migration and you have working CRUD endpoints with a transactional outbox event on create/update/delete. See [`docs/new-domain.md`](new-domain.md) for the full flow and wiring details.

## Configuration & security

Config is env-driven (`.env` / `.env.example`). A few defaults are **development-only**:

- `AUTHZ_MODE=allow` and `AUTH_DEV_HEADERS=true` make the API permissive for local dev. **Set `AUTHZ_MODE=rbac` (or `opa`) and `AUTH_DEV_HEADERS=false` before any real deployment.**
- The `*_HEX` secrets in `docker-compose.yml` are throwaway local values. **Generate real secrets for any non-local use** (the app validates length but not strength).

Never expose the domain API directly to the internet — it is designed to sit behind the BFF / a trusted gateway that authenticates requests. See [`SECURITY.md`](../SECURITY.md).

## Key concepts

- **Modular monolith / hexagonal**: domain packages import no infrastructure; services own authorization and validation; repositories own SQL.
- **Transactional outbox**: writes and their integration events commit in one transaction, then a worker delivers them — no dual-write inconsistency.
- **Pluggable authz**: a single `Authorizer` interface with `allow` / `rbac` / `opa` implementations.

## License

MIT — see [`LICENSE`](../LICENSE).
