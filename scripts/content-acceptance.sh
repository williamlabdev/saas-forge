#!/usr/bin/env bash
#
# content-acceptance.sh — runs the Phase 2.5 content-domain REST vertical slice
# (handoff §5.1) against a running app server, asserting the 7 acceptance
# signals. The two that prove "direction viable" are step 2 (add a field with no
# redeploy) and step 5 (one endpoint serves any content type via the GIN index).
#
# Prereqs: a fresh DB with the content migration applied + the app on :8080.
#   RESET=1 ./scripts/content-acceptance.sh   # tears down + rebuilds the stack
#   ./scripts/content-acceptance.sh           # assumes stack already up
#
# Dev auth uses headers (AUTHZ_MODE=allow, AUTH_DEV_HEADERS=true): the tenant is
# taken from X-Tenant-Id — the API never accepts a tenant parameter.
set -uo pipefail

BASE="${BASE:-http://localhost:8080}"
UID_A="11111111-1111-1111-1111-111111111111"
H_A=(-H "X-User-Id: ${UID_A}" -H "X-Tenant-Id: tenant-a" -H "X-User-Roles: admin")
H_B=(-H "X-User-Id: ${UID_A}" -H "X-Tenant-Id: tenant-b" -H "X-User-Roles: admin")

pass=0 fail=0
ok()   { echo "  ✅ $1"; pass=$((pass+1)); }
bad()  { echo "  ❌ $1"; fail=$((fail+1)); }
have() { echo "$1" | grep -qF "$2"; }  # -F: literal match (JSON has [ ] " , regex metachars)

if [ "${RESET:-0}" = "1" ]; then
	echo "==> Resetting stack (down -v && up -d --build) ..."
	docker compose down -v
	docker compose up -d --build
fi

echo "==> Waiting for ${BASE}/health ..."
for _ in $(seq 1 60); do
	if curl -fsS "${BASE}/health" >/dev/null 2>&1; then break; fi
	sleep 2
done
curl -fsS "${BASE}/health" >/dev/null 2>&1 || { echo "app not reachable at ${BASE}"; exit 1; }

echo "== Step 1: create content type 'order' =="
r=$(curl -s "${H_A[@]}" -X POST "${BASE}/api/v1/content/types" -d '{
  "name":"order","label":"Order",
  "fields":[
    {"key":"title","type":"string","required":true},
    {"key":"amount","type":"number"},
    {"key":"state","type":"enum","enum_values":["new","paid"]}
  ]}')
have "$r" '"name":"order"' && ok "type created" || bad "type create: $r"

echo "== Step 2: add field 'note' (NO redeploy, NO codegen) ★ =="
r=$(curl -s "${H_A[@]}" -X POST "${BASE}/api/v1/content/types/order/fields" -d '{"key":"note","type":"text"}')
have "$r" '"key":"note"' && ok "field added at runtime ★" || bad "add field: $r"

echo "== Step 3: create a valid entry =="
r=$(curl -s "${H_A[@]}" -X POST "${BASE}/api/v1/content/entries?type=order" \
  -d '{"title":"first","amount":42,"state":"paid","note":"hello"}')
EID=$(echo "$r" | sed -n 's/.*"id":"\([0-9a-f-]\{36\}\)".*/\1/p')
have "$r" '"state":"paid"' && [ -n "$EID" ] && ok "entry created id=$EID" || bad "create entry: $r"
# a second, non-matching entry so the filter has something to exclude
curl -s "${H_A[@]}" -X POST "${BASE}/api/v1/content/entries?type=order" \
  -d '{"title":"second","amount":7,"state":"new"}' >/dev/null

echo "== Step 4: bad values are rejected with field-level codes =="
r=$(curl -s "${H_A[@]}" -X POST "${BASE}/api/v1/content/entries?type=order" -d '{"amount":"NaN","state":"x"}')
have "$r" 'CONTENT_FIELD_' && ok "rejected: $(echo "$r" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p')" || bad "expected CONTENT_FIELD_* : $r"

echo "== Step 5: generic list, filter=state:eq:paid sort=amount:desc (GIN @>) ★ =="
r=$(curl -s "${H_A[@]}" "${BASE}/api/v1/content/entries?type=order&filter=state:eq:paid&sort=amount:desc")
if have "$r" '"state":"paid"' && have "$r" '"title":"first"' && ! have "$r" '"title":"second"'; then
	ok "single endpoint served the type + filter matched ★"
else
	bad "list/filter: $r"
fi

echo "== Step 6: cross-tenant isolation (tenant B) =="
r=$(curl -s "${H_B[@]}" "${BASE}/api/v1/content/entries?type=order")
# tenant B has no 'order' type, OR sees zero entries — either way, no A data.
if have "$r" '"total":0' || have "$r" 'NOT_FOUND' || have "$r" 'not found'; then
	ok "tenant B sees none of A's data"
else
	bad "tenant B leak: $r"
fi
code=$(curl -s -o /dev/null -w "%{http_code}" "${H_B[@]}" "${BASE}/api/v1/content/entries/${EID}?type=order")
[ "$code" = "404" ] && ok "GET A's entry as B -> 404" || bad "expected 404 got $code"

echo "== Step 7: GET type shape matches EntityField =="
r=$(curl -s "${H_A[@]}" "${BASE}/api/v1/content/types/order")
if have "$r" '"key":"title"' && have "$r" '"type":"string"' && have "$r" '"enum_values":["new","paid"]'; then
	ok "field shape aligns with admin-app.schema.json EntityField"
else
	bad "type shape: $r"
fi

echo
echo "==> ${pass} passed, ${fail} failed"
[ "$fail" = "0" ]
