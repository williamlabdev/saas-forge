#!/usr/bin/env bash
# Restores a deploy/backup.sh backup into the stack named by
# COMPOSE_FILE/ENV_FILE/COMPOSE_PROJECT_NAME (defaults: this directory's
# production stack). DESTRUCTIVE — pg_restore --clean drops every object the
# dump touches before recreating it. Requires RESTORE_CONFIRM=yes.
#
# Usage: RESTORE_CONFIRM=yes deploy/restore.sh <backup-dir>
#
# Env: same COMPOSE_FILE / ENV_FILE / COMPOSE_PROJECT_NAME as deploy/backup.sh.
#   BACKUP_MEDIA_RESTORE_CMD  shell command that copies media FROM
#                             $BACKUP_MEDIA_SRC back into storage. Unset skips
#                             media restore (db-only).
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/../.env.production}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-}"

# See deploy/backup.sh's identical block: docker compose interpolates every
# service's image: line for ANY invocation, including this script's
# postgres-only restore, so IMAGE_TAG must be set even though restore.sh
# itself never touches app/bff/etc.
export IMAGE_TAG="${IMAGE_TAG:-$(git -C "$SCRIPT_DIR/.." rev-parse --short HEAD 2>/dev/null || echo restore)}"

BACKUP_DIR_ARG="${1:?usage: restore.sh <backup-dir>}"

log() { echo "[restore] $*" >&2; }

compose() {
	local args=(-f "$COMPOSE_FILE")
	[[ -f "$ENV_FILE" ]] && args+=(--env-file "$ENV_FILE")
	[[ -n "$COMPOSE_PROJECT_NAME" ]] && args+=(-p "$COMPOSE_PROJECT_NAME")
	docker compose "${args[@]}" "$@"
}

env_var() {
	local name="$1" default="${2:-}"
	local val=""
	if [[ -f "$ENV_FILE" ]]; then
		val="$(grep -E "^${name}=" "$ENV_FILE" | tail -n1 | cut -d= -f2- || true)"
	fi
	echo "${val:-$default}"
}

[[ -d "$BACKUP_DIR_ARG" ]] || {
	echo "no such backup dir: $BACKUP_DIR_ARG" >&2
	exit 1
}
[[ -f "$BACKUP_DIR_ARG/db.dump" ]] || {
	echo "missing db.dump in $BACKUP_DIR_ARG" >&2
	exit 1
}
[[ -f "$BACKUP_DIR_ARG/manifest.json" ]] || {
	echo "missing manifest.json in $BACKUP_DIR_ARG — refusing an unverifiable backup" >&2
	exit 1
}

log "target: $COMPOSE_FILE (project: ${COMPOSE_PROJECT_NAME:-default})"
log "manifest for $BACKUP_DIR_ARG:"
cat "$BACKUP_DIR_ARG/manifest.json" >&2

if [[ "${RESTORE_CONFIRM:-}" != "yes" ]]; then
	echo "Refusing to proceed: this OVERWRITES the target database. Set RESTORE_CONFIRM=yes to continue." >&2
	exit 1
fi

POSTGRES_USER_VAL="$(env_var POSTGRES_USER app)"
POSTGRES_DB_VAL="$(env_var POSTGRES_DB app)"

# pg_restore needs a seekable file for the custom (-Fc) format — it cannot
# reliably read that format from a pipe — so the dump is copied into the
# container rather than streamed over `exec -T`'s stdin.
log "copying dump into the postgres container..."
compose cp "$BACKUP_DIR_ARG/db.dump" postgres:/tmp/restore.dump

log "pg_restore --clean --if-exists into $POSTGRES_DB_VAL..."
compose exec -T postgres pg_restore --clean --if-exists --no-owner \
	-U "$POSTGRES_USER_VAL" -d "$POSTGRES_DB_VAL" /tmp/restore.dump
compose exec -T postgres rm -f /tmp/restore.dump

if [[ -d "$BACKUP_DIR_ARG/media" && -n "${BACKUP_MEDIA_RESTORE_CMD:-}" ]]; then
	log "restoring media via BACKUP_MEDIA_RESTORE_CMD..."
	BACKUP_MEDIA_SRC="$BACKUP_DIR_ARG/media" bash -c "$BACKUP_MEDIA_RESTORE_CMD"
elif [[ -d "$BACKUP_DIR_ARG/media" ]]; then
	log "media/ present in backup but BACKUP_MEDIA_RESTORE_CMD not set — skipping media restore"
fi

log "done"
