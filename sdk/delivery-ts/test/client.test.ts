import { describe, expect, it, vi } from "vitest";
import { createDeliveryClient, DeliveryError, filter } from "../src/client.js";
import type { Entry, EntryList } from "../src/client.js";

function jsonResponse(body: unknown, init: ResponseInit & { headers?: Record<string, string> } = {}): Response {
  return new Response(JSON.stringify(body), {
    status: init.status ?? 200,
    headers: { "Content-Type": "application/json", ...(init.headers ?? {}) },
  });
}

function fakeFetch(handler: (url: URL, init: RequestInit | undefined) => Response): typeof fetch {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === "string" ? input : input.toString());
    return handler(url, init);
  }) as typeof fetch;
}

const entry: Entry = {
  id: "11111111-1111-1111-1111-111111111111",
  type: "post",
  data: { title: "hello" },
  version: 1,
  status: "published",
  locale: "en",
  translation_group_id: "22222222-2222-2222-2222-222222222222",
  created_at: "2026-01-01T00:00:00Z",
};

const emptyList: EntryList = { items: [], limit: 20 };

describe("createDeliveryClient — URL building", () => {
  it("builds the list URL with tenant and type, trailing slash", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(emptyList);
      }),
    });

    await client.listEntries("post");

    expect(seen?.origin + seen?.pathname).toBe("https://cdn.example.com/v1/acme/post/");
  });

  it("builds the get-entry URL with the id segment", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(entry);
      }),
    });

    await client.getEntry("post", entry.id);

    expect(seen?.pathname).toBe(`/v1/acme/post/${entry.id}`);
  });

  it("forwards limit, locale, cursor, sort and q", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(emptyList);
      }),
    });

    await client.listEntries("post", { limit: 10, locale: "en", cursor: "abc", sort: "-created_at", q: "hello world" });

    expect(seen?.searchParams.get("limit")).toBe("10");
    expect(seen?.searchParams.get("locale")).toBe("en");
    expect(seen?.searchParams.get("cursor")).toBe("abc");
    expect(seen?.searchParams.get("sort")).toBe("-created_at");
    expect(seen?.searchParams.get("q")).toBe("hello world");
  });

  it("repeats filter/fields/populate as separate query parameters, not comma-joined", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(emptyList);
      }),
    });

    await client.listEntries("post", {
      filter: [filter("status", "eq", "live"), filter("tags", "has", "ai")],
      fields: ["title", "slug"],
      populate: ["author.avatar"],
    });

    expect(seen?.searchParams.getAll("filter")).toEqual(["status:eq:live", "tags:has:ai"]);
    expect(seen?.searchParams.getAll("fields")).toEqual(["title", "slug"]);
    expect(seen?.searchParams.getAll("populate")).toEqual(["author.avatar"]);
  });

  it("accepts a single string for filter/fields/populate without wrapping it in an array", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(emptyList);
      }),
    });

    await client.listEntries("post", { populate: "author.avatar" });

    expect(seen?.searchParams.getAll("populate")).toEqual(["author.avatar"]);
  });

  it("URL-encodes tenant, type, id and query values", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme & co",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(emptyList);
      }),
    });

    await client.listEntries("blog post", { q: "a/b c&d" });

    expect(seen?.pathname).toBe("/v1/acme%20%26%20co/blog%20post/");
    expect(seen?.searchParams.get("q")).toBe("a/b c&d");
  });

  it("builds a media URL without fetching, optionally with a preset", () => {
    const client = createDeliveryClient({ baseUrl: "https://cdn.example.com", tenant: "acme", fetch: fakeFetch(() => jsonResponse({})) });

    expect(client.mediaUrl("m1")).toBe("https://cdn.example.com/v1/acme/media/m1");
    expect(client.mediaUrl("m1", "thumb")).toBe("https://cdn.example.com/v1/acme/media/m1?preset=thumb");
  });
});

describe("createDeliveryClient — dotted populate", () => {
  it("forwards a depth-3 dotted populate path unchanged on getEntry", async () => {
    let seen: URL | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((url) => {
        seen = url;
        return jsonResponse(entry);
      }),
    });

    await client.getEntry("post", entry.id, { populate: ["author.mentor.avatar"] });

    expect(seen?.searchParams.getAll("populate")).toEqual(["author.mentor.avatar"]);
  });
});

describe("createDeliveryClient — ETag / 304", () => {
  it("sends If-None-Match and reports notModified with data null on 304", async () => {
    let sentHeaders: Headers | undefined;
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch((_url, init) => {
        sentHeaders = new Headers(init?.headers);
        return new Response(null, { status: 304, headers: { ETag: '"abc123"', "Cache-Control": "public, no-cache" } });
      }),
    });

    const result = await client.getEntry("post", entry.id, { ifNoneMatch: '"abc123"' });

    expect(sentHeaders?.get("If-None-Match")).toBe('"abc123"');
    expect(result.notModified).toBe(true);
    expect(result.data).toBeNull();
    expect(result.etag).toBe('"abc123"');
    expect(result.cacheControl).toBe("public, no-cache");
  });

  it("returns the etag alongside data on a fresh 200", async () => {
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch(() => jsonResponse(entry, { headers: { ETag: '"xyz"' } })),
    });

    const result = await client.getEntry("post", entry.id);

    expect(result.notModified).toBe(false);
    expect(result.data).toEqual(entry);
    expect(result.etag).toBe('"xyz"');
  });
});

describe("createDeliveryClient — error envelope", () => {
  it("throws a DeliveryError carrying the edge's code, status and message", async () => {
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch(() =>
        jsonResponse({ error: { code: "NOT_FOUND", message: "not found" } }, { status: 404 }),
      ),
    });

    await expect(client.getEntry("post", "missing")).rejects.toMatchObject({
      name: "DeliveryError",
      code: "NOT_FOUND",
      status: 404,
      message: "not found",
    });
  });

  it("still throws a DeliveryError when the error body is not JSON", async () => {
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch(() => new Response("gateway timeout", { status: 502, statusText: "Bad Gateway" })),
    });

    const err = await client.listEntries("post").catch((e) => e);
    expect(err).toBeInstanceOf(DeliveryError);
    expect((err as DeliveryError).status).toBe(502);
  });

  it("rejects on a rate-limited response with RATE_LIMITED", async () => {
    const client = createDeliveryClient({
      baseUrl: "https://cdn.example.com",
      tenant: "acme",
      fetch: fakeFetch(() =>
        jsonResponse({ error: { code: "RATE_LIMITED", message: "too many requests" } }, { status: 429, headers: { "Retry-After": "60" } }),
      ),
    });

    await expect(client.listEntries("post")).rejects.toMatchObject({ code: "RATE_LIMITED", status: 429 });
  });
});

describe("filter()", () => {
  it("builds key:op:value", () => {
    expect(filter("status", "eq", "live")).toBe("status:eq:live");
    expect(filter("price", "gte", 100)).toBe("price:gte:100");
  });

  it("comma-joins an array for in/has/nhas", () => {
    expect(filter("tags", "in", ["a", "b"])).toBe("tags:in:a,b");
    expect(filter("tags", "has", ["ai"])).toBe("tags:has:ai");
  });

  it("rejects an array for a non-list operator", () => {
    expect(() => filter("price", "gt", ["1", "2"] as unknown as string)).toThrow(TypeError);
  });
});

describe("createDeliveryClient — no global fetch", () => {
  it("throws immediately rather than failing later on the first request", () => {
    const originalFetch = globalThis.fetch;
    // @ts-expect-error deliberately clearing the global to exercise the guard
    globalThis.fetch = undefined;
    try {
      expect(() => createDeliveryClient({ baseUrl: "https://cdn.example.com", tenant: "acme" })).toThrow(/no fetch available/);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});

// vi is imported for its types even though every test above supplies its own
// fake fetch — kept as a named import (not `* as vi`) so an unused-import
// lint rule would catch it if this stopped being true.
void vi;
