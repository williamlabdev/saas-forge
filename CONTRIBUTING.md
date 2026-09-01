# Contributing

## Current status: not accepting pull requests

I want to be straightforward about this instead of leaving you to find out after
you've done the work.

This repository is published as a **reference and a working artifact** — it is a
real backend you can run, read, and learn from — but it is not currently run as
a community project. There is one maintainer, and the continuous integration
that would be needed to review outside changes responsibly is not in place.
Merging patches I can't properly verify would be worse for you than declining
them.

So:

- **Issues are welcome.** Bug reports, questions about how something works, and
  "this document is wrong" corrections are genuinely useful and I read them.
- **Security reports go through the private channel**, not issues — see
  [`SECURITY.md`](SECURITY.md).
- **Pull requests will not be merged right now.** If you've already opened one,
  it isn't wasted: I'll read it and it may well drive a change, but it will land
  as my own commit rather than a merge of yours.
- **Forking is encouraged.** MIT licensed. If you want to take this somewhere,
  take it.

If this changes, this file changes first.

## Building and testing

Prerequisites: **Go 1.26+**, **Docker** + Docker Compose, **make**.

```bash
cp .env.example .env
docker compose up --build -d      # postgres + domain API + BFF + mcp-mock
./scripts/seed-demo.sh            # demo login account + sample data
```

| Command | What it runs |
|---|---|
| `make test` | `go test ./...` |
| `make test-integration` | Repository tests against a real Postgres (testcontainers — needs Docker) |
| `make test-e2e` | End-to-end suites in `test/e2e/` (needs Docker) |
| `make lint` | golangci-lint |
| `make opa-test` | OPA policy tests under `internal/pkg/authz/policies/` |
| `make govulncheck` | Known-vulnerability scan |
| `make build` | Build the domain API and BFF binaries |
| `make fmt` | gofmt + goimports |
| `make wire` | Regenerate the DI graph |
| `make sqlc` / `make mocks` | Regenerate SQL bindings / mocks (both need Docker) |
| `make up` / `make down` | `docker compose up --build` / `down -v` |

The whole gate in one line, which is what CI runs:

```bash
go test ./... && make test-coverage && make opa-test && make test-e2e && make govulncheck
```

Full walkthrough: [`docs/GETTING_STARTED.md`](docs/GETTING_STARTED.md).

## Adding a domain

The scaffolder is the headline feature and the thing most worth reading if you
want to understand how the codebase is organized:

```bash
make new-domain NAME=ticket
make fmt && make wire
go build ./...
go test ./internal/ticket/...
```

It generates `domain / service / repository / handler / migrations` for the new
domain and wires it into the application. Details in
[`docs/new-domain.md`](docs/new-domain.md).

## Conventions, if you're reading the code

- **Modular monolith, hexagonal.** Domain packages import no infrastructure.
  Services own authorization and validation. Repositories own SQL. If a change
  makes a domain package import a driver, that's the signal something is in the
  wrong layer.
- **Transactional outbox.** A write and its integration event commit in one
  transaction; a worker delivers afterwards. Don't add a second write path that
  publishes directly.
- **Authorization is an interface**, with `allow` / `rbac` / `opa` behind it.
  New checks go through `Authorizer`, not through ad-hoc conditionals.
- **Migrations are append-only.** Once a migration has been applied anywhere, it
  is immutable; correct it with a new one.
