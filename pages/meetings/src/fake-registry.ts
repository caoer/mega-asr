// Tests only: an in-memory /d/ + /f/ registry behind a stubbed fetch (and
// XMLHttpRequest for uploads), with ccc-pages' compare-and-swap rules.
import { vi } from "vitest";

export const SLUG = "joinery-4f2e";

interface Stored {
  version: number;
  value: { schema: string; data: Record<string, unknown> };
}

export interface Fake {
  store: Map<string, Stored>;
  /** Every write, in order: "PUT rec.x v=1", "DELETE q.x", "DELETE f/id", "POST f". */
  writes: string[];
  /** Answer the next upload POST with a network error. */
  failPost: boolean;
  data(key: string): Record<string, unknown> | undefined;
}

const json = (status: number, body: unknown) => new Response(body === null ? "" : JSON.stringify(body), { status });
const refuse = (status: number, code: string) => json(status, { error: code, code });

export function fakeRegistry(seed: Record<string, Record<string, unknown>> = {}): Fake {
  const store = new Map<string, Stored>();
  for (const [key, data] of Object.entries(seed)) store.set(key, { version: 1, value: { schema: key.startsWith("rec.") ? "meeting@1" : "x@1", data } });
  const fake: Fake = { store, writes: [], failPost: false, data: (k) => store.get(k)?.value.data };
  let fileSeq = 0;

  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const u = new URL(url, "https://pages.example");
      const method = init?.method ?? "GET";
      const [area, , rawKey] = u.pathname.split("/").filter(Boolean);
      const key = rawKey ? decodeURIComponent(rawKey) : "";
      if (area === "f") {
        const s = store.get(`f.${key}`);
        if (!s) return refuse(404, "not_found");
        if (method === "DELETE") {
          store.delete(`f.${key}`);
          fake.writes.push(`DELETE f/${key}`);
        }
        return json(200, {});
      }
      if (!key) {
        const prefix = u.searchParams.get("prefix") ?? "";
        const keys = [...store.entries()].filter(([k]) => k.startsWith(prefix)).map(([k, s]) => ({ key: k, version: s.version, value: s.value }));
        return json(200, { keys, cursor: null });
      }
      const cur = store.get(key);
      if (method === "GET") return cur ? json(200, { key, ...cur }) : refuse(404, "not_found");
      if (method === "DELETE") {
        if (!cur) return refuse(404, "not_found");
        store.delete(key);
        fake.writes.push(`DELETE ${key}`);
        return json(200, {});
      }
      const v = Number(u.searchParams.get("v"));
      if ((cur?.version ?? 0) !== v) return refuse(409, "version_conflict");
      const body = JSON.parse(String(init?.body)) as Stored["value"];
      const next = { version: v + 1, value: body };
      store.set(key, next);
      fake.writes.push(`PUT ${key} v=${v}`);
      return json(200, { key, ...next });
    }),
  );

  class FakeXHR {
    status = 0;
    responseText = "";
    upload: { onprogress: ((e: { lengthComputable: boolean; loaded: number; total: number }) => void) | null } = { onprogress: null };
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    withCredentials = false;
    private url = "";
    open(_m: string, url: string) {
      this.url = url;
    }
    setRequestHeader() {}
    send(file: Blob) {
      queueMicrotask(async () => {
        if (fake.failPost) return this.onerror?.();
        const meta = JSON.parse(new URL(this.url, "https://pages.example").searchParams.get("meta") ?? "{}");
        const id = `file${++fileSeq}`;
        const sha256 = [...new Uint8Array(await crypto.subtle.digest("SHA-256", await file.arrayBuffer()))].map((b) => b.toString(16).padStart(2, "0")).join("");
        store.set(`f.${id}`, { version: 1, value: { schema: "file@1", data: { meta } } });
        fake.writes.push("POST f");
        this.upload.onprogress?.({ lengthComputable: true, loaded: file.size, total: file.size });
        this.status = 201;
        this.responseText = JSON.stringify({ id, file: { sha256 } });
        this.onload?.();
      });
    }
  }
  vi.stubGlobal("XMLHttpRequest", FakeXHR);
  return fake;
}

/** Fresh modules on the fake's slug, the store loaded once. */
export async function loaded() {
  vi.resetModules();
  vi.stubGlobal("location", { pathname: `/a/${SLUG}/`, search: "" });
  vi.stubGlobal("document", { hidden: false, addEventListener() {} });
  const store = await import("./store");
  await store.load();
  return store;
}
