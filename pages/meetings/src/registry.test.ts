import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SLUG } from "./fake-registry";

/** A fetch stub answering from a table of path → response bodies, recording each call. */
function stubFetch(answer: (url: string, init?: RequestInit) => { status?: number; body?: unknown }) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      const { status = 200, body = null } = answer(url, init);
      return new Response(body === null ? "" : JSON.stringify(body), { status });
    }),
  );
  return calls;
}

async function registry() {
  vi.resetModules();
  vi.stubGlobal("location", { pathname: `/a/${SLUG}/`, search: "" });
  return import("./registry");
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => vi.unstubAllGlobals());

describe("slugFrom", () => {
  it("takes the slug from /a/<slug>/ and falls back to ?slug=", async () => {
    const { slugFrom } = await registry();
    expect(slugFrom({ pathname: `/a/${SLUG}/`, search: "" })).toBe(SLUG);
    expect(slugFrom({ pathname: "/", search: "?slug=x" })).toBe("x");
    expect(slugFrom(undefined)).toBe("");
  });
});

describe("listAll", () => {
  it("follows cursor until the last page", async () => {
    const calls = stubFetch((url) => {
      const after = new URL(url, "https://pages.example").searchParams.get("after");
      if (!after) return { body: { keys: [{ key: "rec.a", version: 1 }], cursor: "rec.a" } };
      if (after === "rec.a") return { body: { keys: [{ key: "rec.b", version: 2 }], cursor: "rec.b" } };
      return { body: { keys: [{ key: "rec.c", version: 3 }], cursor: null } };
    });
    const { listAll } = await registry();
    const keys = await listAll("rec.", true);
    expect(keys.map((k) => k.key)).toEqual(["rec.a", "rec.b", "rec.c"]);
    expect(calls).toHaveLength(3);
    const first = new URL(calls[0].url, "https://pages.example");
    expect(first.pathname).toBe(`/d/${SLUG}`);
    expect(first.searchParams.get("prefix")).toBe("rec.");
    expect(first.searchParams.get("values")).toBe("1");
    expect(new URL(calls[2].url, "https://pages.example").searchParams.get("after")).toBe("rec.b");
  });

  it("throws the refusal's code", async () => {
    stubFetch(() => ({ status: 401, body: { error: "locked", code: "page_locked" } }));
    const { listAll, Refused } = await registry();
    const err = await listAll("rec.", true).catch((e) => e);
    expect(err).toBeInstanceOf(Refused);
    expect(err.code).toBe("page_locked");
  });
});

describe("writes", () => {
  it("putRec states the version and carries the cookie-auth header", async () => {
    const calls = stubFetch(() => ({ body: { key: "rec.a", version: 4 } }));
    const { putRec } = await registry();
    await putRec("rec.a", "meeting@1", { id: "a" }, 3);
    expect(calls[0].url).toBe(`/d/${SLUG}/rec.a?v=3`);
    expect(calls[0].init?.method).toBe("PUT");
    expect((calls[0].init?.headers as Record<string, string>)["x-ccc-auth"]).toBe("cookie");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ schema: "meeting@1", data: { id: "a" } });
  });
});

describe("probeRole", () => {
  it("reads 403 role_insufficient as a contributor link, anything else as a reader", async () => {
    stubFetch(() => ({ status: 403, body: { error: "no", code: "role_insufficient" } }));
    expect(await (await registry()).probeRole()).toBe("contributor");
    stubFetch(() => ({ status: 404, body: { error: "no view", code: "not_found" } }));
    expect(await (await registry()).probeRole()).toBe("reader");
  });
});
