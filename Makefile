.PHONY: test test-coverage test-coverage-html test-integration test-e2e sqlc wire mocks build migrate-status migrate-up up down opa-test mcp-mock govulncheck lint new-domain new-domain-smoke fmt

test:
	go test ./...

# Coverage over hand-written code. Delegates to scripts/coverage.sh, which
# excludes test/e2e (needs Docker) and generated files (gqlgen/mockery/sqlc/wire,
# identified by their `// Code generated ... DO NOT EDIT` header) from the
# denominator. The generated gqlgen resolver alone is ~5.4k statements and would
# otherwise swamp the metric.
test-coverage:
	@bash ./scripts/coverage.sh

test-coverage-html: test-coverage
	go tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html in a browser"

test-integration:
	go test -timeout=5m ./internal/user/repository/... ./internal/tenant/repository/...

# Every end-to-end suite, not just the platform one. The apps keep their own
# because Go's internal/ rule makes an app's handler and client importable only
# from inside that app — apps/delivery/test/e2e and apps/cmsmcp/test/e2e exist
# for that reason, not by preference.
#
# The wildcard is doing real work: these suites were reachable only through the
# blanket `go test ./...`, so a Testcontainers suite was running inside a CI job
# labelled "Unit tests" and this target — the one named for e2e — did not touch
# them. A hand-written list of app paths would have been a claim that expires
# the next time an app is added, which is how that gap opened in the first place.
test-e2e:
	go test -timeout=5m ./test/e2e/... ./apps/.../test/e2e/...

sqlc:
	docker run --rm -v "$(PWD):/src" -w /src sqlc/sqlc:1.28.0 generate

# Pinned to the module's own wire version so local output matches the CI
# codegen-fresh check (see .github/workflows/ci.yml).
wire:
	cd cmd/server && go run github.com/google/wire/cmd/wire@$$(go list -m -f '{{.Version}}' github.com/google/wire)

mocks:
	go run github.com/vektra/mockery/v2@v2.53.5

build:
	go build -o bin/server ./cmd/server
	go build -o bin/bff ./apps/bff/cmd/bff

# Format Go sources (gofmt always; goimports too if installed — fixes import order).
fmt:
	gofmt -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w . || echo "fmt: goimports not installed; ran gofmt only"

bff-gqlgen:
	cd apps/bff && go run github.com/99designs/gqlgen@$$(go list -m -f '{{.Version}}' github.com/99designs/gqlgen) generate

# Migrations (ADR-012). `up` applies what the ledger does not have; `status`
# compares the ledger against the files. Adding a migration needs no other step
# — the applier embeds them by glob, so there is no list to regenerate.
#
# `adopt` is missing from here on purpose: it asserts a fact about a specific
# database that only a person knows, so it is typed by hand with the number:
#   DATABASE_URL=... go run ./cmd/migrate adopt --to=NNN
migrate-status:
	go run ./cmd/migrate status

migrate-up:
	go run ./cmd/migrate up

up:
	docker compose up --build

down:
	docker compose down -v

opa-test:
	docker run --rm -v "$(PWD)/internal/pkg/authz/policies:/policies" openpolicyagent/opa:1.4.2 test -v /policies

mcp-mock:
	go run ./cmd/mcp-mock

# Pinned for the same reason GOLANGCI_VERSION below is: `@latest` means the tool
# changes under you without a commit, so a green main can turn red with nothing
# in the diff. The findings it reports still float — the vulnerability database
# is queried live — but the analyser itself does not.
GOVULNCHECK_VERSION := v1.6.0

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# The SAME version the lint job pins in .github/workflows/ci.yml — keep the two
# in step. `go run` rather than whatever golangci-lint is on PATH: a stale local
# install silently checks fewer rules than CI does, which is how main went red
# on 2026-08-04 with seven findings that could not be reproduced locally.
GOLANGCI_VERSION := v2.12.2

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

# Scaffold a new domain under internal/<NAME>/ from scripts/domain-template/.
#   make new-domain NAME=invoice
#   make new-domain NAME=person PLURAL=people   # irregular plural
#   make new-domain NAME=invoice WIRE=0         # scaffold only, skip auto-wiring
new-domain:
	@./scripts/new-domain.sh $(NAME) $(PLURAL) $(if $(filter 0,$(WIRE)),--no-wire,)

# End-to-end smoke test: scaffold a throwaway domain, fmt, wire, build, test,
# then restore the repo to its prior state (even on failure). Needs Go; no DB.
#   make new-domain-smoke            # uses default temp name
#   make new-domain-smoke NAME=foo   # custom temp name (lowercase letters)
new-domain-smoke:
	@./scripts/new-domain-smoke.sh $(NAME)
