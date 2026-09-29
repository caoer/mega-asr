// The page's registry: /d/<slug> records and /f/<slug> files on the pages
// host. Every request is same-origin, so the grant cookie rides along; a write
// also carries `x-ccc-auth: cookie`. Every refusal is {error, code}.

/** The slug is the second segment of /a/<slug>[/…]; `?slug=` serves local runs. */
export function slugFrom(loc: { pathname: string; search: string } | undefined): string {
  if (!loc) return "";
  const seg = loc.pathname.split("/").filter(Boolean);
  return (seg[0] === "a" && seg[1]) || new URLSearchParams(loc.search).get("slug") || "";
}
export const SLUG = slugFrom(globalThis.location);

export class Refused extends Error {
  status: number;
  code: string;
  body: Record<string, unknown>;
  constructor(status: number, body: { error?: string; code?: string } | null) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || String(status);
    this.body = (body as Record<string, unknown>) ?? {};
  }
}

export async function call<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== "GET" && method !== "HEAD") headers["x-ccc-auth"] = "cookie";
  const init: RequestInit = { method, credentials: "same-origin", headers };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    headers["content-type"] = "application/json";
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let parsed: unknown = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) throw new Refused(res.status, parsed as { error?: string; code?: string } | null);
  return parsed as T;
}

/** One stored record as /d/ serves it. */
export interface Entry<T = unknown> {
  key: string;
  version: number;
  updated_at?: number;
  value?: { schema: string; data: T };
}

const dPath = (key: string) => `/d/${encodeURIComponent(SLUG)}/${encodeURIComponent(key)}`;

/** Every key under `prefix`, following `cursor` (a values page holds at most 100). */
export async function listAll<T = unknown>(prefix: string, values: boolean): Promise<Entry<T>[]> {
  const out: Entry<T>[] = [];
  let after: string | null = null;
  for (let i = 0; i < 200; i++) {
    const q = new URLSearchParams({ prefix });
    if (values) q.set("values", "1");
    if (after) q.set("after", after);
    const page = await call<{ keys?: Entry<T>[]; cursor?: string | null }>("GET", `/d/${encodeURIComponent(SLUG)}?${q}`);
    out.push(...(page.keys ?? []));
    if (!page.cursor) return out;
    after = page.cursor;
  }
  return out;
}

export const getRec = <T = unknown>(key: string) => call<Entry<T>>("GET", dPath(key));
/** Compare-and-swap: `v` is the version read (0 creates). */
export const putRec = (key: string, schema: string, data: unknown, v: number) => call<Entry>("PUT", `${dPath(key)}?v=${v}`, { schema, data });
export const delRec = (key: string) => call("DELETE", dPath(key));
export const fileURL = (id: string) => `/f/${encodeURIComponent(SLUG)}/${encodeURIComponent(id)}`;

/**
 * Ring the page's inbox (/i/<slug>): a grant may post only `human.message`.
 * The delivery id dedupes a retry of the same ring.
 */
export async function ring(delivery: string, body: { text: string; by: string; channel: "page" }): Promise<void> {
  const res = await fetch(`/i/${encodeURIComponent(SLUG)}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "x-ccc-auth": "cookie", "x-inbox-event": "human.message", "x-inbox-delivery": delivery, "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let parsed: { error?: string; code?: string } | null = null;
    try {
      parsed = JSON.parse(await res.text());
    } catch {
      parsed = null;
    }
    throw new Refused(res.status, parsed);
  }
}

/**
 * Which link opened the page. ccc-pages refuses a contributor every `~` view
 * with 403 role_insufficient; any other reader gets 404 for an unknown view.
 */
export async function probeRole(): Promise<"contributor" | "reader"> {
  try {
    await call("GET", dPath("~meetings-role"));
  } catch (e) {
    if (e instanceof Refused && e.status === 403 && e.code === "role_insufficient") return "contributor";
  }
  return "reader";
}

/**
 * Whether this link may read a file. A contributor reads only the files its own
 * grant uploaded, and any other answers 404 (or 403) exactly like a missing one.
 */
const readable = new Map<string, Promise<boolean>>();
export function canRead(id: string): Promise<boolean> {
  let p = readable.get(id);
  if (!p) {
    p = fetch(fileURL(id), { method: "HEAD", credentials: "same-origin" }).then(
      (res) => !(res.status === 404 || res.status === 403),
      () => true,
    );
    readable.set(id, p);
  }
  return p;
}

/** Reader-facing text for a failed call. */
export function explain(e: unknown): string {
  if (!(e instanceof Refused)) {
    const msg = e instanceof Error ? e.message : String(e);
    return e instanceof TypeError ? `Network error: ${msg}. Check the connection and try again.` : msg;
  }
  switch (e.code) {
    case "page_locked":
    case "token_invalid":
      return "This browser has no access to the page. Open your share link again.";
    case "role_insufficient":
      return "Your link can upload recordings but cannot change or delete them. Use an editor link for that.";
    case "not_creator":
      return "Your link can only change records it created itself.";
    case "too_many_writes":
      return "Too many writes in the last minute. Wait a minute and try again.";
    case "too_large":
      return "The server refused the file as too large.";
    case "csrf_header_required":
      return "The server refused the write (missing auth header).";
    default:
      return `${e.message} (${e.code})`;
  }
}
