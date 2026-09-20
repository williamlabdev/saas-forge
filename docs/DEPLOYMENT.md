# Production Deployment

A runbook for running this stack in production with `deploy/docker-compose.prod.yml`
and Caddy as the edge. Complements [`DEPLOYMENT_HARDENING.md`](DEPLOYMENT_HARDENING.md)
(the *why* and the environment-variable checklist) — this document is the
*how*: bring the stack up, keep it backed up, and know it's healthy.

Everything here is real, not aspirational: every file it references
(`deploy/*`, `.env.production.example`, `Makefile`) is in this repository.

## 1. Topology

```
                         ┌────────────┐
  internet ── :80/:443 ─▶│   caddy    │  TLS termination, only host ports published
                         └─────┬──────┘
             ┌─────────────────┼──────────────────┬────────────────────┐
             ▼                 ▼                   ▼                    ▼
        API_DOMAIN       DELIVERY_DOMAIN      CONSOLE_DOMAIN       MEDIA_DOMAIN (optional)
             │                 │              (static SPA +            │
             │                 │               /content-api/*)         │
             ▼                 ▼                   ▼                    ▼
       bff:4000          delivery:4100         app:8080             minio:9000
   (+ /ready → app:8080         │                  ▲               (self-hosted
     directly, see §3)          └──────────────────┘                media profile)
             │
             ▼
         app:8080
             │
             ▼
        postgres:5432
```

`app`, `bff`, `delivery`, `postgres` (and `cms-mcp`/`minio` if enabled) publish
**no host ports** — only `caddy` is reachable from outside the compose
network. This matches [`DEPLOYMENT_HARDENING.md` §4](DEPLOYMENT_HARDENING.md).

## 2. Prerequisites

- Docker Engine + the `docker compose` plugin (v2).
- Three DNS records pointing at this host: `API_DOMAIN`, `DELIVERY_DOMAIN`,
  `CONSOLE_DOMAIN` (a fourth, `MEDIA_DOMAIN`, only if self-hosting media —
  see §6). Automatic HTTPS needs port 80 reachable from the internet for the
  ACME HTTP-01 challenge.
- A built console SPA (from the `saas-platform-console` repo's `dist/` output),
  or accept serving a placeholder until you have one.
- Either a managed Postgres instance, or plan to use the bundled `postgres`
  service (§5).
- Either real S3-compatible object storage, or plan to use the bundled
  `minio` profile (§6).

## 3. First deploy

```bash
cp .env.production.example .env.production
# Generate every REPLACE_WITH_* secret separately:
openssl rand -hex 32   # JWT_SECRET_HEX, ENCRYPTION_KEY_HEX, GATEWAY_SECRET, ...
```

Fill in `.env.production`: domains, `ACME_EMAIL`, database (§5), media (§6),
and every secret. `ENCRYPTION_KEY_HEX` must be exactly 64 hex chars (32
bytes); `DELIVERY_JWT_SECRET_HEX` must differ from `JWT_SECRET_HEX` — the
server refuses to start otherwise (see `DEPLOYMENT_HARDENING.md`).

Build the console and point `CONSOLE_DIST` at its output, or leave the
default `./console-dist` and drop a placeholder `index.html` there for now
— see "Build the console" below.

Validate the compose file resolves before bringing anything up:

```bash
make prod-config
```

Bring the stack up:

```bash
make prod-up
```

This builds `app`, `bff`, `delivery` (and `migrate`) from this repo's
Dockerfiles, runs migrations once, then starts everything else. Caddy will
not report its dependent services healthy until `app`/`bff`/`delivery` pass
their own healthchecks — first boot can take a few tens of seconds.

Check it:

```bash
curl -f https://$API_DOMAIN/ready       # {"db":"ok"}
curl -f https://$DELIVERY_DOMAIN/health # ok
curl -f https://$CONSOLE_DOMAIN/        # the SPA shell
```

Tear down with `make prod-down` (add `-v` by hand, i.e.
`docker compose -f deploy/docker-compose.prod.yml --env-file .env.production down -v`,
only when you intend to discard the database volume too).

### Build the console

The console (`saas-platform-console`) is a Vite SPA — its backend addresses
are baked into the JS bundle at *build* time, not read from the environment
at runtime, so they must be set before `pnpm build` runs, and a rebuild is
required whenever a domain changes. See that repo's `.env.example` (lines
15-68) for the full explanation of each key; the values below are what a
deployment fronted by `deploy/Caddyfile` needs, matching what it actually
routes on `CONSOLE_DOMAIN`/`API_DOMAIN`/`DELIVERY_DOMAIN`:

```bash
cd ../saas-platform-console
cat >.env.production <<EOF
VITE_BFF_GRAPHQL_URL=https://$API_DOMAIN/graphql
VITE_CONTENT_API=/content-api
VITE_SCHEMA_SOURCE=api
VITE_DELIVERY_BASE=https://$DELIVERY_DOMAIN
EOF
pnpm install && pnpm build   # -> dist/
```

- `VITE_BFF_GRAPHQL_URL=https://$API_DOMAIN/graphql` — `API_DOMAIN`'s default
  route in `deploy/Caddyfile` is `bff:4000`, which serves `/graphql`.
- `VITE_CONTENT_API=/content-api` — a **same-origin, relative** path, not
  `https://$API_DOMAIN/...`. This has to be same-origin with the console
  itself: `CONSOLE_DOMAIN`'s `handle_path /content-api/*` block is what
  strips the prefix and injects `X-Gateway-Secret` when forwarding to `app`
  directly (see §7) — that's the one place a browser is allowed to reach
  the domain API without going through bff, and only because Caddy, not the
  browser, adds the header. Pointing this at the domain API's own origin
  instead gets `403 GATEWAY_REQUIRED` on every request, and pointing it at
  `API_DOMAIN` skips the gateway build entirely (that domain proxies to bff,
  not `app`).
- `VITE_SCHEMA_SOURCE=api` — pins the schema source explicitly. Leaving both
  this and `VITE_CONTENT_API` unset silently ships a fully-functional-looking
  console wired to nothing but static mock data (no error, no banner) —
  see the `.env.example` warning at lines 10-13.
- `VITE_DELIVERY_BASE=https://$DELIVERY_DOMAIN` — public base the console
  uses to build preview links; must be the public `DELIVERY_DOMAIN`; the
  compose network name (`delivery:4100`) is unreachable from a browser.
- Leave every `VITE_DEV_*` var **unset** — those are dev-only header
  overrides trusted only when the backend runs `AUTH_DEV_HEADERS=true`,
  which production must not.

Then point `CONSOLE_DIST` in this repo's `.env.production` at that `dist/`
directory (absolute or relative to `deploy/docker-compose.prod.yml`) before
`make prod-up`, or bind-mount it directly.

### Optional profiles

```bash
# Self-hosted media (see §6):
docker compose -f deploy/docker-compose.prod.yml --env-file .env.production --profile minio up -d

# Agent-facing MCP HTTP transport (ADR-013 step 6):
docker compose -f deploy/docker-compose.prod.yml --env-file .env.production --profile agents up -d
```

## 4. TLS modes

`CADDY_TLS_MODE` in `.env.production` selects Caddy's `tls` directive for
every site:

- **Empty (default)** — automatic HTTPS via ACME (Let's Encrypt), using
  `ACME_EMAIL`. Requires the domains to resolve publicly and port 80 to be
  reachable for the HTTP-01 challenge.
- **`internal`** — Caddy's local, self-signed CA. No ACME calls, works with
  domains that resolve only locally (`/etc/hosts` or `--resolve`). This is
  what `deploy/restore-drill.sh`-adjacent local verification and any
  from-scratch smoke test should use before pointing real DNS at the host.

## 5. Database

`DATABASE_URL` in `.env.production` is the single source of truth for where
`app`/`migrate` connect. Two supported shapes:

- **Managed Postgres (recommended)** — point `DATABASE_URL` at it directly
  (`sslmode=require`). The bundled `postgres` service in
  `deploy/docker-compose.prod.yml` still starts (compose has no per-service
  disable short of removing it from the file) but nothing connects to it;
  if you're committed to managed Postgres, delete the `postgres` service and
  `migrate`'s `depends_on: postgres` entry from your copy of the compose file.
- **Self-hosted (bundled `postgres` service)** — set `POSTGRES_USER` /
  `POSTGRES_PASSWORD` / `POSTGRES_DB`, and point `DATABASE_URL` at
  `postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@postgres:5432/$POSTGRES_DB?sslmode=disable`
  (`sslmode=disable` because the connection never leaves the compose
  network — see the commented-out example already in
  `.env.production.example`).

## 6. Media storage

Same two shapes as the database:

- **Real object storage (recommended)** — set `MEDIA_S3_*` to it. Apply CORS
  once with `deploy/s3-cors.json` after replacing `CONSOLE_ORIGIN` with the
  console's real origin:

  ```bash
  aws s3api put-bucket-cors --bucket your-media-bucket --cors-configuration file://deploy/s3-cors.json
  ```

  Uploads use a **presigned POST** (an S3 POST policy — see
  `internal/pkg/objectstore/store.go`'s `PresignPost`), not a presigned PUT,
  so the CORS rule allows `POST` (plus `GET`/`HEAD` for reading back), not `PUT`.

- **Self-hosted (bundled `minio` profile)** — set `MEDIA_S3_ENDPOINT=minio:9000`,
  `MEDIA_S3_USE_SSL=false`, and `MEDIA_S3_ACCESS_KEY`/`MEDIA_S3_SECRET_KEY`
  (used as MinIO's root credentials too). Because the **browser**, not the
  app, dereferences a presigned URL, and `minio:9000` is a compose-network
  name the browser cannot resolve, also set `MEDIA_S3_PUBLIC_ENDPOINT` to
  `MEDIA_DOMAIN` (routed by Caddy, see §1) and `MEDIA_S3_PUBLIC_USE_SSL=true`.
  Uncomment the `MEDIA_DOMAIN` site block in `deploy/Caddyfile` and set
  `MEDIA_DOMAIN` in `.env.production`. MinIO's own CORS defaults are
  permissive in dev but should not be relied on for a bucket reachable via a
  public domain — apply `deploy/s3-cors.json` here too, using `mc anonymous`
  / `mc admin` or MinIO's console.

## 7. Health vs. readiness

Two distinct probes, both unauthenticated (exempt from `GATEWAY_SECRET` and
any bearer requirement — see `internal/platform/router.go`):

- **`/health`** — "the process is up." Always 200 once the server has
  started. Use it for restart-policy / crash-loop detection.
- **`/ready`** — "the process can currently reach its database" (pings the
  pool with a 2s timeout). Use it for load-balancer / rotation decisions. A
  slow Postgres failover should look like "take this replica out of
  rotation," not "the process crashed" — conflating the two would make it
  look like the latter.

`deploy/Caddyfile` routes `API_DOMAIN/ready` straight to `app:8080`,
bypassing `bff`. **Decision:** `bff` only proxies `/health`, `/playground`,
and `/graphql` (see `apps/bff`'s router) — it has no passthrough for
arbitrary paths, so routing `/ready` through it was not an option without
adding one. Caddy talking to `app` directly for this one route is the
smaller change, and `/ready` needs no `X-Gateway-Secret` (it is exempt from
the guard, same as `/health`), so no secret handling is needed on that path.

## 8. Backups

```bash
make backup
# or directly:
BACKUP_KEEP_DAYS=14 deploy/backup.sh
```

Writes `deploy/backups/<UTC timestamp>/` containing `db.dump` (`pg_dump -Fc`),
`manifest.json` (git SHA, migration version, a total live-row count, the
dump's SHA-256, and media file count/bytes if `BACKUP_MEDIA_CMD` is set), and
`media/` if media backup is configured. `BACKUP_MEDIA_CMD` is a shell command
you provide (an `mc mirror` or `aws s3 sync` invocation, typically) — the
script does not guess at your object storage. Old backup directories older
than `BACKUP_KEEP_DAYS` are pruned after each successful run.

Schedule it — see `deploy/cron.example`. `backup.sh`/`restore.sh`/
`restore-drill.sh` all target `docker-compose.prod.yml`, whose `image:` lines
require `IMAGE_TAG` even for these scripts' postgres-only operations (compose
interpolates every service on any invocation); pin `IMAGE_TAG` in
`.env.production` for cron runs, or let the scripts fall back to the
checkout's current git sha (see §13).

## 9. Restore

```bash
RESTORE_CONFIRM=yes deploy/restore.sh deploy/backups/<timestamp>
```

Prints the backup's `manifest.json` and refuses to proceed without
`RESTORE_CONFIRM=yes` — this is destructive (`pg_restore --clean --if-exists`
drops every object the dump touches before recreating it). Targets the real
stack by default (`COMPOSE_FILE`/`ENV_FILE`/`COMPOSE_PROJECT_NAME`, same
defaults as `backup.sh`).

## 10. Restore drill

Backups nobody has restored are a hope, not a plan. `deploy/restore-drill.sh`
proves one is actually restorable, automatically:

```bash
deploy/restore-drill.sh                          # newest backup under deploy/backups/
deploy/restore-drill.sh deploy/backups/<timestamp> # a specific one
```

It spins up a **throwaway** compose project (isolated network and volumes,
never the real stack — a random-suffixed `-p` project name), restores the
backup into it, checks the restored migration version and row count against
the manifest, brings up `app` against the restored database and polls its
real `/ready` endpoint, and always tears the throwaway project down
(`down -v`) on exit — pass or fail. Prints `RESTORE DRILL: PASS ...` or
`RESTORE DRILL: FAIL — <reason>` as its last line and exits 0/1 to match.
Run it on a schedule too (see `deploy/cron.example`) — a backup that stops
being restorable and nobody notices for months is the same as no backup.

## 11. Alerting

```bash
deploy/healthwatch.sh
```

Polls `API_DOMAIN`'s `/ready`, `DELIVERY_DOMAIN`'s `/health`,
`CONSOLE_DOMAIN`'s `/`, and local disk headroom (`DISK_MIN_FREE_PCT`, default
15%). POSTs a JSON alert to `ALERT_WEBHOOK_URL` **only when the combined
state changes** (a small state file dedupes repeats), so a cron running this
every few minutes pages once when something breaks and once when it
recovers — see `deploy/cron.example` for a ready-to-copy crontab.

## 12. Makefile targets

| Target | Does |
|---|---|
| `make prod-config` | Validates `deploy/docker-compose.prod.yml` resolves against `.env.production` |
| `make prod-up` | Builds and starts the production stack, detached |
| `make prod-down` | Stops the production stack (volumes kept) |
| `make backup` | Runs `deploy/backup.sh` |
| `make restore-drill` | Runs `deploy/restore-drill.sh` against the newest backup |

## 13. Updating

```bash
git pull
make prod-up   # rebuilds changed images, re-runs migrate, restarts what changed
```

`IMAGE_TAG` has no default in `docker-compose.prod.yml` — a bare `latest`
tag drifts under you with no commit to point at, so compose refuses to start
without it (`${IMAGE_TAG:?IMAGE_TAG is required (git sha)}`). The `make
prod-*` targets default it to the current commit's short git sha
(`git rev-parse --short HEAD`) when it isn't already set, so plain `make
prod-up` still works for local/dev use; override with `make prod-up
IMAGE_TAG=v1.2.3` or an exported `IMAGE_TAG` env var.

Pin `IMAGE_TAG` (and build/push to a registry under `IMAGE_PREFIX`) instead
of building on the host if you'd rather deploy an already-built, already-
tested image — `docker-compose.prod.yml` supports both (`image:` + `build:`
on the same service; `docker compose build` tags the local build with
`IMAGE_TAG`).

## 14. Troubleshooting

- **Caddy up but a domain 404s / cert never issues** — check `docker compose
  logs caddy`; almost always DNS not pointing here yet, or port 80 blocked
  (ACME HTTP-01 needs it even though the site serves on 443).
- **`/ready` returns 503 with a real error body** — Postgres is reachable at
  the network level but refusing the connection; check `DATABASE_URL`
  credentials and `postgres`'s own health (`docker compose logs postgres`).
- **Console loads but every API call 404s** — `/content-api/*` on
  `CONSOLE_DOMAIN` did not get stripped/proxied correctly; confirm
  `deploy/Caddyfile`'s `handle_path /content-api/*` block matches what's
  actually mounted (`docker compose exec caddy cat /etc/caddy/Caddyfile`) and
  that `GATEWAY_SECRET` matches what `app` expects.
- **Console loads and looks fully functional, but every list/save is mock
  data that never reaches the backend** — the bundle was built without
  `VITE_CONTENT_API`/`VITE_SCHEMA_SOURCE` set. This is a silent degradation
  by design on the console side (no error, no banner — see its
  `.env.example` lines 10-13), so a 200 response and a working-looking UI is
  not evidence the build is wired up. Rebuild per "Build the console" in §3
  and redeploy `CONSOLE_DIST`.
- **`restore-drill.sh` fails at the table row-count check** — it compares
  exact `COUNT(*)` per table (`manifest.json`'s `tables` map) against the
  restored database, not `pg_stat_user_tables.n_live_tup` (that's a planner
  statistics estimate — it drifts from the real count between
  autovacuum/analyze runs and produced false failures before this drill
  switched to exact counts). A mismatch here is therefore a real discrepancy
  (the dump is stale, or something wrote to the target between backup and
  restore in a way the drill's isolation should have prevented — file a bug
  if so), not a flake to retry past. The FAIL line names the first
  mismatching table.
