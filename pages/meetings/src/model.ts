// Derived views over records: labels by kind, people, search, sort, the gist.
import type { Rec } from "./types";
import type { Row } from "./store";
import { parseTime } from "./time";

// The closed list is the `labels` record's `type` entries: the meeting types.
// The label editor never adds to it; every other label is on the open list.
let CLOSED: string[] = [];
export function setClosedList(list: string[]) {
  CLOSED = list;
}
export const closedList = () => CLOSED;

export type LabelKind = "type" | "person" | "topic";
export function labelKind(l: string): LabelKind {
  if (l.startsWith("人:")) return "person";
  return CLOSED.includes(l) ? "type" : "topic";
}

/** The meeting type(s): the record's closed-list labels, in list order. */
export function typesOf(rec: Rec): string[] {
  const ls = rec.labels ?? [];
  return CLOSED.filter((c) => ls.includes(c));
}
/** A label change, applied to the labels read fresh from the registry. */
export type LabelChange = (cur: string[]) => string[];
/** Labels trimmed and de-duplicated, order kept. */
export function normLabels(xs: unknown): string[] {
  const out: string[] = [];
  for (const x of Array.isArray(xs) ? xs : []) {
    const t = typeof x === "string" ? x.trim() : "";
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}
/** Add a label; a meeting type replaces the type the record has (one type per record). */
export const addLabel =
  (l: string): LabelChange =>
  (cur) => [...(CLOSED.includes(l) ? cur.filter((x) => !CLOSED.includes(x)) : cur), l];
export const removeLabel =
  (l: string): LabelChange =>
  (cur) => cur.filter((x) => x !== l);

/** Open-list labels: topics and projects. */
export const topicsOf = (rec: Rec): string[] => (rec.labels ?? []).filter((l) => labelKind(l) !== "type");
export const labelText = (l: string) => (l.startsWith("人:") ? l.slice(2) : l);

/** Named speakers only — "Speaker 3" is the diariser's placeholder. */
export function namedSpeakers(rec: Rec): string[] {
  return (rec.speakers ?? []).map((s) => s.name).filter((n) => n && !/^Speaker \d+$/i.test(n));
}

export const displayTitle = (rec: Rec) => rec.display_title?.trim() || rec.title?.trim() || rec.id;

export function summaryGist(rec: Rec): { text: string; kind: "gist" | "pending" | "empty" } {
  if (rec.summary === undefined || rec.summary === null) return { text: "Summary pending", kind: "pending" };
  const first = rec.summary
    .split("\n")
    .map((l) => l.replace(/^[\s>#*-]+/, "").replace(/\*\*/g, "").trim())
    .find(Boolean);
  return first ? { text: first, kind: "gist" } : { text: "No summary", kind: "empty" };
}

export const startedOf = (rec: Rec) => parseTime(rec.started);

export function haystack(rec: Rec): string {
  return [rec.display_title, rec.title, rec.summary, ...(rec.speakers ?? []).map((s) => s.name), ...(rec.labels ?? [])].filter(Boolean).join("\n").toLowerCase();
}

export type SortKey = "date" | "duration" | "status" | "title";
const STATE_ORDER: Record<string, number> = { failed: 0, uploading: 1, uploaded: 2, processing: 3, aligned: 4, ingested: 5, deleted: 6 };

export function sortRows(rows: Row[], key: SortKey, dir: "asc" | "desc"): Row[] {
  const v = (r: Row): number | string => {
    switch (key) {
      case "date":
        return startedOf(r.rec)?.getTime() ?? 0;
      case "duration":
        return r.rec.duration_s ?? 0;
      case "status":
        return STATE_ORDER[r.rec.state] ?? 9;
      case "title":
        return displayTitle(r.rec);
    }
  };
  const sign = dir === "asc" ? 1 : -1;
  return [...rows].sort((a, b) => {
    const x = v(a), y = v(b);
    const c = typeof x === "string" ? x.localeCompare(y as string, "zh-Hans-CN") : x - (y as number);
    return c ? c * sign : (startedOf(b.rec)?.getTime() ?? 0) - (startedOf(a.rec)?.getTime() ?? 0);
  });
}

/** Stable speaker hue index within one meeting, by first appearance. */
export function speakerPalette(names: string[]): Map<string, number> {
  const m = new Map<string, number>();
  for (const n of names) if (!m.has(n)) m.set(n, m.size % 8);
  return m;
}
