# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Use GitHub's private vulnerability reporting on this repository
(**Security** tab → **Report a vulnerability**). That channel is private between
you and the maintainer.

This is a single-maintainer project, so please set expectations accordingly:
you should get an acknowledgement within about a week. If a report is valid
and I can fix it, I will; if I can't, I'll say so plainly rather than leave
you waiting. There is no bug bounty.

## Supported versions

Pre-1.0. Only the `main` branch is supported — there are no maintained release
branches and no backports. Fixes land on `main`.

## Defaults that are deliberately unsafe for local development

This is the most important section on this page. Several defaults exist to make
`docker compose up` work on a laptop with zero configuration, and **every one of
them is wrong for a real deployment**. They are documented rather than hidden:

These defaults only apply when you *declare* a development environment.
`APP_ENV` itself defaults to `production` in code, which arms the hard startup
guards below; `.env.example` and `docker-compose.yml` are what set
`APP_ENV=development` and disarm them. So the unsafe combination is reachable
only by a line someone wrote, never by a default they never saw.

| Setting | Local default | Why it is unsafe | What to set |
|---|---|---|---|
| `APP_ENV` | `development` (set in `.env.example`; the code default is `production`) | Disarms every hard startup guard, turning the two rows below from boot failures into log lines | Remove the line, or set `production` |
| `AUTHZ_MODE` | `allow` | Authorization checks pass unconditionally | `rbac` or `opa` |
| `AUTH_DEV_HEADERS` | `true` | Identity can be asserted with a plain request header — anyone reaching the API is anyone they claim to be | `false` |
| `JWT_SECRET_HEX`, `ENCRYPTION_KEY_HEX`, `BLIND_INDEX_PEPPER_HEX` | throwaway values in `docker-compose.yml` | Published in this repository, therefore public knowledge | Generate real secrets |

Under `APP_ENV=production` the first two rows are **refusals, not warnings** —
the server exits at startup rather than serving with them (TKT-R2), as it does
for the published throwaway secrets in the third row.

The application validates that secrets are the right *length*. It does not and
cannot validate that they are *secret*. A key of the right length taken from
this repository will pass every check and protect nothing.

This page says *why*. The step-by-step list for turning each of these off —
including the gateway guard, the delivery edge, agent credentials and media
storage — is [`docs/DEPLOYMENT_HARDENING.md`](docs/DEPLOYMENT_HARDENING.md).

## The domain API is not an edge service

`cmd/server` (:8080) is designed to sit behind the GraphQL BFF or another
trusted gateway that has already authenticated the caller. **Do not expose it
directly to the internet.** Its trust model assumes an authenticated upstream;
`AUTH_DEV_HEADERS` above is the sharpest example of what that assumption costs
if the assumption is false.

`apps/delivery` is the component intended to face the public — it serves
published content only.

## Scope

In scope: anything in this repository — the domain API, the GraphQL BFF, the
delivery edge, the MCP server, the authz/authn packages, the migrations, and
the OPA policies under `internal/pkg/authz/policies/`.

Out of scope: findings that consist only of the documented local-development
defaults above. They are a known, deliberate trade-off, stated here so that
nobody deploys them by accident.
