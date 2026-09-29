import type { Segment } from "../types";

const CJK = /[　-鿿豈-﫿]/;
export const joinText = (a: string, b: string) => (!a ? b : !b ? a : CJK.test(a.slice(-1)) || CJK.test(b[0]) ? a + b : `${a} ${b}`);

export interface Turn {
  speaker: string;
  start: number;
  end: number;
  text: string;
  raw: string | null;
}

const TURN_CHARS = 160;

/** Consecutive segments of one speaker read as one turn, kept short enough
 *  that a long monologue keeps a timestamp every few lines. */
export function turnsOf(segs: Segment[]): Turn[] {
  const out: Turn[] = [];
  for (const s of segs) {
    const text = s.text.trim();
    const last = out[out.length - 1];
    if (last && last.speaker === s.speaker && s.start - last.end < 3 && last.text.length + text.length <= TURN_CHARS) {
      last.text = joinText(last.text, text);
      if (s.raw) last.raw = joinText(last.raw ?? "", s.raw.trim());
      last.end = s.end;
    } else out.push({ speaker: s.speaker, start: s.start, end: s.end, text, raw: s.raw ?? null });
  }
  return out;
}

export interface Lane {
  /** The raw speaker label ("Speaker 1" or a name). */
  label: string;
  segs: Segment[];
  talk: number;
  others?: string[];
}

const MAX_LANES = 7;

/** One lane per speaker, most talkative first; beyond seven, the rest share one lane. */
export function lanesOf(segs: Segment[]): Lane[] {
  const by = new Map<string, Segment[]>();
  for (const s of segs) {
    const k = s.speaker || "Unknown";
    if (!by.has(k)) by.set(k, []);
    by.get(k)!.push(s);
  }
  const all: Lane[] = [...by.entries()].map(([label, list]) => ({ label, segs: list, talk: list.reduce((n, s) => n + Math.max(0, s.end - s.start), 0) }));
  all.sort((a, b) => b.talk - a.talk);
  if (all.length <= MAX_LANES + 1) return all;
  const rest = all.slice(MAX_LANES);
  return [...all.slice(0, MAX_LANES), { label: `${rest.length} others`, segs: rest.flatMap((x) => x.segs), talk: rest.reduce((n, x) => n + x.talk, 0), others: rest.map((x) => x.label) }];
}

export function tickStep(dur: number): number {
  for (const s of [30, 60, 120, 300, 600, 900, 1800, 3600]) if (dur / s <= 7) return s;
  return 3600;
}

/** Split text around case-insensitive matches of q, for <mark>. */
export function splitMatches(text: string, q: string): { t: string; hit: boolean }[] {
  const needle = q.trim().toLowerCase();
  if (!needle) return [{ t: text, hit: false }];
  const out: { t: string; hit: boolean }[] = [];
  const lower = text.toLowerCase();
  let i = 0;
  for (;;) {
    const j = lower.indexOf(needle, i);
    if (j < 0) break;
    if (j > i) out.push({ t: text.slice(i, j), hit: false });
    out.push({ t: text.slice(j, j + needle.length), hit: true });
    i = j + needle.length;
  }
  if (i < text.length) out.push({ t: text.slice(i), hit: false });
  return out;
}
