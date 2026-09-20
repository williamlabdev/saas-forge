#!/usr/bin/env bash
# Backs up Postgres (pg_dump -Fc) and, if configured, media, for the stack
# named by COMPOSE_FILE/ENV_FILE/COMPOSE_PROJECT_NAME. Defaults to this
# directory's production stack — deploy/restore-drill.sh reuses this same
# script against a throwaway project by overriding those three vars, so
# backup.sh itself stays project-agnostic.
#
# Usage: deploy/backup.sh
# Env:
#   BACKUP_DIR            where timestamped backup dirs are written (default:
#                          deploy/backups)
#   BACKUP_KEEP_DAYS       delete backup dirs older than this many days after a
#                          successful run (default: 14; 0 disables cleanup)
#   BACKUP_MEDIA_CMD       shell command that copies media into
#                          $BACKUP_MEDIA_DEST (e.g. an `mc mirror` or
#                          `aws s3 sync` invocation). Unset skips media —
#                          this script never guesses at your object storage.
#   COMPOSE_FILE            compose file to target (default: ./docker-compose.prod.yml)
#   ENV_FILE                 env file passed to `docker compose --env-file` and
#                          read for POSTGRES_USER/POSTGRES_DB (default:
#                          ../.env.production next to COMPOSE_FILE)
#   COMPOSE_PROJECT_NAME  passed to `docker compose -p` when set
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/../.env.production}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-}"
BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/backups}"
BACKUP_KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"

# docker-compose.prod.yml's image: lines require IMAGE_TAG (see its comment).
# docker compose interpolates EVERY service's image: line for ANY invocation,
# including this script's postgres-only `exec` — so a cron-driven backup with
# no IMAGE_TAG pinned in ENV_FILE would otherwise fail before pg_dump ever
# runs. The value is irrelevant here (backup.sh never pulls or starts app/
# bff/etc.), so prefer the current commit's short sha (best-effort — cron's
# cwd may not be the checkout and git may be absent) and fall back to a fixed
# placeholder purely to satisfy interpolation.
export IMAGE_TAG="${IMAGE_TAG:-$(git -C "$SCRIPT_DIR/.." rev-parse --short HEAD 2>/dev/null || echo backup)}"

log() { echo "[backup] $*" >&2; }

compose() {
	local args=(-f "$COMPOSE_FILE")
	[[ -f "$ENV_FILE" ]] && args+=(--env-file "$ENV_FILE")
	[[ -n "$COMPOSE_PROJECT_NAME" ]] && args+=(-p "$COMPOSE_PROJECT_NAME")
	docker compose "${args[@]}" "$@"
}

# Reads NAME=value out of ENV_FILE without sourcing it (the file also holds
# secrets we have no reason to export into this shell's environment wholesale).
env_var() {
	local name="$1" default="${2:-}"
	local val=""
	if [[ -f "$ENV_FILE" ]]; then
		val="$(grep -E "^${name}=" "$ENV_FILE" | tail -n1 | cut -d= -f2- || true)"
	fi
	echo "${val:-$default}"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# Exact per-table row counts for every base table in the public schema, as
# "table_name|count" lines sorted by table name. NOT pg_stat_user_tables'
# n_live_tup — that column is a planner statistics ESTIMATE (refreshed by
# autovacuum/analyze, not by row changes), and was observed off by one row on
# a live table during verification. A restore drill needs an exact figure to
# compare against, so this runs a real COUNT(*) per table: first ask Postgres
# to build one UNION ALL query text over information_schema.tables (so the
# table list itself is read exactly once, inside the database), then execute
# that generated query to get every count in a single second round trip.
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

# Renders "table_name|count" lines (as produced by table_counts) into a JSON
# object, indented to nest under a "tables" key.
build_tables_json() {
	local first=1 tname tcount out
	out=$'{\n'
	while IFS='|' read -r tname tcount; do
		[[ -z "$tname" ]] && continue
		[[ $first -eq 0 ]] && out+=$',\n'
		first=0
		out+="    \"${tname}\": ${tcount}"
	done
	out+=$'\n  }'
	printf '%s' "$out"
}

POSTGRES_USER_VAL="$(env_var POSTGRES_USER app)"
POSTGRES_DB_VAL="$(env_var POSTGRES_DB app)"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$BACKUP_DIR/$STAMP"
mkdir -p "$DEST"
log "writing to $DEST"

log "pg_dump ($POSTGRES_DB_VAL as $POSTGRES_USER_VAL)..."
compose exec -T postgres pg_dump -U "$POSTGRES_USER_VAL" -Fc -d "$POSTGRES_DB_VAL" >"$DEST/db.dump"

MIGRATION_VERSION="$(compose exec -T postgres psql -U "$POSTGRES_USER_VAL" -d "$POSTGRES_DB_VAL" -tAc \
	"SELECT COALESCE(MAX(version), -1) FROM schema_migrations" | tr -d '[:space:]')"

TABLE_COUNTS_RAW="$(table_counts)"
TABLES_JSON="$(build_tables_json <<<"$TABLE_COUNTS_RAW")"
ROW_COUNT=0
while IFS='|' read -r _tname tcount; do
	[[ -z "$tcount" ]] && continue
	ROW_COUNT=$((ROW_COUNT + tcount))
done <<<"$TABLE_COUNTS_RAW"

MEDIA_FILES=0
MEDIA_BYTES=0
if [[ -n "${BACKUP_MEDIA_CMD:-}" ]]; then
	log "backing up media via BACKUP_MEDIA_CMD..."
	mkdir -p "$DEST/media"
	BACKUP_MEDIA_DEST="$DEST/media" bash -c "$BACKUP_MEDIA_CMD"
	MEDIA_FILES="$(find "$DEST/media" -type f | wc -l | tr -d '[:space:]')"
	MEDIA_BYTES="$(find "$DEST/media" -type f -exec du -k {} + 2>/dev/null | awk '{s+=$1} END {print s*1024+0}')"
else
	log "BACKUP_MEDIA_CMD not set — skipping media backup (db-only)"
fi

GIT_SHA="$(git -C "$SCRIPT_DIR/.." rev-parse HEAD 2>/dev/null || echo unknown)"
DB_SHA256="$(sha256_of "$DEST/db.dump")"

cat >"$DEST/manifest.json" <<JSON
{
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "git_sha": "$GIT_SHA",
  "compose_file": "$COMPOSE_FILE",
  "migration_version": ${MIGRATION_VERSION:-null},
  "db_row_count": ${ROW_COUNT:-0},
  "db_dump_sha256": "$DB_SHA256",
  "media_file_count": ${MEDIA_FILES:-0},
  "media_total_bytes": ${MEDIA_BYTES:-0},
  "tables": ${TABLES_JSON}
}
JSON

log "manifest:"
cat "$DEST/manifest.json" >&2

if [[ "$BACKUP_KEEP_DAYS" -gt 0 ]]; then
	log "pruning backups older than ${BACKUP_KEEP_DAYS}d in $BACKUP_DIR"
	find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -mtime "+${BACKUP_KEEP_DAYS}" -exec rm -rf {} +
fi

log "done: $DEST"
echo "$DEST"
