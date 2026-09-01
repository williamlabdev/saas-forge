#!/usr/bin/env bash
# End-to-end smoke test for the `new-domain` generator + auto-wiring.
#
# Scaffolds a throwaway domain, formats, regenerates wire, builds the whole
# module, and runs the generated domain's unit tests — then restores the repo
# to its exact prior state (even on failure, via an EXIT trap).
#
# Requires the Go toolchain (it actually compiles). Run locally, not in CI's
# sandbox. No database needed: the generated service test uses an in-memory repo.
#
# Usage: ./scripts/new-domain-smoke.sh [name]
#   name  optional; lowercase letters only; default: zzsmoketmp
set -euo pipefail

cd "$(dirname "$0")/.."
NAME="${1:-zzsmoketmp}"

if ! printf '%s' "$NAME" | grep -qE '^[a-z]+$'; then
  echo "smoke: name must be lowercase letters only, got '$NAME'" >&2
  exit 2
fi
if [ -e "internal/$NAME" ]; then
  echo "smoke: internal/$NAME already exists; pick another name or remove it" >&2
  exit 2
fi

# Files the generator + `make wire` mutate. Snapshot them so we can restore.
WIRED_FILES=(
  cmd/server/providers.go
  cmd/server/wire.go
  cmd/server/router.go
  internal/platform/router.go
  internal/platform/app.go
  cmd/server/wire_gen.go
)

BACKUP="$(mktemp -d)"
RESULT="UNKNOWN"
cleanup() {
  echo
  echo "smoke: restoring repo to prior state..."
  for f in "${WIRED_FILES[@]}"; do
    # Preserve the relative path under BACKUP. Two of the wired files are both
    # named router.go (cmd/server/ and internal/platform/), so keying the
    # backup by basename alone would collide and restore the wrong content.
    if [ -f "$BACKUP/$f" ]; then
      cp "$BACKUP/$f" "$f"
    fi
  done
  rm -rf "internal/$NAME"
  rm -rf "$BACKUP"
  echo "smoke: cleanup done."
  echo "smoke: RESULT = $RESULT"
}
trap cleanup EXIT

for f in "${WIRED_FILES[@]}"; do
  if [ -f "$f" ]; then
    mkdir -p "$BACKUP/$(dirname "$f")"
    cp "$f" "$BACKUP/$f"
  fi
done

run() { echo; echo "==> $*"; "$@"; }

echo "smoke: scaffolding throwaway domain '$NAME' (auto-wire)..."
run ./scripts/new-domain.sh "$NAME"

echo "smoke: formatting generated + wired files..."
run gofmt -w "internal/$NAME" "${WIRED_FILES[@]}"
if command -v goimports >/dev/null 2>&1; then
  run goimports -w "internal/$NAME" "${WIRED_FILES[@]}"
fi

run make wire
run go build ./...
run go vet "./internal/$NAME/..."
run go test "./internal/$NAME/..."

RESULT="PASS"
echo
echo "smoke: PASS — generator output compiles, wires, and tests green."
