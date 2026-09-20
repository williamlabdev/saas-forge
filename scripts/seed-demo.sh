#!/usr/bin/env bash
#
# seed-demo.sh — prepare a clean stack for a live demo.
#
# A fresh DB already has platform apps / billing / staff / alerts seeded by
# migrations 07–08. The ONE thing missing is a login account (users need
# app-side password hashing + PII encryption, so they can't be seeded via SQL).
# This script creates that account and adds a couple of fresh platform apps so
# the "create" flow has visible, recent rows.
#
# Usage:
#   docker compose up --build -d      # start the stack first
#   ./scripts/seed-demo.sh            # then seed
#
# Re-running is safe: an already-registered account is treated as success.

set -euo pipefail

API="${API:-http://localhost:8080}"
EMAIL="${DEMO_EMAIL:-demo@demo.io}"
USERNAME="${DEMO_USERNAME:-demoadmin}"
PASSWORD="${DEMO_PASSWORD:-DemoPass123}"

say()  { printf '\033[1;36m%s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*"; }

# --- 1. wait for the domain API to be healthy ---------------------------------
say "Waiting for $API/health ..."
for i in $(seq 1 30); do
  if curl -sf "$API/health" >/dev/null 2>&1; then
    ok "API is up"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "API never became healthy. Is 'docker compose up' running?" >&2
    exit 1
  fi
  sleep 2
done

# --- 2. register the demo account (idempotent) --------------------------------
say "Registering demo account ($EMAIL) ..."
reg_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/api/v1/users" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USERNAME\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
case "$reg_status" in
  20*) ok "account created" ;;
  409) warn "account already exists — reusing it" ;;
  *)   warn "register returned HTTP $reg_status (continuing; login will confirm)" ;;
esac

# --- 3. log in and grab the token ---------------------------------------------
say "Logging in ..."
login_body=$(curl -s -X POST "$API/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
TOKEN=$(printf '%s' "$login_body" | grep -oE '"access_token":"[^"]+"' | head -1 | cut -d'"' -f4)
if [ -z "$TOKEN" ]; then
  echo "Login failed. Response was:" >&2
  echo "$login_body" >&2
  exit 1
fi
ok "logged in, got access token"

# --- 4. add a couple of fresh platform apps (allow-mode dev stack) ------------
say "Creating sample platform apps ..."
create_app() {
  local name="$1" tenant="$2" owner="$3"
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/api/v1/platform/apps" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $TOKEN" \
    -d "{\"name\":\"$name\",\"tenant_id\":\"$tenant\",\"owner\":\"$owner\"}")
  case "$code" in
    20*) ok "app: $name" ;;
    *)   warn "app '$name' returned HTTP $code" ;;
  esac
}
create_app "Acme Analytics"  "acme"   "demo@demo.io"
create_app "Globex Billing"  "globex" "demo@demo.io"

# --- 5. summary ---------------------------------------------------------------
echo
say "Demo stack is ready."
cat <<EOF

  Email:        $EMAIL
  Password:     $PASSWORD

  Log in and drive it over HTTP (BFF GraphQL -> Go domain -> Postgres):

    curl -s -X POST $API/api/v1/auth/login \\
      -H 'Content-Type: application/json' \\
      -d '{"email":"$EMAIL","password":"$PASSWORD"}'

    GraphQL playground   http://localhost:4000/playground
    Domain REST API      $API/api/v1/...   (Authorization: Bearer <access_token>)
    Delivery edge        http://localhost:4100   (published content, public read)

  These back real pages in an admin console: platform apps, billing, reports,
  staff/roles, notifications. The console is a separate, closed-source project —
  it holds no private protocol with the backend, so any frontend can drive the
  same two interfaces.

  See docs/DEMO.md for the full runbook and the real-vs-mock map.
EOF
