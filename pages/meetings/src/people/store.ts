// Speakers and people. A person is a page in a wiki; the `people.index`
// record lists those pages, and the page works without it. Nothing about a
// person is stored here: the page reads `rec.speakers` and the index, and
// naming writes `rec.speakers` alone.
//
// A speaker is keyed by its transcript label: `role` once named, else `name`
// (an entry never named carries only `name`, which is the label). Naming
// `{name:"Speaker 1"}` writes `{role:"Speaker 1", name:<the name>, person?}` —
// `person` only on an exact name/alias match in the index. Any other name
// waits for the drain, which finds or creates the wiki page and sets
// `person`.
import { useMemo } from "react";
import { getState as mainState, load, useStore, type Row } from "../store";
import { getRec, putRec, Refused, type Entry } from "../registry";
import type { PageConfig, PeopleIndexRecord, PersonPage, Rec, Speaker } from "../types";
import { wikiLink } from "../wiki";
import { startedOf } from "../model";

/**
 * The drain resolves named speakers (its people-resolve trigger): a named
 * entry without `person` reads *resolving* until the drain writes `person`.
 */
export const RESOLVER_LIVE = true;

export type WikiPerson = {
  /** The primary page's slug; the person's route. */
  slug: string;
  name: string;
  aliases: string[];
  /** The wiki page's address, when the index gives one. */
  wiki_url: string;
  wiki?: string;
  /** Every wiki page of this person, primary first. */
  pages: PersonPage[];
  /** One line from the primary page (≤120 chars). */
  summary?: string;
  /** Known only from `person` on a record: the index does not list it (yet). */
  unindexed?: boolean;
  role?: string;
  org?: string;
  description?: string;
};
export type SpeakerRef = { rec: string; label: string };
export type SpeakerState = "unnamed" | "named" | "resolving" | "linked";

export const isPlaceholder = (label: string) => /^Speaker \d+$/i.test(label.trim());
/** "Alice Example（QA）" → "Alice Example": a trailing gloss in half- or full-width parentheses dropped. */
export const shortName = (p: WikiPerson) => p.name.replace(/\s*[(（].*[)）]\s*$/, "").trim() || p.name;
/** The transcript label an entry answers to. */
export const labelOf = (s: Speaker) => s.role ?? s.name;

// ---------------------------------------------------------------------------
// The index.

type IndexEntry = PeopleIndexRecord["people"][number] & { role?: string | null; org?: string | null; description?: string | null };

/** A page's web address: an absolute `page` as is, else its wiki's template in the page config; "" when unknown. */
export function pageURL(pg: { wiki?: string; page?: string }): string {
  if (typeof pg.page !== "string" || !pg.page) return "";
  return wikiLink(mainState().config, pg.wiki, pg.page);
}

const isPage = (x: unknown): x is PersonPage => !!x && typeof (x as PersonPage).slug === "string" && typeof (x as PersonPage).page === "string";

export function indexPeople(idx: PeopleIndexRecord | null): WikiPerson[] {
  return ((idx?.people ?? []) as IndexEntry[])
    .filter((p) => p && typeof p.slug === "string" && typeof p.name === "string")
    .map((p) => {
      const primary: PersonPage = { wiki: p.wiki ?? "", page: p.page ?? "", slug: p.slug };
      const listed = Array.isArray(p.pages) ? p.pages.filter(isPage) : [];
      // Primary first; an index without `pages` has the primary alone.
      const pages = [primary, ...listed.filter((x) => x.slug !== p.slug || x.wiki !== primary.wiki)];
      return {
        slug: p.slug,
        name: p.name,
        aliases: Array.isArray(p.aliases) ? p.aliases : [],
        wiki: p.wiki,
        wiki_url: pageURL(primary),
        pages,
        summary: typeof p.summary === "string" && p.summary.trim() ? p.summary.trim() : undefined,
        role: p.role ?? undefined,
        org: p.org ?? undefined,
        description: p.description ?? undefined,
      };
    });
}

/** The person holding `slug` as any of its pages. */
export function personBySlug(slug: string, people: WikiPerson[]): WikiPerson | undefined {
  return people.find((p) => p.slug === slug) ?? people.find((p) => p.pages.some((pg) => pg.slug === slug));
}

/** The role line: role · org, else the page's summary. */
export const roleLine = (p: WikiPerson) => [p.role, p.org].filter(Boolean).join(" · ") || p.summary || "";

const norm = (s: string) => s.trim().toLowerCase().replace(/\s+/g, " ");
const namesOf = (p: WikiPerson) => [p.name, shortName(p), ...p.aliases].map(norm).filter(Boolean);
/** Exact match on name, short name or an alias. */
export function exactPerson(name: string, people: WikiPerson[]): WikiPerson | undefined {
  const n = norm(name);
  return n ? people.find((p) => namesOf(p).includes(n)) : undefined;
}

// ---------------------------------------------------------------------------
// One speaker's state, from the record alone.

export interface SpeakerView {
  ref: SpeakerRef;
  display: string;
  state: SpeakerState;
  person?: WikiPerson;
}

export function viewOf(ref: SpeakerRef, entry: Speaker | undefined, people: WikiPerson[]): SpeakerView {
  const name = entry?.name ?? ref.label;
  if (entry?.person) {
    const person = personBySlug(entry.person, people) ?? { slug: entry.person, name, aliases: [], wiki_url: "", pages: [], unindexed: true };
    return { ref, display: name, state: "linked", person };
  }
  if (isPlaceholder(name)) return { ref, display: name, state: "unnamed" };
  const person = exactPerson(name, people);
  if (person) return { ref, display: name, state: "linked", person };
  // The trigger skips a name equal to its role (a label never renamed).
  const resolving = RESOLVER_LIVE && !!entry && name !== entry.role;
  return { ref, display: name, state: resolving ? "resolving" : "named" };
}

interface Derived {
  people: WikiPerson[];
  /** rec id → its speakers' views, in the record's order. */
  byRec: Map<string, SpeakerView[]>;
}
let cache: { rows: Row[]; idx: PeopleIndexRecord | null; config: PageConfig | null; d: Derived } | null = null;

function derive(): Derived {
  const { rows, peopleIndex, config } = mainState();
  if (cache && cache.rows === rows && cache.idx === peopleIndex && cache.config === config) return cache.d;
  const people = indexPeople(peopleIndex);
  const byRec = new Map<string, SpeakerView[]>();
  for (const r of rows) byRec.set(r.rec.id, (r.rec.speakers ?? []).filter((s) => s && typeof s.name === "string").map((s) => viewOf({ rec: r.rec.id, label: labelOf(s) }, s, people)));
  const d = { people, byRec };
  cache = { rows, idx: peopleIndex, config, d };
  return d;
}

/** Re-derive when the rows or the index change. */
function useDerived(): Derived {
  const rows = useStore((s) => s.rows);
  const idx = useStore((s) => s.peopleIndex);
  const config = useStore((s) => s.config);
  return useMemo(() => derive(), [rows, idx, config]);
}

// ---------------------------------------------------------------------------
// Reading.

export function useSpeaker(ref: SpeakerRef): SpeakerView {
  const d = useDerived();
  return useMemo(() => d.byRec.get(ref.rec)?.find((s) => s.ref.label === ref.label) ?? viewOf(ref, undefined, d.people), [d, ref.rec, ref.label]);
}

/** Every speaker of one recording, in the recording's order. */
export function useSpeakersOf(recId: string): SpeakerView[] {
  const d = useDerived();
  return d.byRec.get(recId) ?? [];
}

/** Display names of a recording's speakers (for search and the list). */
export function speakerNamesOf(rec: Rec): SpeakerView[] {
  return derive().byRec.get(rec.id) ?? [];
}

/** The index's people. */
export function usePeople(): WikiPerson[] {
  return useDerived().people;
}

/** Names already on recordings that the index does not hold — offered beside it when naming. */
export function useKnownNames(): string[] {
  const d = useDerived();
  return useMemo(() => {
    const out = new Set<string>();
    for (const views of d.byRec.values()) for (const v of views) if (v.state !== "unnamed" && !(v.person && !v.person.unindexed)) out.add(v.display);
    return [...out].sort((a, b) => a.localeCompare(b, "zh-Hans-CN"));
  }, [d]);
}

// ---------------------------------------------------------------------------
// Writing: CAS on the whole record, touching `speakers` only.

/**
 * `speakers` with the entry for `label` named `name`. `role`
 * keeps the label at first naming; `person` is set on an exact index match,
 * else dropped for the drain to resolve. A label with no entry gains one.
 * Every other entry, and every other field of this one, is unchanged.
 */
export function withName(speakers: Speaker[], label: string, name: string, people: WikiPerson[]): Speaker[] {
  const n = name.trim();
  const i = speakers.findIndex((s) => labelOf(s) === label);
  const old = i >= 0 ? speakers[i] : undefined;
  const { person: _drop, ...rest } = old ?? ({ name: label } as Speaker);
  void _drop;
  const hit = exactPerson(n, people);
  const next: Speaker = { ...rest, role: old ? labelOf(old) : label, name: n, ...(hit ? { person: hit.slug } : {}) };
  return i >= 0 ? speakers.map((s, j) => (j === i ? next : s)) : [...speakers, next];
}

/** `speakers` with the entry for `label` back to its label: `name = role`, no `person`. */
export function withoutName(speakers: Speaker[], label: string): Speaker[] {
  return speakers.map((s) => {
    // Never named here: the entry already is its label.
    if (labelOf(s) !== label || (s.role === undefined && !s.person)) return s;
    const { person: _drop, ...rest } = s;
    void _drop;
    return { ...rest, role: labelOf(s), name: labelOf(s) };
  });
}

export class DeletedMeanwhile extends Error {
  constructor() {
    super("This recording was deleted meanwhile.");
  }
}

const TRIES = 5;
const same = (a: Speaker[], b: Speaker[]) => JSON.stringify(a) === JSON.stringify(b);

/** Re-read, rewrite `speakers`, write at the version read; retry a conflict. */
async function rewriteSpeakers(recId: string, change: (speakers: Speaker[]) => Speaker[]): Promise<void> {
  const key = `rec.${recId}`;
  for (let attempt = 1; ; attempt++) {
    const e: Entry<Rec> = await getRec<Rec>(key);
    const data = e.value?.data;
    if (!data || data.state === "deleted") throw new DeletedMeanwhile();
    const before = Array.isArray(data.speakers) ? data.speakers : [];
    const after = change(before);
    if (same(before, after)) return;
    try {
      await putRec(key, e.value!.schema || "meeting@1", { ...data, speakers: after }, e.version);
      return;
    } catch (err) {
      if (!(err instanceof Refused && err.code === "version_conflict") || attempt >= TRIES) {
        if (err instanceof Refused && err.code === "version_conflict") throw new Error("The recording changed meanwhile. The page shows it as it is now; name the speaker again if still needed.");
        throw err;
      }
    }
  }
}

/** Name a speaker (an empty name unnames it). Resolves once the registry holds it and the list is re-read. */
export async function nameSpeaker(ref: SpeakerRef, name: string): Promise<void> {
  const n = name.trim();
  try {
    await rewriteSpeakers(ref.rec, (s) => (n ? withName(s, ref.label, n, derive().people) : withoutName(s, ref.label)));
  } finally {
    await load();
  }
}

export const unnameSpeaker = (ref: SpeakerRef) => nameSpeaker(ref, "");

// ---------------------------------------------------------------------------
// The People page.

export interface PersonMeeting {
  row: Row;
  labels: string[];
}
export interface PersonEntry {
  person: WikiPerson;
  meetings: PersonMeeting[];
  last?: Date;
}

/**
 * Every wiki person — the index's, plus any `person` a record carries that the
 * index lacks — with the meetings they spoke in, most active first.
 */
export function directory(rows: Row[], d: Derived): PersonEntry[] {
  const by = new Map<string, PersonEntry>(d.people.map((p) => [p.slug, { person: p, meetings: [] }]));
  for (const r of rows) {
    if (r.rec.state === "deleted") continue;
    for (const v of d.byRec.get(r.rec.id) ?? []) {
      if (!v.person) continue;
      const e = by.get(v.person.slug) ?? { person: v.person, meetings: [] };
      by.set(v.person.slug, e);
      if (!e.meetings.some((m) => m.row === r)) e.meetings.push({ row: r, labels: r.rec.labels ?? [] });
    }
  }
  for (const e of by.values()) {
    e.meetings.sort((a, b) => (startedOf(b.row.rec)?.getTime() ?? 0) - (startedOf(a.row.rec)?.getTime() ?? 0));
    e.last = e.meetings[0] ? startedOf(e.meetings[0].row.rec) ?? undefined : undefined;
  }
  return [...by.values()].sort((a, b) => b.meetings.length - a.meetings.length || a.person.name.localeCompare(b.person.name, "zh-Hans-CN"));
}

export function usePeopleDirectory(): PersonEntry[] {
  const d = useDerived();
  const rows = useStore((s) => s.rows);
  return useMemo(() => directory(rows, d), [rows, d]);
}

export interface Waiting {
  /** Names with no wiki page yet, by name. */
  named: { display: string; meetings: Row[]; state: SpeakerState }[];
  /** Speakers still unnamed, by meeting. */
  unnamed: { row: Row; views: SpeakerView[] }[];
}

export function waiting(rows: Row[], d: Derived): Waiting {
  const named = new Map<string, Waiting["named"][number]>();
  const unnamed: Waiting["unnamed"] = [];
  for (const r of rows) {
    if (r.rec.state === "deleted") continue;
    const views = d.byRec.get(r.rec.id) ?? [];
    for (const s of views)
      if (s.state === "named" || s.state === "resolving") {
        const e = named.get(s.display) ?? { display: s.display, meetings: [], state: s.state };
        if (s.state === "resolving") e.state = "resolving";
        if (!e.meetings.includes(r)) e.meetings.push(r);
        named.set(s.display, e);
      }
    const open = views.filter((s) => s.state === "unnamed");
    if (open.length) unnamed.push({ row: r, views: open });
  }
  return { named: [...named.values()].sort((a, b) => b.meetings.length - a.meetings.length || a.display.localeCompare(b.display, "zh-Hans-CN")), unnamed };
}

export function useWaiting(): Waiting {
  const d = useDerived();
  const rows = useStore((s) => s.rows);
  return useMemo(() => waiting(rows, d), [rows, d]);
}
