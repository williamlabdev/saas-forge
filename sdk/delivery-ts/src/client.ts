// Typed client for the content delivery edge (apps/delivery). Generated from
// apps/delivery/openapi/delivery.yaml — apps/delivery/internal/handler/openapi_test.go
// is what keeps that spec honest about the routes, query parameters and error
// codes below, so this file trusts it rather than re-deriving any of them.
import type { components } from "./generated/openapi.js";

export type Entry = components["schemas"]["Entry"];
export type EntryList = components["schemas"]["EntryList"];
export type ErrorCode = components["schemas"]["ErrorCode"];

/** A filter operator the Domain API's `key:op:value` grammar accepts. */
export type FilterOp =
  | "eq"
  | "neq"
  | "gt"
  | "gte"
  | "lt"
  | "lte"
  | "in"
  | "contains"
  | "has"
  | "nhas";

/**
 * Builds one `?filter=` clause. `in`/`has`/`nhas` take a comma-joined list of
 * values on the wire — pass an array and this joins it; every other operator
 * takes a single value, and passing an array to one is a mistake this throws
 * on rather than silently sending "a,b,c" as an equality comparison.
 */
export function filter(field: string, op: FilterOp, value: string | number | boolean | Array<string | number>): string {
  const listOps: FilterOp[] = ["in", "has", "nhas"];
  if (Array.isArray(value) && !listOps.includes(op)) {
    throw new TypeError(`filter: op ${JSON.stringify(op)} takes a single value, not an array`);
  }
  // has/nhas take a single value on the wire too (set membership of one
  // item) — only a comma-joined list is meaningful for them, same as "in".
  const raw = Array.isArray(value) ? value.join(",") : String(value);
  return `${field}:${op}:${raw}`;
}

/** Thrown for every non-2xx/304 response. Carries the edge's own error code — see ErrorCode. */
export class DeliveryError extends Error {
  readonly code: ErrorCode | string;
  readonly status: number;

  constructor(code: ErrorCode | string, status: number, message: string) {
    super(message);
    this.name = "DeliveryError";
    this.code = code;
    this.status = status;
  }
}

/** The result of a request that supports revalidation: `data` is `null` exactly when `notModified` is true. */
export interface DeliveryResult<T> {
  data: T | null;
  notModified: boolean;
  etag?: string;
  cacheControl?: string;
}

export interface DeliveryClientOptions {
  /** Origin the edge is served from, e.g. `https://cdn.example.com`. No trailing slash needed. */
  baseUrl: string;
  /** Tenant slug — the first path segment on every route but /health and /v1/openapi.*. */
  tenant: string;
  /** Override for testing, or to supply a fetch polyfill. Defaults to the global `fetch`. */
  fetch?: typeof fetch;
}

export interface ListEntriesOptions {
  locale?: string;
  /** Opaque token from a previous page's `next_cursor`. This API pages by cursor only — there is no offset. */
  cursor?: string;
  limit?: number;
  sort?: string;
  /** One or more `key:op:value` clauses — see the `filter()` helper. */
  filter?: string | string[];
  /** Field projection on the parent entry only; does not reach into `populate`d relations. */
  fields?: string | string[];
  /** Dotted relation paths, up to 3 segments deep (e.g. `"author.avatar"`). */
  populate?: string | string[];
  /** Free-text search over the published snapshot. */
  q?: string;
  /** A previous response's `ETag`, to revalidate instead of re-fetching the body. */
  ifNoneMatch?: string;
}

export interface GetEntryOptions {
  /** Dotted relation paths, up to 3 segments deep. Refused together with `previewToken`. */
  populate?: string | string[];
  /** Scopes this one request to that entry's unpublished working copy. Never cached — no ETag comes back. */
  previewToken?: string;
  ifNoneMatch?: string;
}

export interface DeliveryClient {
  listEntries(type: string, options?: ListEntriesOptions): Promise<DeliveryResult<EntryList>>;
  getEntry(type: string, id: string, options?: GetEntryOptions): Promise<DeliveryResult<Entry>>;
  /** Builds the media redirect URL — does not fetch it. Following it is what resolves the signed URL. */
  mediaUrl(id: string, preset?: string): string;
}

/** Creates a client bound to one tenant on one edge origin. Holds no credential — every route here is public. */
export function createDeliveryClient(options: DeliveryClientOptions): DeliveryClient {
  const fetchImpl = options.fetch ?? globalThis.fetch;
  if (!fetchImpl) {
    throw new Error("createDeliveryClient: no fetch available — pass options.fetch on a runtime without a global one");
  }
  const base = options.baseUrl.replace(/\/+$/, "");
  const tenant = encodeURIComponent(options.tenant);

  function appendQuery(url: URL, name: string, value: string | number | string[] | undefined): void {
    if (value === undefined) return;
    if (Array.isArray(value)) {
      for (const v of value) url.searchParams.append(name, v);
      return;
    }
    url.searchParams.append(name, String(value));
  }

  async function get<T>(url: URL, ifNoneMatch: string | undefined): Promise<DeliveryResult<T>> {
    const headers: Record<string, string> = {};
    if (ifNoneMatch) headers["If-None-Match"] = ifNoneMatch;

    const res = await fetchImpl(url.toString(), { headers });
    const etag = res.headers.get("ETag") ?? undefined;
    const cacheControl = res.headers.get("Cache-Control") ?? undefined;

    if (res.status === 304) {
      return { data: null, notModified: true, etag, cacheControl };
    }
    if (!res.ok) {
      throw await toDeliveryError(res);
    }
    const data = (await res.json()) as T;
    return { data, notModified: false, etag, cacheControl };
  }

  return {
    async listEntries(type, opts = {}) {
      const url = new URL(`${base}/v1/${tenant}/${encodeURIComponent(type)}/`);
      appendQuery(url, "locale", opts.locale);
      appendQuery(url, "cursor", opts.cursor);
      appendQuery(url, "limit", opts.limit);
      appendQuery(url, "sort", opts.sort);
      appendQuery(url, "filter", opts.filter === undefined ? undefined : ([] as string[]).concat(opts.filter));
      appendQuery(url, "fields", opts.fields === undefined ? undefined : ([] as string[]).concat(opts.fields));
      appendQuery(url, "populate", opts.populate === undefined ? undefined : ([] as string[]).concat(opts.populate));
      appendQuery(url, "q", opts.q);
      return get<EntryList>(url, opts.ifNoneMatch);
    },

    async getEntry(type, id, opts = {}) {
      const url = new URL(`${base}/v1/${tenant}/${encodeURIComponent(type)}/${encodeURIComponent(id)}`);
      appendQuery(url, "populate", opts.populate === undefined ? undefined : ([] as string[]).concat(opts.populate));
      appendQuery(url, "preview_token", opts.previewToken);
      return get<Entry>(url, opts.ifNoneMatch);
    },

    mediaUrl(id, preset) {
      const url = new URL(`${base}/v1/${tenant}/media/${encodeURIComponent(id)}`);
      if (preset) url.searchParams.set("preset", preset);
      return url.toString();
    },
  };
}

async function toDeliveryError(res: Response): Promise<DeliveryError> {
  let code: ErrorCode | string = "UNKNOWN_ERROR";
  let message = res.statusText || `request failed with status ${res.status}`;
  try {
    const body = (await res.json()) as { error?: { code?: string; message?: string } };
    if (body?.error?.code) code = body.error.code as ErrorCode;
    if (body?.error?.message) message = body.error.message;
  } catch {
    // Body was not JSON (or empty) — the generic message above stands.
  }
  return new DeliveryError(code, res.status, message);
}
