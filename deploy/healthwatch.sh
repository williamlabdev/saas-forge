#!/usr/bin/env bash
# Polls the stack's public endpoints plus local disk headroom and POSTs a JSON
# alert to ALERT_WEBHOOK_URL on state CHANGES only (dedup via STATE_FILE) — a
# cron running this every few minutes pages once when something breaks and
# once when it recovers, not on every run in between. See deploy/cron.example.
#
# Usage (cron): */5 * * * * deploy/healthwatch.sh   (with env set — see below,
# or export them from a sourced file in the crontab entry itself)
#
# Env:
#   API_DOMAIN, DELIVERY_DOMAIN, CONSOLE_DOMAIN   checked over HTTPS; any unset
#                                                  domain is skipped
#   ALERT_WEBHOOK_URL   generic JSON webhook. {"text": "..."} works as-is for
#                       Slack/Mattermost incoming webhooks; point it at
#                       something else (PagerDuty, a custom endpoint) and adapt
#                       the payload below.
#   DISK_MIN_FREE_PCT  alert when free space on / falls below this (default 15)
#   STATE_FILE         dedup state file (default: deploy/.healthwatch.state)
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
STATE_FILE="${STATE_FILE:-$SCRIPT_DIR/.healthwatch.state}"
DISK_MIN_FREE_PCT="${DISK_MIN_FREE_PCT:-15}"

log() { echo "[healthwatch] $*" >&2; }

check_url() { # check_url <url> -> "ok" | "fail(http=NNN)"
	local url="$1" code
	code="$(curl -fsS -o /dev/null -w '%{http_code}' --max-time 10 "$url" 2>/dev/null || echo 000)"
	if [[ "$code" == "200" ]]; then
		echo "ok"
	else
		echo "fail(http=$code)"
	fi
}

results=()
[[ -n "${API_DOMAIN:-}" ]] && results+=("api:$(check_url "https://$API_DOMAIN/ready")")
[[ -n "${DELIVERY_DOMAIN:-}" ]] && results+=("delivery:$(check_url "https://$DELIVERY_DOMAIN/health")")
[[ -n "${CONSOLE_DOMAIN:-}" ]] && results+=("console:$(check_url "https://$CONSOLE_DOMAIN/")")

disk_free_pct="$(df -kP / | awk 'NR==2 {if ($2>0) printf "%d", ($4/$2)*100; else print 100}')"
disk_state="ok"
[[ "$disk_free_pct" -lt "$DISK_MIN_FREE_PCT" ]] && disk_state="fail(free=${disk_free_pct}pct)"
results+=("disk:$disk_state")

CURRENT_STATE="$(printf '%s\n' "${results[@]}" | sort)"
PREVIOUS_STATE=""
[[ -f "$STATE_FILE" ]] && PREVIOUS_STATE="$(cat "$STATE_FILE")"

HAS_FAILURE=0
for r in "${results[@]}"; do
	[[ "$r" == *fail* ]] && HAS_FAILURE=1
done

log "state: ${results[*]}"

if [[ "$CURRENT_STATE" != "$PREVIOUS_STATE" ]]; then
	STATUS_WORD="RECOVERED"
	[[ "$HAS_FAILURE" -eq 1 ]] && STATUS_WORD="ALERT"
	MESSAGE="[$STATUS_WORD] $(date -u +%Y-%m-%dT%H:%M:%SZ) - ${results[*]}"
	log "state changed - notifying: $MESSAGE"
	if [[ -n "${ALERT_WEBHOOK_URL:-}" ]]; then
		curl -fsS --max-time 10 -X POST -H 'Content-Type: application/json' \
			-d "$(printf '{"text":"%s"}' "$MESSAGE")" \
			"$ALERT_WEBHOOK_URL" >/dev/null || log "webhook POST failed"
	else
		log "ALERT_WEBHOOK_URL not set - printing only"
	fi
	echo "$CURRENT_STATE" >"$STATE_FILE"
else
	log "state unchanged - no notification"
fi

[[ "$HAS_FAILURE" -eq 0 ]]
