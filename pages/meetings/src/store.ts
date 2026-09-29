// One store over the page's registry: the recordings (`rec.*`), what is
// queued (`q.*`), the label vocabulary (`labels`) and the people index
// (`people.index`, optional). Read on load, every 30 s while the page is
// visible, and after each write.
import { useSyncExternalStore } from "react";
import type { FileRef, LabelQuestion, LabelsRecord, PageConfig, PeopleIndexRecord, Rec, Segment } from "./types";
import { DEVICE_TZ } from "./time";
import { normLabels, setClosedList, type LabelChange } from "./model";
import { canRead, explain, fileURL, getRec, listAll, probeRole, putRec, Refused, SLUG, type Entry } from "./registry";

export type Theme = "system" | "light" | "dark";

export interface Vocabulary {
  /** The meeting types: the closed `type` entries. */
  closed: string[];
  /** Every other label: closed projects and topics, and what the agent coined. */
  open: string[];
}

export interface Row {
  key: string;
  version: number;
  rec: Rec;
  /** `q.<id>` exists: the drain has it queued. */
  queued: boolean;
  /** Labels the agent set, to tell them from the viewer's. */
  agentLabels: string[];
}

interface State {
  /** The first read has landed (or been refused as a contributor's). */
  loaded: boolean;
  /** The first read failed; the page shows why. */
  loadError: string | null;
  /** A later read failed; the page keeps the last one. */
  pollError: string | null;
  lastLoad: Date | null;
  /** Opened through a contributor (upload) link. */
  contributor: boolean;
  tz: string;
  theme: Theme;
  progress: Record<string, number>;
  rows: Row[];
  vocabulary: Vocabulary;
  closed: NonNullable<LabelsRecord["closed"]>;
  questions: LabelQuestion[];
  peopleIndex: PeopleIndexRecord | null;
  /** The `config` record: wiki link templates and the default zone. */
  config: PageConfig | null;
}

function readString(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function write(key: string, v: string) {
  try {
    localStorage.setItem(key, v);
  } catch {
    /* private mode: the choice lasts for this visit */
  }
}

const storedTheme = readString("meetings.theme");
const storedTz = readString("meetings.tz");
/** The viewer chose a zone: the config's zone no longer applies. */
let tzChosen = !!storedTz;
let state: State = {
  loaded: false,
  loadError: null,
  pollError: null,
  lastLoad: null,
  contributor: false,
  tz: storedTz || DEVICE_TZ,
  theme: storedTheme === "light" || storedTheme === "dark" ? storedTheme : "system",
  progress: {},
  rows: [],
  vocabulary: { closed: [], open: [] },
  closed: [],
  questions: [],
  peopleIndex: null,
  config: null,
};

const listeners = new Set<() => void>();
function set(patch: Partial<State>) {
  state = { ...state, ...patch };
  for (const l of listeners) l();
}
export const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => listeners.delete(l);
};
export function useStore<T>(pick: (s: State) => T): T {
  return useSyncExternalStore(subscribe, () => pick(state));
}
export const getState = () => state;

// ---------------------------------------------------------------------------
// Reading the registry.

/** The registry as read, turned into the store's rows and vocabulary. */
export function derive(recs: Entry<Rec>[], queue: Entry[], labels: LabelsRecord | null, people: PeopleIndexRecord | null): Pick<State, "rows" | "vocabulary" | "closed" | "questions" | "peopleIndex"> {
  const closed = (labels?.closed ?? []).filter((c) => c && typeof c.name === "string" && c.name.trim());
  const types = closed.filter((c) => c.kind === "type").map((c) => c.name.trim());
  setClosedList(types);
  const queued = new Set(queue.map((q) => q.key.slice(2)));
  const rows: Row[] = recs.map((e) => {
    const data = e.value?.data && typeof e.value.data === "object" ? e.value.data : ({} as Rec);
    const rec: Rec = { ...data, id: data.id || e.key.slice(4), state: data.state || "unknown" };
    return { key: e.key, version: e.version, rec, queued: queued.has(rec.id), agentLabels: rec.labels ?? [] };
  });
  const open = new Set(closed.filter((c) => c.kind !== "type").map((c) => c.name.trim()));
  for (const r of rows) for (const l of r.rec.labels ?? []) if (!types.includes(l)) open.add(l);
  return {
    rows,
    vocabulary: { closed: types, open: [...open] },
    closed,
    questions: Array.isArray(labels?.questions) ? labels.questions : [],
    peopleIndex: people && Array.isArray(people.people) ? people : null,
  };
}

/** The config record, and its zone for a viewer who has not chosen one. */
function configPatch(config: PageConfig | null): Partial<State> {
  const cfg = config && typeof config === "object" ? config : null;
  const tz = !tzChosen && typeof cfg?.tz === "string" && validZone(cfg.tz) ? { tz: cfg.tz } : {};
  return { config: cfg, ...tz };
}
function validZone(tz: string): boolean {
  try {
    new Intl.DateTimeFormat("en", { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

const optional = <T>(p: Promise<Entry<T>>) => p.then((e) => e.value?.data ?? null).catch(() => null);

let loading: Promise<void> | null = null;
let probed = false;

/** Read everything once; concurrent calls share one read. */
export function load(): Promise<void> {
  if (!loading) loading = read().finally(() => (loading = null));
  return loading;
}

async function read() {
  if (!SLUG) {
    set({ loadError: "Open this page through its /a/<slug> address." });
    return;
  }
  if (!probed) {
    probed = true;
    set({ contributor: (await probeRole()) === "contributor" });
  }
  try {
    const [recs, queue, labels, people, config] = await Promise.all([
      listAll<Rec>("rec.", true),
      listAll("q.", false).catch(() => []),
      optional(getRec<LabelsRecord>("labels")),
      optional(getRec<PeopleIndexRecord>("people.index")),
      optional(getRec<PageConfig>("config")),
    ]);
    set({ ...derive(recs, queue, labels, people), ...configPatch(config), loaded: true, loadError: null, pollError: null, lastLoad: new Date() });
  } catch (e) {
    // A link that may upload but not list: a normal state, not an error.
    if (e instanceof Refused && (e.status === 401 || e.status === 403) && state.contributor) {
      set({ ...derive([], [], null, null), loaded: true, loadError: null, pollError: null, lastLoad: new Date() });
      return;
    }
    const msg = `Could not read the recordings: ${explain(e)}`;
    if (state.loaded) set({ pollError: msg });
    else set({ loadError: msg });
  }
}

export const POLL_MS = 30_000;
let timer: ReturnType<typeof setInterval> | null = null;

/** First read, then every 30 s while the page is visible, and on coming back. */
export function start() {
  void load();
  if (timer) return;
  timer = setInterval(() => {
    if (!document.hidden) void load();
  }, POLL_MS);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && state.lastLoad && Date.now() - state.lastLoad.getTime() > POLL_MS) void load();
  });
}

// ---------------------------------------------------------------------------
// Preferences.

export function setTz(tz: string) {
  tzChosen = true;
  write("meetings.tz", tz);
  set({ tz });
}

export function setTheme(theme: Theme) {
  write("meetings.theme", theme);
  if (theme === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
  set({ theme });
}

// ---------------------------------------------------------------------------
// Writes. Each resolves when the registry holds it and the list is re-read.

// Labels: setLabels below. Actions and Delete: ./actions.ts. Upload: ./uploadFlow.ts.

/** Upload progress (0–1) of a recording in flight; `undefined` when it settles. */
export function setProgress(id: string, p: number | undefined) {
  const progress = { ...state.progress };
  if (p === undefined) delete progress[id];
  else progress[id] = p;
  set({ progress });
}

// ---------------------------------------------------------------------------
// Labels: a CAS on rec.<id> touching only `labels`. Re-read, apply the change
// to the labels read, write at that version; a conflict (the drain, the
// labels agent, another tab) re-reads and applies again, five tries.

export const LABEL_TRIES = 5;

export async function setLabels(id: string, change: LabelChange): Promise<void> {
  const key = `rec.${id}`;
  for (let attempt = 1; ; attempt++) {
    const cur = await getRec<Rec>(key);
    const data = cur.value?.data ?? ({ id, state: "unknown" } as Rec);
    if (data.state === "deleted") {
      void load();
      throw new Error("This recording was deleted meanwhile.");
    }
    const before = normLabels(data.labels);
    const after = normLabels(change(before));
    if (before.length === after.length && before.every((l, i) => l === after[i])) return;
    try {
      const res = await putRec(key, cur.value?.schema || "meeting@1", { ...data, labels: after }, cur.version);
      patchRow(key, { ...data, labels: after }, res?.version ?? cur.version + 1);
      void load();
      return;
    } catch (e) {
      if (!(e instanceof Refused && e.code === "version_conflict")) throw e;
      if (attempt >= LABEL_TRIES) {
        void load();
        throw new Error("The recording changed meanwhile and the labels were not saved. The list is refreshed; try again.");
      }
    }
  }
}

/** Show a write at once; the re-read that follows confirms it. */
function patchRow(key: string, rec: Rec, version: number) {
  set({ rows: state.rows.map((r) => (r.key === key ? { ...r, rec: { ...r.rec, ...rec }, version } : r)) });
}

// ---------------------------------------------------------------------------
// Files: text and audio stream from /f/.

const textCache = new Map<string, Promise<string>>();

export const audioURL = (fileId: string): string => fileURL(fileId);
export { canRead };

export function loadText(fileId: string): Promise<string> {
  let p = textCache.get(fileId);
  if (!p) {
    p = fetch(fileURL(fileId), { credentials: "same-origin" }).then((r) => {
      if (r.status === 404 || r.status === 403) throw new Error("Not readable with this link.");
      if (!r.ok) throw new Error(`The file answered ${r.status}.`);
      return r.text();
    });
    p.catch(() => textCache.delete(fileId));
    textCache.set(fileId, p);
  }
  return p;
}

export const fileByRole = (rec: Rec, role: string): FileRef | undefined => (rec.files ?? []).find((f) => f.role === role && f.file);

function parseSegmentsJSON(text: string): Segment[] {
  const doc = JSON.parse(text) as unknown;
  const list = Array.isArray(doc) ? doc : doc && typeof doc === "object" && Array.isArray((doc as { segments?: unknown }).segments) ? (doc as { segments: unknown[] }).segments : [];
  const out: Segment[] = [];
  for (const s of list as Record<string, unknown>[]) {
    if (!s || typeof s.start_s !== "number") continue;
    out.push({ speaker: String(s.speaker ?? ""), start: s.start_s, end: typeof s.end_s === "number" ? s.end_s : NaN, text: String(s.text ?? ""), raw: typeof s.raw === "string" ? s.raw : null });
  }
  return out.sort((a, b) => a.start - b.start);
}

function parseTranscriptMd(text: string): Segment[] {
  const out: Segment[] = [];
  for (const line of text.split("\n")) {
    const m = /^\[?(\d{1,2}):(\d{2}):(\d{2})\]?\s+([^:：]{1,60})[:：]\s?(.*)$/.exec(line.trim());
    if (m) out.push({ speaker: m[4].trim(), start: +m[1] * 3600 + +m[2] * 60 + +m[3], end: NaN, text: m[5], raw: null });
    else if (out.length && line.trim()) out[out.length - 1].text += `\n${line.trim()}`;
  }
  return out;
}

/** Fill missing ends from the next start. */
function fillEnds(segs: Segment[]): Segment[] {
  for (let i = 0; i < segs.length; i++) if (!Number.isFinite(segs[i].end)) segs[i].end = i + 1 < segs.length ? segs[i + 1].start : segs[i].start + 20;
  return segs;
}

export async function loadSegments(f: FileRef): Promise<Segment[]> {
  const text = await loadText(f.file!);
  return fillEnds(f.role === "transcript" ? parseTranscriptMd(text) : parseSegmentsJSON(text));
}

/** Our ASR (segments, else transcript.md) and Feishu's minute, when present. */
export function transcriptSources(rec: Rec): { ours?: FileRef; feishu?: FileRef } {
  return { ours: fileByRole(rec, "segments") ?? fileByRole(rec, "transcript"), feishu: fileByRole(rec, "feishu-transcript") };
}
