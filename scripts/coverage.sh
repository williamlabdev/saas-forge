#!/usr/bin/env bash
# Runs the unit-test suite with coverage and reports a total computed over
# HAND-WRITTEN code only. Generated files (gqlgen generated.go/models_gen.go,
# mockery mocks, sqlc, wire_gen) carry a `// Code generated ... DO NOT EDIT`
# header and are excluded from the denominator — they are not meaningfully
# unit-testable and would otherwise dominate the statement count (the gqlgen
# generated.go alone is ~5.4k statements). test/e2e is excluded here and run
# separately (needs Docker).
set -euo pipefail
cd "$(dirname "$0")/.."

MOD=$(go list -m)
COVER_PKGS=$(go list ./... | grep -vE '/test/e2e$')

go test $COVER_PKGS -coverprofile=coverage.raw.out -covermode=atomic "$@"

# Build the list of module-qualified generated-file prefixes to drop.
GEN_PREFIXES=$(grep -rlE '^// Code generated .* DO NOT EDIT' --include='*.go' . \
	| sed "s|^\./||; s|^|$MOD/|; s|$|:|")

if [ -n "$GEN_PREFIXES" ]; then
	{
		head -1 coverage.raw.out
		tail -n +2 coverage.raw.out | grep -vFf <(printf '%s\n' "$GEN_PREFIXES")
	} >coverage.out
else
	cp coverage.raw.out coverage.out
fi
rm -f coverage.raw.out

go tool cover -func=coverage.out | tee coverage-summary.txt
echo "total (excl. generated & test/e2e):" \
	"$(go tool cover -func=coverage.out | awk '/^total:/ {print $3}')"
