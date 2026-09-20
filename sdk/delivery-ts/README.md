# @saas-platform/delivery-client

A typed TypeScript client for the platform's public content delivery API
(`apps/delivery`). ESM only, zero runtime dependencies — the generated types
and the ~200-line client in `src/client.ts` are the whole package.

The client is generated against, and tested for honesty against,
[`apps/delivery/openapi/delivery.yaml`](../../apps/delivery/openapi/delivery.yaml).
That spec itself is kept honest by
[`apps/delivery/internal/handler/openapi_test.go`](../../apps/delivery/internal/handler/openapi_test.go),
which fails the Go build if a route, query parameter or error code drifts
from what the edge actually serves — so this client's types are only ever as
stale as the last `pnpm gen`.

## Install

This package is not published; consume it as a workspace/path dependency
from within this repo, or copy `src/` into your own project.

## Usage

```ts
import { createDeliveryClient, filter, DeliveryError } from "@saas-platform/delivery-client";

const client = createDeliveryClient({
  baseUrl: "https://cdn.example.com",
  tenant: "acme",
});

// List, with a dotted populate path and a filter clause.
const page = await client.listEntries("post", {
  limit: 20,
  populate: ["author.avatar"],
  filter: [filter("status", "eq", "live")],
});
console.log(page.data?.items);

// Revalidate a page you already have an ETag for.
const revalidated = await client.listEntries("post", { ifNoneMatch: page.etag });
if (revalidated.notModified) {
  // page.data is still current — nothing to do.
}

// Fetch one entry.
try {
  const { data: entry } = await client.getEntry("post", "3fa85f64-...");
} catch (err) {
  if (err instanceof DeliveryError) {
    console.error(err.code, err.status, err.message);
  }
}

// Preview an unpublished working copy (never cached, no ETag).
await client.getEntry("post", "3fa85f64-...", { previewToken: "..." });

// Build a media URL (does not fetch — following it resolves a signed URL).
const url = client.mediaUrl("media-id", "thumb");
```

## API surface

- `createDeliveryClient(options)` — `{ baseUrl, tenant, fetch? }`. `fetch`
  defaults to the global `fetch`; pass a polyfill on a runtime without one.
- `client.listEntries(type, options?)` — pages by cursor only, per the edge's
  own rule (there is no offset). Returns `{ data, notModified, etag,
  cacheControl }`; `data` is `null` exactly when `notModified` is `true`.
- `client.getEntry(type, id, options?)` — same result shape. `previewToken`
  addresses that one entry's unpublished working copy and is never cached.
- `client.mediaUrl(id, preset?)` — builds the redirect URL; does not fetch it.
- `filter(field, op, value)` — builds one `key:op:value` clause for
  `listEntries`'s `filter` option. `op` is one of `eq | neq | gt | gte | lt |
  lte | in | contains | has | nhas`; `in`/`has`/`nhas` take an array and
  comma-join it, every other operator takes a single value.
- `DeliveryError` — thrown for every non-2xx/304 response; carries `code`
  (the edge's `ErrorCode`), `status`, and `message`.
- Types: `Entry`, `EntryList`, `ErrorCode`, `DeliveryResult<T>`,
  `ListEntriesOptions`, `GetEntryOptions`, `DeliveryClient`,
  `DeliveryClientOptions`, `FilterOp` — all re-exported from `src/index.ts`.

`populate` and `filter`/`fields` accept either a single string or an array —
an array becomes repeated query parameters (`?populate=a&populate=b`), never
a comma-joined value, because that is the grammar the edge and the Domain API
both expect.

## Scripts

```sh
pnpm install
pnpm gen         # regenerate src/generated/openapi.d.ts from ../../apps/delivery/openapi/delivery.yaml
pnpm build       # tsc -> dist/
pnpm typecheck   # tsc --noEmit
pnpm test        # vitest run
```

From the repo root, `make sdk-ts-gen` and `make sdk-ts-check` wrap `pnpm gen`
and `pnpm typecheck && pnpm test` respectively.

`src/generated/openapi.d.ts` is committed (not gitignored): a consumer that
only runs `pnpm install` — no Go toolchain, no access to the spec file's
build step — still gets working types. Re-run `pnpm gen` and commit the
result whenever `apps/delivery/openapi/delivery.yaml` changes; the honesty
test on the Go side (see above) is what tells you when that's needed.

## What this deliberately does not do

- No retries, no request de-duplication, no in-memory cache — this is a thin
  typed wrapper over `fetch`, not a data-fetching framework. Layer that on
  top (or use one of the many that already exist) if you need it.
- No auth: every route this client calls is public by design (ADR-004). There
  is no credential to configure.
- `?fields=` narrows the parent entry only — it does not reach into
  `populate`d relations, matching the edge's own grammar. See
  `docs/handbook/08-api.html`.
