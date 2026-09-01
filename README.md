# SaaSForge

> A batteries-included Go SaaS backend: domain REST API + GraphQL BFF, transactional outbox, and pluggable authz — scaffold a new domain with one command.

SaaSForge is a Go **modular-monolith** backend you can build a SaaS product on. It ships the parts every platform re-implements — authentication, IAM, users, notifications, a transactional outbox, and pluggable authorization (allow / RBAC / OPA) — behind a clean ports-and-adapters layout, with a GraphQL BFF in front. Adding a new business domain is a single command that scaffolds **and** wires it end to end.

```bash
make new-domain NAME=ticket   # domain + service + repository + handler + migration, auto-wired
```

## Why

- **Modular monolith, done right** — domain packages import no infrastructure; services own authorization and validation; repositories own SQL. One deployable, clean seams.
- **Transactional outbox** — writes and their integration events commit in one transaction, then a worker delivers them. No dual-write drift.
- **Pluggable authorization** — a single `Authorizer` interface with `allow` / `rbac` / `opa` implementations.
- **AI-native scaffolding** — `make new-domain` generates a full vertical slice and auto-wires it, so effort goes into business logic, not boilerplate.
- **Agents are first-class clients, not an afterthought** — the CMS is exposed over MCP (`apps/cmsmcp`), agents authenticate with their own scoped, revocable credentials, and any schema change that would break existing content becomes a *proposal* a human has to answer.

## Quick start

```bash
cp .env.example .env
docker compose up --build -d      # postgres + domain API + GraphQL BFF + mcp
./scripts/seed-demo.sh
```

Full walkthrough: **[docs/GETTING_STARTED.md](docs/GETTING_STARTED.md)**.

| Deployable | Path | Port |
|---|---|---|
| Domain REST API | `cmd/server` | 8080 |
| GraphQL BFF | `apps/bff` | 4000 |
| Delivery edge (published content, public read) | `apps/delivery` | 4100 |
| CMS MCP server (for agents) | `apps/cmsmcp` | 4200 |

### Where is the frontend?

There isn't one in this repository, and that is deliberate rather than missing.
The admin console used in development is a separate, closed-source project. It
holds no private protocol with the backend — it talks to the same GraphQL BFF
documented here, which is open to any frontend you care to write. Everything in
this repository runs and is testable without it.

`.env.example` allows `:5173` / `:4173` / `:4174` in `BFF_CORS_ORIGINS`; replace
those with your own origins.

## Documentation

- [Getting Started](docs/GETTING_STARTED.md) — run it, test it, add a domain
- [Architecture](docs/ARCHITECTURE.md) — layering, outbox, authz
- [Adding a domain](docs/new-domain.md) — the generator in depth
- [Security](SECURITY.md) — the unsafe development defaults, and vulnerability reporting
- [Deployment hardening](docs/DEPLOYMENT_HARDENING.md) — the checklist for turning each of them off
- [Contributing](CONTRIBUTING.md) — build/test commands, and the current stance on pull requests

## Security note

Some defaults are **development-only** (permissive authz, dev auth headers, throwaway secrets in `docker-compose.yml`). **Harden them before any real deployment** and never expose the domain API directly — it is designed to sit behind the BFF / a trusted gateway. See [SECURITY.md](SECURITY.md).

## Project status

Pre-1.0 and maintained by one person. It is published as a working reference —
something you can run, read, fork, and take ideas from — rather than as a
community project seeking contributors. Issues are welcome; pull requests are
not being merged at the moment, for reasons set out honestly in
[CONTRIBUTING.md](CONTRIBUTING.md).

For what it is worth as evidence that it actually works: 149 test files,
~1,000 test functions, and 14 end-to-end suites that drive real HTTP against a
real Postgres.

## License

[MIT](LICENSE) © 2026 William Chiu

---

<!-- ===== GitHub repo settings (paste into the repo's About panel) ===== -->

**Description (About):**
`A batteries-included Go SaaS backend: domain REST API + GraphQL BFF, transactional outbox, and pluggable authz — scaffold a new domain with one command.`

**Topics:**
`go` · `golang` · `saas` · `modular-monolith` · `hexagonal-architecture` · `scaffolding` · `boilerplate` · `graphql` · `gqlgen` · `postgres` · `transactional-outbox` · `rbac` · `opa`
