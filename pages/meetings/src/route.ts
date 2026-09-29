// Hash routing: the host serves files only (no SPA fallback), and the hash
// keeps every view — filters included — shareable.
import { useSyncExternalStore } from "react";

export interface Route {
  path: string[];
  query: URLSearchParams;
}

function parse(hash: string): Route {
  const h = hash.replace(/^#/, "") || "/";
  const [p, q = ""] = h.split("?");
  return { path: p.split("/").filter(Boolean).map(decodeURIComponent), query: new URLSearchParams(q) };
}

let current = location.hash;
const subscribe = (l: () => void) => {
  const on = () => {
    current = location.hash;
    l();
  };
  window.addEventListener("hashchange", on);
  return () => window.removeEventListener("hashchange", on);
};

export function useRoute(): Route {
  const hash = useSyncExternalStore(subscribe, () => current);
  return parse(hash);
}

export function href(path: string[], query?: URLSearchParams | Record<string, string | string[] | undefined>): string {
  const p = `/${path.map(encodeURIComponent).join("/")}`;
  let q: URLSearchParams;
  if (query instanceof URLSearchParams) q = query;
  else {
    q = new URLSearchParams();
    for (const [k, v] of Object.entries(query ?? {})) {
      if (Array.isArray(v)) for (const x of v) q.append(k, x);
      else if (v) q.set(k, v);
    }
  }
  const s = q.toString();
  return `#${p}${s ? `?${s}` : ""}`;
}

export function navigate(to: string, replace = false) {
  if (replace) {
    history.replaceState(null, "", to);
    current = location.hash;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  } else location.hash = to;
}

/** The list's own query keys; a meeting's view keys (tab, ts) ride beside them. */
export const LIST_KEYS = ["q", "s", "l", "p", "src", "sort", "dir"];
export function listParams(query: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams();
  for (const [k, v] of query) if (LIST_KEYS.includes(k)) out.append(k, v);
  return out;
}

/** Open a meeting keeping the index's filters and the tab being read. */
export function meetingHref(id: string, current: URLSearchParams, extra?: Record<string, string>): string {
  const q = listParams(current);
  const tab = current.get("tab");
  if (tab) q.set("tab", tab);
  for (const [k, v] of Object.entries(extra ?? {})) q.set(k, v);
  return href(["m", id], q);
}

/** The list with the filters currently in force. */
export const listHref = (current: URLSearchParams) => href([], listParams(current));

/** The last list view, so "All meetings" returns to the same filters. */
let lastList = "#/";
export const rememberList = (h: string) => {
  lastList = h;
};
export const lastListHref = () => lastList;
