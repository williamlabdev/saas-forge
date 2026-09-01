#!/usr/bin/env bash
#
# new-domain.sh — scaffold a new domain under internal/<name>/ from the
# token-based template in scripts/_domain-template/.
#
# The template dir is underscore-prefixed so the Go toolchain ignores it
# (`go build ./...`, `go vet ./...`, and wire's package loader skip dirs whose
# names begin with "_" or "."). Its *.go files contain unreplaced __TOKEN__
# imports that would otherwise break a whole-module build.
#
# Usage:
#   ./scripts/new-domain.sh <name> [plural]
#
#   <name>    lowercase singular package name (letters only), e.g. invoice
#   [plural]  optional explicit plural for irregular nouns, e.g.
#               ./scripts/new-domain.sh person people
#             defaults to "<name>s".
#
set -euo pipefail

MODULE="github.com/williamlabdev/saas-forge"

# Resolve repo root from this script's location (scripts/ is at repo root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEMPLATE_DIR="${SCRIPT_DIR}/_domain-template"

die() {
	echo "error: $*" >&2
	exit 1
}

# --- args -------------------------------------------------------------------
# Parse flags (anywhere on the line) and collect positionals separately so the
# existing "<name> [plural]" usage keeps working.
WIRE=1
POSITIONALS=()
for arg in "$@"; do
	case "${arg}" in
		--no-wire) WIRE=0 ;;
		--*) die "unknown flag: ${arg}" ;;
		*) POSITIONALS+=("${arg}") ;;
	esac
done

NAME="${POSITIONALS[0]:-}"
PLURAL_ARG="${POSITIONALS[1]:-}"

[ -n "${NAME}" ] || die "missing <name>. usage: ./scripts/new-domain.sh <name> [plural]"

if ! printf '%s' "${NAME}" | grep -Eq '^[a-z]+$'; then
	die "name must be lowercase letters only (got: '${NAME}')"
fi

[ -d "${TEMPLATE_DIR}" ] || die "template dir not found: ${TEMPLATE_DIR}"

DEST="${REPO_ROOT}/internal/${NAME}"
# If the domain dir already exists we do NOT overwrite it. With auto-wiring on,
# we still continue to the (idempotent) wiring pass so a re-run can finish the
# wiring. With --no-wire there's nothing left to do, so we refuse outright.
SCAFFOLD=1
if [ -e "${DEST}" ]; then
	if [ "${WIRE}" -eq 0 ]; then
		die "internal/${NAME}/ already exists; refusing to overwrite (and --no-wire leaves nothing to do)"
	fi
	echo "note: internal/${NAME}/ already exists; skipping scaffold, running idempotent wiring only."
	SCAFFOLD=0
fi

# --- derive tokens ----------------------------------------------------------
DOMAIN_LOWER="${NAME}"
# PascalCase: uppercase first letter, keep the rest.
DOMAIN_PASCAL="$(printf '%s' "${NAME}" | awk '{ print toupper(substr($0,1,1)) substr($0,2) }')"
# Screaming upper.
DOMAIN_UPPER="$(printf '%s' "${NAME}" | tr '[:lower:]' '[:upper:]')"
# Plural: explicit arg wins, else naive append "s".
if [ -n "${PLURAL_ARG}" ]; then
	if ! printf '%s' "${PLURAL_ARG}" | grep -Eq '^[a-z]+$'; then
		die "plural must be lowercase letters only (got: '${PLURAL_ARG}')"
	fi
	DOMAINS_LOWER="${PLURAL_ARG}"
else
	DOMAINS_LOWER="${NAME}s"
fi

# --- next migration number --------------------------------------------------
# Scan existing internal/*/migrations for the highest NNNNNN prefix, add 1.
HIGHEST=0
while IFS= read -r f; do
	base="$(basename "${f}")"
	num="${base%%_*}"
	# Strip leading zeros for arithmetic; guard against empty.
	case "${num}" in
		''|*[!0-9]*) continue ;;
	esac
	n=$((10#${num}))
	if [ "${n}" -gt "${HIGHEST}" ]; then
		HIGHEST="${n}"
	fi
done < <(find "${REPO_ROOT}/internal" -path '*/migrations/*.sql' 2>/dev/null)

NEXT=$((HIGHEST + 1))
MIGNUM="$(printf '%06d' "${NEXT}")"

# --- copy + token replace ---------------------------------------------------
if [ "${SCAFFOLD}" -eq 1 ]; then
echo "Scaffolding domain '${NAME}' -> internal/${NAME}/"
echo "  singular : ${DOMAIN_LOWER} / ${DOMAIN_PASCAL}"
echo "  plural   : ${DOMAINS_LOWER}"
echo "  migration: ${MIGNUM}"

mkdir -p "${DEST}"
# Copy the whole template tree, then drop template-only docs.
cp -R "${TEMPLATE_DIR}/." "${DEST}/"
rm -f "${DEST}/TOKENS.md"

# Replace tokens in file CONTENTS. Order matters: replace the longer/cased
# tokens before the lowercase one so e.g. __Domain__ is not eaten by __domain__
# (they are distinct strings, but we keep a deterministic order anyway).
replace_contents() {
	local file="$1"
	# Use a temp file for portable in-place editing (BSD/GNU sed differ on -i).
	sed \
		-e "s|__MODULE__|${MODULE}|g" \
		-e "s|__MIGNUM__|${MIGNUM}|g" \
		-e "s|__Domain__|${DOMAIN_PASCAL}|g" \
		-e "s|__DOMAIN__|${DOMAIN_UPPER}|g" \
		-e "s|__domains__|${DOMAINS_LOWER}|g" \
		-e "s|__domain__|${DOMAIN_LOWER}|g" \
		"${file}" > "${file}.tmp"
	mv "${file}.tmp" "${file}"
}

# Process contents of every file.
while IFS= read -r f; do
	replace_contents "${f}"
done < <(find "${DEST}" -type f)

# Replace tokens in file NAMES. Rename deepest paths first so parent renames
# don't invalidate child paths. (Our template has no token dir names, but this
# is safe and future-proof.)
while IFS= read -r path; do
	newpath="$(printf '%s' "${path}" \
		| sed \
			-e "s|__MIGNUM__|${MIGNUM}|g" \
			-e "s|__Domain__|${DOMAIN_PASCAL}|g" \
			-e "s|__DOMAIN__|${DOMAIN_UPPER}|g" \
			-e "s|__domains__|${DOMAINS_LOWER}|g" \
			-e "s|__domain__|${DOMAIN_LOWER}|g")"
	if [ "${newpath}" != "${path}" ]; then
		mv "${path}" "${newpath}"
	fi
done < <(find "${DEST}" -depth -name '*__*')

# --- verify no tokens remain ------------------------------------------------
if grep -rn '__' "${DEST}" >/dev/null 2>&1; then
	echo "warning: placeholder tokens remain in generated output:" >&2
	grep -rn '__' "${DEST}" >&2 || true
	die "generation left unreplaced tokens; please review internal/${NAME}/"
fi
fi  # end SCAFFOLD

# --- auto-wiring ------------------------------------------------------------
PKG_BASE="${MODULE}/internal/${NAME}"

PROVIDERS_GO="${REPO_ROOT}/cmd/server/providers.go"
WIRE_GO="${REPO_ROOT}/cmd/server/wire.go"
CMD_ROUTER_GO="${REPO_ROOT}/cmd/server/router.go"
PLATFORM_ROUTER_GO="${REPO_ROOT}/internal/platform/router.go"
PLATFORM_APP_GO="${REPO_ROOT}/internal/platform/app.go"

# insert_before_anchor <file> <anchor-substr> <unique-marker> <payload>
# Inserts <payload> on the line(s) immediately before the FIRST line that
# contains <anchor-substr>. Skips (idempotent) if <unique-marker> is already
# present anywhere in <file>. The payload is inserted verbatim (its own
# indentation must be supplied by the caller).
insert_before_anchor() {
	local file="$1" anchor="$2" marker="$3" payload="$4"

	[ -f "${file}" ] || die "auto-wire: file not found: ${file}"
	if ! grep -qF -- "${anchor}" "${file}"; then
		die "auto-wire: anchor '${anchor}' not found in ${file}; cannot wire automatically"
	fi
	if grep -qF -- "${marker}" "${file}"; then
		echo "  skip (already wired): ${file##"${REPO_ROOT}/"} [${marker}]"
		return 0
	fi

	# awk inserts before the first anchor match only; payload passed via env to
	# avoid quoting/escaping pitfalls.
	PAYLOAD="${payload}" ANCHOR="${anchor}" awk '
		BEGIN { anchor = ENVIRON["ANCHOR"]; payload = ENVIRON["PAYLOAD"]; done = 0 }
		(done == 0 && index($0, anchor) > 0) {
			printf "%s\n", payload
			done = 1
		}
		{ print }
	' "${file}" > "${file}.tmp"
	mv "${file}.tmp" "${file}"
	echo "  wired: ${file##"${REPO_ROOT}/"} [${marker}]"
}

if [ "${WIRE}" -eq 1 ]; then
	echo "Auto-wiring domain '${NAME}' into cmd/server + internal/platform ..."

	# 1) cmd/server/providers.go ---------------------------------------------
	insert_before_anchor "${PROVIDERS_GO}" "// new-domain:imports" \
		"${PKG_BASE}/handler" \
		"	${NAME}handler \"${PKG_BASE}/handler\"
	${NAME}repo \"${PKG_BASE}/repository\"
	${NAME}service \"${PKG_BASE}/service\""

	insert_before_anchor "${PROVIDERS_GO}" "// new-domain:providers" \
		"func provide${DOMAIN_PASCAL}Repository(" \
		"func provide${DOMAIN_PASCAL}Repository(pool *pgxpool.Pool, ob *outbox.PostgresRepository) *${NAME}repo.Postgres${DOMAIN_PASCAL}Repository {
	return ${NAME}repo.NewPostgres${DOMAIN_PASCAL}Repository(pool, ob)
}

func provide${DOMAIN_PASCAL}Service(repo *${NAME}repo.Postgres${DOMAIN_PASCAL}Repository, auth authz.Authorizer) ${NAME}service.${DOMAIN_PASCAL}Service {
	return ${NAME}service.New${DOMAIN_PASCAL}Service(repo, auth)
}

func provide${DOMAIN_PASCAL}Handler(svc ${NAME}service.${DOMAIN_PASCAL}Service) *${NAME}handler.Handler {
	return ${NAME}handler.NewHandler(svc)
}
"

	# 2) cmd/server/wire.go ---------------------------------------------------
	# wire.go only lists provider function names (they live in providers.go, same
	# package), so it needs NO new import for this domain — and no wire.Bind:
	# provide${DOMAIN_PASCAL}Service
	# consumes the CONCRETE *Postgres${DOMAIN_PASCAL}Repository (which satisfies the
	# interface structurally), so an interface binding would be unused and wire
	# fails with "unused interface binding". This mirrors the notification domain.
	insert_before_anchor "${WIRE_GO}" "// new-domain:wire" \
		"provide${DOMAIN_PASCAL}Handler," \
		"		provide${DOMAIN_PASCAL}Repository,
		provide${DOMAIN_PASCAL}Service,
		provide${DOMAIN_PASCAL}Handler,"

	# 3) cmd/server/router.go (provideAppRouter forwards to platform.NewRouter)
	insert_before_anchor "${CMD_ROUTER_GO}" "// new-domain:imports" \
		"${PKG_BASE}/handler" \
		"	${NAME}handler \"${PKG_BASE}/handler\""

	insert_before_anchor "${CMD_ROUTER_GO}" "// new-domain:approuter-params" \
		"${NAME}H *${NAME}handler.Handler," \
		"	${NAME}H *${NAME}handler.Handler,"

	insert_before_anchor "${CMD_ROUTER_GO}" "// new-domain:approuter-args" \
		"		${NAME}H," \
		"		${NAME}H,"

	# 4) internal/platform/router.go -----------------------------------------
	insert_before_anchor "${PLATFORM_ROUTER_GO}" "// new-domain:imports" \
		"${PKG_BASE}/handler" \
		"	${NAME}handler \"${PKG_BASE}/handler\""

	insert_before_anchor "${PLATFORM_ROUTER_GO}" "// new-domain:router-params" \
		"${NAME}H *${NAME}handler.Handler," \
		"	${NAME}H *${NAME}handler.Handler,"

	insert_before_anchor "${PLATFORM_ROUTER_GO}" "// new-domain:router-mount" \
		"${NAME}H.Routes(r)" \
		"	${NAME}H.Routes(r)"

	# 5) internal/platform/app.go --------------------------------------------
	# BuildApp is a second, hand-wired copy of the graph (used by test/e2e). It
	# also calls NewRouter, so it must construct this domain's repo/service/
	# handler and pass ${NAME}H, or it stops compiling once NewRouter grows a param.
	insert_before_anchor "${PLATFORM_APP_GO}" "// new-domain:imports" \
		"${PKG_BASE}/handler" \
		"	${NAME}handler \"${PKG_BASE}/handler\"
	${NAME}repo \"${PKG_BASE}/repository\"
	${NAME}service \"${PKG_BASE}/service\""

	insert_before_anchor "${PLATFORM_APP_GO}" "// new-domain:buildapp-construct" \
		"${NAME}H := ${NAME}handler.NewHandler(" \
		"	${NAME}Repo := ${NAME}repo.NewPostgres${DOMAIN_PASCAL}Repository(pool, obRepo)
	${NAME}Svc := ${NAME}service.New${DOMAIN_PASCAL}Service(${NAME}Repo, authorizer)
	${NAME}H := ${NAME}handler.NewHandler(${NAME}Svc)"

	insert_before_anchor "${PLATFORM_APP_GO}" "// new-domain:buildapp-args" \
		"		${NAME}H," \
		"		${NAME}H,"

	echo "Auto-wiring complete."
fi

# --- next steps -------------------------------------------------------------
cat <<EOF

Generated internal/${NAME}/:
$(cd "${REPO_ROOT}" && find "internal/${NAME}" -type f | sort | sed 's/^/  /')
EOF

if [ "${WIRE}" -eq 1 ]; then
cat <<EOF

================================ NEXT STEPS ================================
The new domain has been AUTO-WIRED into:
  - cmd/server/providers.go      (imports + provide${DOMAIN_PASCAL}{Repository,Service,Handler})
  - cmd/server/wire.go           (provider refs; no wire.Bind — provider takes concrete repo)
  - cmd/server/router.go         (provideAppRouter param + forwarded arg)
  - internal/platform/router.go  (NewRouter param + ${NAME}H.Routes(r) mount)
  - internal/platform/app.go     (BuildApp: repo/svc/handler construct + NewRouter arg)

You still need to finish the loop:

1) Regenerate wire (the injector output is NOT auto-updated):

       make wire     # runs wire pinned to go.mod's version (not @latest)
       make mocks    # only if you add this repo to .mockery.yaml

2) Run the migration ${MIGNUM} (internal/${NAME}/migrations/) against your DB.

3) Build + test:

       go build ./...
       go test ./internal/${NAME}/...

4) Authorization: the service uses actions "${NAME}:list|read|create|update|
   delete". They work as-is under AUTHZ_MODE=allow. For rbac/opa, add matching
   rules (internal/pkg/authz/rbac_authorizer.go and/or the OPA policies).

(Re-run with --no-wire to scaffold without touching cmd/server + platform.)
===========================================================================
EOF
exit 0
fi

cat <<EOF

================================ NEXT STEPS ================================
The new domain is NOT wired into the app yet (--no-wire). Add the following
(do not let 'go generate'/wire run until these compile):

1) cmd/server/providers.go — add imports and providers:

   import (
       ${NAME}handler "${PKG_BASE}/handler"
       ${NAME}repo    "${PKG_BASE}/repository"
       ${NAME}service "${PKG_BASE}/service"
   )

   func provide${DOMAIN_PASCAL}Repository(pool *pgxpool.Pool, ob *outbox.PostgresRepository) *${NAME}repo.Postgres${DOMAIN_PASCAL}Repository {
       return ${NAME}repo.NewPostgres${DOMAIN_PASCAL}Repository(pool, ob)
   }

   func provide${DOMAIN_PASCAL}Service(repo *${NAME}repo.Postgres${DOMAIN_PASCAL}Repository, auth authz.Authorizer) ${NAME}service.${DOMAIN_PASCAL}Service {
       return ${NAME}service.New${DOMAIN_PASCAL}Service(repo, auth)
   }

   func provide${DOMAIN_PASCAL}Handler(svc ${NAME}service.${DOMAIN_PASCAL}Service) *${NAME}handler.Handler {
       return ${NAME}handler.NewHandler(svc)
   }

   NOTE: NewPostgres${DOMAIN_PASCAL}Repository takes outbox.Repository; the
   provider above binds the concrete *outbox.PostgresRepository (wire already
   provides it via provideOutboxRepository).

2) cmd/server/wire.go — add to wire.Build(...):

       provide${DOMAIN_PASCAL}Repository,
       provide${DOMAIN_PASCAL}Service,
       provide${DOMAIN_PASCAL}Handler,
       wire.Bind(new(outbox.Repository), new(*outbox.PostgresRepository)),   // only if not already bound

   Do NOT add wire.Bind for the ${DOMAIN_PASCAL}Repository interface:
   provide${DOMAIN_PASCAL}Service takes the concrete *Postgres${DOMAIN_PASCAL}Repository,
   so an interface binding would be unused and wire would fail. wire.go needs no
   new import for this domain (it only lists provider names from providers.go);
   add the outbox import "${MODULE}/internal/pkg/outbox" only if missing.

3) Mount the router. The handler exposes Routes(chi.Router). Pass
   *${NAME}handler.Handler into provideAppRouter (cmd/server/router.go) and on
   into platform.NewRouter (internal/platform/router.go), then inside that
   router call:

       ${NAME}H.Routes(r)

   alongside the existing notificationH.Routes(r) / h.Routes(r) calls.

4) Regenerate wire + mocks:

       make wire     # runs wire pinned to go.mod's version (not @latest)
       make mocks    # only if you add this repo to .mockery.yaml

5) Run the migration ${MIGNUM} (internal/${NAME}/migrations/) against your DB,
   then:

       go build ./...
       go test ./internal/${NAME}/...

6) Authorization: the service uses actions "${NAME}:list|read|create|update|
   delete". They work as-is under AUTHZ_MODE=allow. For rbac/opa, add matching
   rules (internal/pkg/authz/rbac_authorizer.go and/or the OPA policies).
===========================================================================
EOF
