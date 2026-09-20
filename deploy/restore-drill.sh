#!/usr/bin/env bash
# Proves a backup is actually restorable, not just that backup.sh exited 0.
# Spins up a THROWAWAY compose project (postgres + app only, isolated network
# and volumes, never the real stack), restores a backup into it, checks the
# restored numbers against the backup's manifest.json, and hits the real
# /ready endpoint through the restored app. Always tears the throwaway
# project down (including its volumes) on exit, pass or fail.
#
# Usage: deploy/restore-drill.sh [backup-dir]   (default: newest under
# BACKUP_DIR / deploy/backups)
#
# Prints a line starting with "RESTORE DRILL: PASS" or "RESTORE DRILL: FAIL"
# as the last line of output, and exits 0 / 1 to match.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/../.env.production}"
BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/backups}"
DRILL_PROJECT="restore-drill-$$"

# docker-compose.prod.yml's image: lines require IMAGE_TAG (no `latest`
# fallback — see its comment). This throwaway drill project is not a real
# deploy, so default to the current commit's short sha the same way the
# Makefile's prod-* targets do, rather than making every caller set it.
# Exported: docker compose reads it from the environment, and the whole
# compose file is interpolated (all services, even ones this script never
# starts) for ANY invocation, so it must be set before the first `compose`
# call, not just before starting `app`.
export IMAGE_TAG="${IMAGE_TAG:-$(git -C "$SCRIPT_DIR/.." rev-parse --short HEAD 2>/dev/null || echo unknown)}"

log() { echo "[restore-drill] $*" >&2; }
fail() {
	log "FAIL: $*"
	echo "RESTORE DRILL: FAIL — $*"
	exit 1
}

BACKUP_DIR_ARG="${1:-}"
if [[ -z "$BACKUP_DIR_ARG" ]]; then
	BACKUP_DIR_ARG="$(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d | sort | tail -n1 || true)"
	[[ -n "$BACKUP_DIR_ARG" ]] || fail "no backup-dir given and none found under $BACKUP_DIR"
fi
[[ -f "$BACKUP_DIR_ARG/manifest.json" ]] || fail "missing manifest.json in $BACKUP_DIR_ARG"
log "drilling: $BACKUP_DIR_ARG (project: $DRILL_PROJECT)"

json_num() { # json_num <file> <key>
	grep -o "\"$2\"[[:space:]]*:[[:space:]]*[0-9]*" "$1" | grep -o '[0-9]*$'
}

# Pulls "name": count pairs out of manifest.json's "tables": { ... } block, as
# "table_name|count" lines — matches the format deploy/backup.sh writes (one
# entry per line, no nesting inside the block), so this is a plain line
# extraction rather than a real JSON parser.
manifest_table_counts() { # manifest_table_counts <manifest.json>
	awk '/"tables"[[:space:]]*:[[:space:]]*\{/{flag=1; next} flag && /^[[:space:]]*\}/{flag=0} flag' "$1" \
		| grep -oE '"[A-Za-z0-9_]+"[[:space:]]*:[[:space:]]*[0-9]+' \
		| sed -E 's/"([A-Za-z0-9_]+)"[[:space:]]*:[[:space:]]*([0-9]+)/\1|\2/' \
		| sort
}

EXPECT_VERSION="$(json_num "$BACKUP_DIR_ARG/manifest.json" migration_version)"
EXPECT_TABLE_COUNTS="$(manifest_table_counts "$BACKUP_DIR_ARG/manifest.json")"
[[ -n "$EXPECT_TABLE_COUNTS" ]] || fail "manifest.json has no \"tables\" entries — nothing to compare per-table"

compose() {
	docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" -p "$DRILL_PROJECT" "$@"
}

cleanup() {
	log "tearing down $DRILL_PROJECT (including volumes)..."
	compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "starting throwaway postgres..."
compose up -d postgres
# shellcheck disable=SC2016 # single-quoted on purpose: expands inside the
# postgres container's own shell, using the env vars the postgres image sets
# there — not the host shell's.
compose exec -T postgres sh -c 'until pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"; do sleep 1; done' >/dev/null

log "restoring backup into throwaway project..."
COMPOSE_FILE="$COMPOSE_FILE" ENV_FILE="$ENV_FILE" COMPOSE_PROJECT_NAME="$DRILL_PROJECT" \
	RESTORE_CONFIRM=yes "$SCRIPT_DIR/restore.sh" "$BACKUP_DIR_ARG" >&2

env_var() {
	local name="$1" default="${2:-}"
	local val=""
	val="$(grep -E "^${name}=" "$ENV_FILE" | tail -n1 | cut -d= -f2- || true)"
	echo "${val:-$default}"
}
POSTGRES_USER_VAL="$(env_var POSTGRES_USER app)"
POSTGRES_DB_VAL="$(env_var POSTGRES_DB app)"

# Same exact per-table COUNT(*) approach as deploy/backup.sh's table_counts —
# duplicated rather than shared because these are two independent scripts
# (this file already duplicates compose()/env_var() the same way) and the
# whole point of comparing against manifest.json is to not trust any
# statistics estimate on either side.
table_counts() {
	local union_sql
	union_sql="$(compose exec -T postgres psql -U "$POSTGRES_USER_VAL" -d "$POSTGRES_DB_VAL" -tAc "
		SELECT string_agg(format('SELECT %L::text AS t, count(*)::bigint AS c FROM %I.%I', table_name, table_schema, table_name), ' UNION ALL ' ORDER BY table_name)
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
	")"
	[[ -z "$union_sql" ]] && return 0
	compose exec -T postgres psql -U "$POSTGRES_USER_VAL" -d "$POSTGRES_DB_VAL" -tAc \
		"SELECT t || '|' || c FROM ($union_sql) x ORDER BY t"
}

ACTUAL_VERSION="$(compose exec -T postgres psql -U "$POSTGRES_USER_VAL" -d "$POSTGRES_DB_VAL" -tAc \
	"SELECT COALESCE(MAX(version), -1) FROM schema_migrations" | tr -d '[:space:]')"
ACTUAL_TABLE_COUNTS="$(table_counts | sort)"

log "migration_version: expected=$EXPECT_VERSION actual=$ACTUAL_VERSION"
[[ "$ACTUAL_VERSION" == "$EXPECT_VERSION" ]] || fail "migration_version mismatch (expected $EXPECT_VERSION, got $ACTUAL_VERSION)"

[[ -n "$ACTUAL_TABLE_COUNTS" ]] || fail "restored database has no base tables in public — nothing to compare"

# Per-table comparison, not just a summed total: two tables off by opposite
# amounts would cancel out in a sum and still "pass". diff the two
# "table|count" line sets directly; the first mismatching line becomes the
# failure reason (covers a missing/extra table too, not just a wrong count).
MISMATCH="$(diff <(printf '%s\n' "$EXPECT_TABLE_COUNTS") <(printf '%s\n' "$ACTUAL_TABLE_COUNTS") | grep -E '^[<>]' | head -n1 || true)"
if [[ -n "$MISMATCH" ]]; then
	fail "table row count mismatch — first difference: $MISMATCH (expected line starts '<', actual starts '>')"
fi
log "table counts: all $(printf '%s\n' "$EXPECT_TABLE_COUNTS" | wc -l | tr -d '[:space:]') tables match manifest.json exactly"

ACTUAL_ROWS=0
while IFS='|' read -r _tname tcount; do
	[[ -z "$tcount" ]] && continue
	ACTUAL_ROWS=$((ACTUAL_ROWS + tcount))
done <<<"$ACTUAL_TABLE_COUNTS"
log "db_row_count (sum of exact per-table counts): $ACTUAL_ROWS"

# An empty dump would still satisfy "every table count matches" if the source
# was itself empty — that's not a passing drill, it's a drill that never
# proved anything got restored. schema_migrations is bookkeeping, not data.
NONEMPTY_TABLES=0
while IFS='|' read -r tname tcount; do
	[[ -z "$tname" || "$tname" == "schema_migrations" ]] && continue
	[[ "$tcount" -gt 0 ]] && NONEMPTY_TABLES=$((NONEMPTY_TABLES + 1))
done <<<"$ACTUAL_TABLE_COUNTS"
[[ "$NONEMPTY_TABLES" -gt 0 ]] || fail "every non-migration table is empty after restore — refusing to PASS an empty dump"

log "starting app against the restored database (--no-deps: skip migrate, the dump is already migrated)..."
compose up -d --no-deps app
for _ in $(seq 1 30); do
	READY_BODY="$(compose exec -T app wget -qO- http://127.0.0.1:8080/ready 2>/dev/null || true)"
	[[ "$READY_BODY" == *'"db":"ok"'* ]] && break
	sleep 1
done
[[ "$READY_BODY" == *'"db":"ok"'* ]] || fail "/ready never reported db ok against the restored database (last body: ${READY_BODY:-<empty>})"
log "/ready: $READY_BODY"

if [[ -d "$BACKUP_DIR_ARG/media" ]]; then
	if [[ -n "${BACKUP_MEDIA_RESTORE_CMD:-}" ]]; then
		log "media/ present and BACKUP_MEDIA_RESTORE_CMD set — restoring to verify..."
		BACKUP_MEDIA_SRC="$BACKUP_DIR_ARG/media" bash -c "$BACKUP_MEDIA_RESTORE_CMD" >&2
		log "media restore command completed without error"
	else
		log "media/ present in backup but BACKUP_MEDIA_RESTORE_CMD not set — media restore NOT exercised by this drill"
	fi
fi

echo "RESTORE DRILL: PASS ($BACKUP_DIR_ARG, migration_version=$ACTUAL_VERSION, db_row_count=$ACTUAL_ROWS)"
