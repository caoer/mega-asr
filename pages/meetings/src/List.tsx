import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { href, LIST_KEYS, listParams, meetingHref, navigate, rememberList, useRoute } from "./route";
import { speakerNamesOf } from "./people/store";
import { useStore, type Row } from "./store";
import { STATES } from "./types";
import { dayKey, fmtClock, fmtDay, fmtDur, parseTime } from "./time";
import { displayTitle, haystack, labelKind, labelText, sortRows, startedOf, summaryGist, topicsOf, typesOf, type SortKey } from "./model";
import { ProposalsBanner } from "./Proposals";
import { Dialog, DayRuler, HourScale, LabelChip, StateBadge, stateText } from "./ui";

interface Filters {
  q: string;
  status: string;
  labels: string[];
  people: string[];
  source: string;
  sort: SortKey;
  dir: "asc" | "desc";
}

function readFilters(query: URLSearchParams): Filters {
  const sort = (["date", "duration", "status", "title"].includes(query.get("sort") ?? "") ? query.get("sort") : "date") as SortKey;
  const dir = query.get("dir") === "asc" || query.get("dir") === "desc" ? (query.get("dir") as "asc" | "desc") : sort === "title" || sort === "status" ? "asc" : "desc";
  return { q: query.get("q") ?? "", status: query.get("s") ?? "", labels: query.getAll("l"), people: query.getAll("p"), source: query.get("src") ?? "", sort, dir };
}

function toQuery(f: Filters): URLSearchParams {
  const q = new URLSearchParams();
  if (f.q) q.set("q", f.q);
  if (f.status) q.set("s", f.status);
  for (const l of f.labels) q.append("l", l);
  for (const p of f.people) q.append("p", p);
  if (f.source) q.set("src", f.source);
  if (f.sort !== "date") q.set("sort", f.sort);
  const defDir = f.sort === "title" || f.sort === "status" ? "asc" : "desc";
  if (f.dir !== defDir) q.set("dir", f.dir);
  return q;
}

/** Who spoke: a linked speaker counts as their wiki person, a named one by name. */
const peopleOf = (r: Row) =>
  new Set([
    ...speakerNamesOf(r.rec)
      .filter((s) => s.state !== "unnamed")
      .map((s) => (s.person ? s.person.name.replace(/\s*[(（].*$/, "") : s.display)),
    ...(r.rec.labels ?? []).filter((l) => l.startsWith("人:")).map((l) => l.slice(2)),
  ]);
const spokenNames = (r: Row) => speakerNamesOf(r.rec).map((s) => `${s.display} ${s.person?.name ?? ""}`).join("\n").toLowerCase();

/**
 * The list of recordings. With no meeting open it is the front door (filters
 * rail, day groups). With one open it stays mounted as the index on the left:
 * the same filters, the same scroll, one click to the next meeting.
 */
export function List({ selectedId }: { selectedId?: string }) {
  const route = useRoute();
  const rows = useStore((s) => s.rows);
  const tz = useStore((s) => s.tz);
  const progress = useStore((s) => s.progress);
  const f = readFilters(route.query);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const compact = !!selectedId;
  const colRef = useRef<HTMLElement>(null);

  useEffect(() => {
    rememberList(href([], listParams(route.query)));
  }, [route.query]);

  // Filters change in place: on a meeting the index narrows and the meeting stays open.
  const update = (patch: Partial<Filters>) => {
    const q = toQuery({ ...f, ...patch });
    for (const [k, v] of route.query) if (!LIST_KEYS.includes(k)) q.append(k, v);
    navigate(href(route.path, q), true);
  };
  const toggle = (list: string[], v: string) => (list.includes(v) ? list.filter((x) => x !== v) : [...list, v]);

  const { liveCount, visible, counts } = useMemo(() => {
    const q = f.q.trim().toLowerCase();
    const matches = (r: Row) => {
      if (q && !haystack(r.rec).includes(q) && !spokenNames(r).includes(q)) return false;
      if (f.labels.length && !f.labels.every((l) => (r.rec.labels ?? []).includes(l))) return false;
      if (f.people.length) {
        const ps = peopleOf(r);
        if (!f.people.every((p) => ps.has(p))) return false;
      }
      if (f.source && r.rec.source !== f.source) return false;
      return true;
    };
    const live = rows.filter((r) => r.rec.state !== "deleted" && matches(r));
    const dead = rows.filter((r) => r.rec.state === "deleted" && matches(r));
    // Status counts: the other filters applied; deleted is its own pool.
    const counts: Record<string, number> = {};
    for (const s of STATES) counts[s] = 0;
    for (const r of live) counts[r.rec.state] = (counts[r.rec.state] ?? 0) + 1;
    counts.deleted = dead.length;
    const base = f.status === "deleted" ? dead : live;
    const visible = sortRows(f.status && f.status !== "deleted" ? live.filter((r) => r.rec.state === f.status) : base, f.sort, f.dir);
    return { liveCount: live.length, visible, counts };
  }, [rows, f.q, f.status, f.labels.join("\u0000"), f.people.join("\u0000"), f.source, f.sort, f.dir]);

  const hours = visible.reduce((n, r) => n + (r.rec.duration_s ?? 0), 0) / 3600;
  const allLive = rows.filter((r) => r.rec.state !== "deleted").length;
  const active = f.q || f.status || f.labels.length || f.people.length || f.source;

  const groups = useMemo(() => {
    if (f.sort !== "date") return null;
    const out: { day: string; date: Date | null; rows: Row[] }[] = [];
    for (const r of visible) {
      const d = startedOf(r.rec);
      const k = d ? dayKey(d, tz) : "unknown";
      const last = out[out.length - 1];
      if (last && last.day === k) last.rows.push(r);
      else out.push({ day: k, date: d, rows: [r] });
    }
    return out;
  }, [visible, tz, f.sort]);

  // Keep the open meeting in view in the index (deep link, series step, j/k).
  useEffect(() => {
    if (!selectedId) return;
    const el = colRef.current?.querySelector<HTMLElement>(`[data-id="${CSS.escape(selectedId)}"]`);
    el?.scrollIntoView({ block: "nearest" });
  }, [selectedId]);

  // j / k step through the index without leaving the meeting.
  useEffect(() => {
    if (!selectedId) return;
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable) return;
      if (e.key !== "j" && e.key !== "k") return;
      const i = visible.findIndex((r) => r.rec.id === selectedId);
      const next = visible[i + (e.key === "j" ? 1 : -1)];
      if (!next) return;
      e.preventDefault();
      navigate(meetingHref(next.rec.id, route.query));
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectedId, visible, route.query]);

  const facets = (
    <Facets
      rows={visible}
      all={rows}
      f={f}
      onLabel={(l) => update({ labels: toggle(f.labels, l) })}
      onPerson={(p) => update({ people: toggle(f.people, p) })}
      onSource={(s) => update({ source: f.source === s ? "" : s })}
    />
  );

  return (
    <div className="list-page" data-compact={compact ? "" : undefined}>
      {!compact ? (
        <aside className="rail" aria-label="Filters">
          {facets}
        </aside>
      ) : null}
      <section className="list-col" aria-labelledby="list-h" ref={colRef}>
        <div className="list-head">
          <h1 id="list-h" className="count-line">
            <span className="num">{visible.length}</span> {visible.length === 1 ? "recording" : "recordings"}
            <span className="sep">·</span>
            <span className="num">{hours.toFixed(1)}</span>&nbsp;h of audio
            {active ? <span className="of"> of {allLive} recordings</span> : null}
          </h1>
          <div className="list-controls">
            <button type="button" className="btn ghost filters-btn" onClick={() => setFiltersOpen(true)}>
              Filters{f.labels.length + f.people.length + (f.source ? 1 : 0) ? ` · ${f.labels.length + f.people.length + (f.source ? 1 : 0)}` : ""}
            </button>
            <label className="sort">
              <span>Sort</span>
              <select
                value={`${f.sort}:${f.dir}`}
                onChange={(e) => {
                  const [sort, dir] = e.target.value.split(":") as [SortKey, "asc" | "desc"];
                  update({ sort, dir });
                }}
              >
                <option value="date:desc">Newest first</option>
                <option value="date:asc">Oldest first</option>
                <option value="duration:desc">Longest first</option>
                <option value="duration:asc">Shortest first</option>
                <option value="status:asc">Status</option>
                <option value="title:asc">Title A–Z</option>
              </select>
            </label>
          </div>
        </div>

        <div className="states" role="group" aria-label="Filter by status">
          <button type="button" className="pill" aria-pressed={!f.status} onClick={() => update({ status: "" })}>
            All <span className="num">{liveCount}</span>
          </button>
          {STATES.map((s) => (
            <button key={s} type="button" className="pill" data-s={s} data-zero={counts[s] ? undefined : ""} aria-pressed={f.status === s} onClick={() => update({ status: f.status === s ? "" : s })}>
              {stateText(s)} <span className="num">{counts[s]}</span>
            </button>
          ))}
        </div>

        {f.labels.length || f.people.length || f.source || f.q ? (
          <div className="active-filters">
            {f.q ? (
              <span className="af">
                “{f.q}”
                <button type="button" aria-label="Clear search" onClick={() => update({ q: "" })}>
                  ×
                </button>
              </span>
            ) : null}
            {f.labels.map((l) => (
              <span className="af" key={l}>
                {labelText(l)}
                <button type="button" aria-label={`Remove filter ${l}`} onClick={() => update({ labels: toggle(f.labels, l) })}>
                  ×
                </button>
              </span>
            ))}
            {f.people.map((p) => (
              <span className="af" key={p}>
                人 {p}
                <button type="button" aria-label={`Remove filter ${p}`} onClick={() => update({ people: toggle(f.people, p) })}>
                  ×
                </button>
              </span>
            ))}
            {f.source ? (
              <span className="af">
                {f.source}
                <button type="button" aria-label="Remove source filter" onClick={() => update({ source: "" })}>
                  ×
                </button>
              </span>
            ) : null}
            <button type="button" className="link" onClick={() => update({ q: "", labels: [], people: [], source: "" })}>
              Clear All
            </button>
          </div>
        ) : null}

        {f.status === "deleted" ? <p className="note">Deleted recordings keep a tombstone so no source imports them again. Their audio, transcripts and summary are gone.</p> : null}

        {visible.length === 0 ? (
          <div className="empty">
            <p>No recordings match.</p>
            {active ? (
              <button type="button" className="btn ghost" onClick={() => update({ q: "", status: "", labels: [], people: [], source: "" })}>
                Clear Filters
              </button>
            ) : null}
          </div>
        ) : groups ? (
          groups.map((g) => (
            <section className="day" key={g.day} aria-label={g.date ? fmtDay(g.date, tz, true) : "Unknown date"}>
              <div className="day-head">
                <span className="type-pad" aria-hidden="true" />
                <h2>
                  {g.date ? fmtDay(g.date, tz, g.date.getFullYear() !== new Date().getFullYear()) : "No date"}
                  <span className="day-meta">
                    {g.rows.length} · {fmtDur(g.rows.reduce((n, r) => n + (r.rec.duration_s ?? 0), 0))}
                  </span>
                </h2>
                <HourScale />
                <span className="day-pad" />
              </div>
              {g.rows.map((r) => (
                <ListRow key={r.key} row={r} tz={tz} q={f.q} progress={progress[r.rec.id]} f={f} selected={r.rec.id === selectedId} query={route.query} onLabel={(l) => (labelKind(l) === "person" ? update({ people: toggle(f.people, l.slice(2)) }) : update({ labels: toggle(f.labels, l) }))} />
              ))}
            </section>
          ))
        ) : (
          <section className="day flat">
            <div className="day-head">
              <span className="type-pad" aria-hidden="true" />
              <h2>{f.sort === "duration" ? "By length" : f.sort === "status" ? "By status" : "By title"}</h2>
              <HourScale />
              <span className="day-pad" />
            </div>
            {visible.map((r) => (
              <ListRow key={r.key} row={r} tz={tz} q={f.q} progress={progress[r.rec.id]} f={f} selected={r.rec.id === selectedId} query={route.query} withDate onLabel={(l) => (labelKind(l) === "person" ? update({ people: toggle(f.people, l.slice(2)) }) : update({ labels: toggle(f.labels, l) }))} />
            ))}
          </section>
        )}
      </section>

      <Dialog open={filtersOpen} onClose={() => setFiltersOpen(false)} title="Filters" className="sheet">
        <div className="sheet-body">{facets}</div>
        <div className="sheet-foot">
          <button type="button" className="btn primary" onClick={() => setFiltersOpen(false)}>
            Show {visible.length} {visible.length === 1 ? "Recording" : "Recordings"}
          </button>
        </div>
      </Dialog>
    </div>
  );
}

function highlight(text: string, q: string): ReactNode {
  const n = q.trim();
  if (!n) return text;
  const i = text.toLowerCase().indexOf(n.toLowerCase());
  if (i < 0) return text;
  return (
    <>
      {text.slice(0, i)}
      <mark>{text.slice(i, i + n.length)}</mark>
      {text.slice(i + n.length)}
    </>
  );
}

function snippet(summary: string, q: string): string | null {
  const n = q.trim().toLowerCase();
  if (!n) return null;
  const flat = summary.replace(/\*\*/g, "").replace(/\s*\n\s*[-*]?\s*/g, " ");
  const i = flat.toLowerCase().indexOf(n);
  if (i < 0) return null;
  const a = Math.max(0, i - 24);
  return `${a ? "…" : ""}${flat.slice(a, i + n.length + 48).trim()}…`;
}

function ListRow({ row, tz, q, progress, f, withDate, selected, query, onLabel }: { row: Row; tz: string; q: string; progress?: number; f: Filters; withDate?: boolean; selected: boolean; query: URLSearchParams; onLabel: (l: string) => void }) {
  const rec = row.rec;
  const start = parseTime(rec.started);
  const end = start && rec.duration_s ? new Date(start.getTime() + rec.duration_s * 1000) : null;
  const gist = summaryGist(rec);
  const snip = q && rec.summary && !gist.text.toLowerCase().includes(q.trim().toLowerCase()) ? snippet(rec.summary, q) : null;
  const titled = !!rec.display_title?.trim();
  const spoken = speakerNamesOf(rec);
  const people = spoken.filter((s) => s.state !== "unnamed").map((s) => (s.person ? s.person.name.replace(/\s*[(（].*$/, "") : s.display));
  const unnamed = spoken.filter((s) => s.state === "unnamed").length;
  const types = typesOf(rec);
  const topics = topicsOf(rec);
  return (
    <article className="row" data-s={rec.state} data-id={rec.id} data-selected={selected ? "" : undefined}>
      <div className="row-type">
        {types.map((t) => (
          <button key={t} type="button" className="type" aria-pressed={f.labels.includes(t)} onClick={() => onLabel(t)} aria-label={`Show the ${t} series`}>
            {t}
          </button>
        ))}
      </div>
      <div className="row-main">
        <h3 className="row-title">
          <a href={meetingHref(rec.id, query)} aria-current={selected ? "page" : undefined}>
            {highlight(displayTitle(rec), q)}
          </a>
        </h3>
        <div className="row-meta">
          <StateBadge state={rec.state} extra={rec.action ? <span className="queued"> · {rec.action === "retry" ? "retry" : rec.action === "realign" ? "re-align" : "re-ingest"} queued</span> : null} />
          {titled && rec.title && rec.title !== rec.display_title ? <span className="orig">「{highlight(rec.title, q)}」</span> : !titled ? <span className="pending-title">title pending</span> : null}
          {people.length || unnamed ? (
            <span className="people" title={[...people, unnamed ? `${unnamed} unnamed` : ""].filter(Boolean).join("、")}>
              {people.slice(0, 3).join("、")}
              {people.length > 3 ? ` +${people.length - 3}` : ""}
              {unnamed ? <span className="unnamed-n">{people.length ? " · " : ""}{unnamed} unnamed</span> : null}
            </span>
          ) : null}
          <span className="src">
            {rec.source}
            {rec.host ? ` · ${rec.host}` : ""}
          </span>
        </div>
        {rec.state === "deleted" ? null : (
          <p className="row-gist" data-kind={gist.kind}>{snip ? highlight(snip, q) : gist.kind === "gist" ? highlight(gist.text, q) : gist.text}</p>
        )}
        {rec.state === "failed" && rec.error ? <p className="row-err">Failed at {rec.stage ?? "?"}: {rec.error}</p> : null}
        {progress !== undefined ? (
          <div className="progress" role="progressbar" aria-label="Upload progress" aria-valuenow={Math.round(progress * 100)} aria-valuemin={0} aria-valuemax={100}>
            <span style={{ transform: `scaleX(${progress})` }} />
          </div>
        ) : null}
        {topics.length ? (
          <div className="row-labels">
            {topics.map((l) => (
              <LabelChip key={l} label={l} onPick={() => onLabel(l)} pressed={labelKind(l) === "person" ? f.people.includes(l.slice(2)) : f.labels.includes(l)} />
            ))}
          </div>
        ) : null}
      </div>
      <div className="row-when">
        <DayRuler rec={rec} tz={tz} />
        <div className="row-time">
          {start ? (
            <time dateTime={start.toISOString()}>
              {withDate ? `${fmtDay(start, tz)} ` : ""}
              {fmtClock(start, tz)}
              {end ? `–${fmtClock(end, tz)}` : ""}
            </time>
          ) : (
            "—"
          )}
          <span className="dur">{fmtDur(rec.duration_s)}</span>
        </div>
      </div>
    </article>
  );
}

function Facets({ rows, all, f, onLabel, onPerson, onSource }: { rows: Row[]; all: Row[]; f: Filters; onLabel: (l: string) => void; onPerson: (p: string) => void; onSource: (s: string) => void }) {
  const vocabulary = useStore((s) => s.vocabulary);
  const [pq, setPq] = useState("");
  const [morePeople, setMorePeople] = useState(false);
  const labelCounts = new Map<string, number>();
  const personCounts = new Map<string, number>();
  const sourceCounts = new Map<string, number>();
  for (const r of rows) {
    for (const l of r.rec.labels ?? []) labelCounts.set(l, (labelCounts.get(l) ?? 0) + 1);
    for (const p of peopleOf(r)) personCounts.set(p, (personCounts.get(p) ?? 0) + 1);
  }
  for (const r of all) if (r.rec.state !== "deleted" && r.rec.source) sourceCounts.set(r.rec.source, 0);
  for (const r of rows) if (r.rec.source) sourceCounts.set(r.rec.source, (sourceCounts.get(r.rec.source) ?? 0) + 1);

  const types = vocabulary.closed.map((l) => ({ l, n: labelCounts.get(l) ?? 0 }));
  const topics = vocabulary.open
    .map((l) => ({ l, n: labelCounts.get(l) ?? 0 }))
    .filter((x) => x.n || f.labels.includes(x.l))
    .sort((a, b) => b.n - a.n || a.l.localeCompare(b.l, "zh-Hans-CN"));

  const people = [...personCounts.entries()].map(([p, n]) => ({ p, n }));
  for (const p of f.people) if (!personCounts.has(p)) people.push({ p, n: 0 });
  people.sort((a, b) => b.n - a.n || a.p.localeCompare(b.p, "zh-Hans-CN"));
  const pFiltered = pq ? people.filter((x) => x.p.toLowerCase().includes(pq.toLowerCase())) : people;
  const shownPeople = morePeople || pq ? pFiltered : pFiltered.slice(0, 10);

  return (
    <div className="facets">
      <ProposalsBanner />
      <section>
        <h2>
          Meeting type <span className="h2-sub">closed list</span>
        </h2>
        <Facet items={types.map((x) => ({ key: x.l, text: x.l, n: x.n }))} pressed={(k) => f.labels.includes(k)} onPick={onLabel} />
      </section>
      <section>
        <h2>
          Topics <span className="h2-sub">open list · agent-kept</span>
        </h2>
        <Facet items={topics.map((x) => ({ key: x.l, text: <span className="topic-name">{x.l}</span>, n: x.n }))} pressed={(k) => f.labels.includes(k)} onPick={onLabel} />
      </section>
      <section>
        <h2>People</h2>
        {people.length > 10 ? (
          <input className="facet-filter" type="search" name="people-filter" autoComplete="off" aria-label="Find a person" placeholder={`Find among ${people.length} people…`} value={pq} onChange={(e) => setPq(e.target.value)} />
        ) : null}
        <Facet items={shownPeople.map((x) => ({ key: x.p, text: x.p, n: x.n }))} pressed={(k) => f.people.includes(k)} onPick={onPerson} />
        {!pq && pFiltered.length > 10 ? (
          <button type="button" className="link facet-more" onClick={() => setMorePeople(!morePeople)}>
            {morePeople ? "Show Fewer" : `Show All ${pFiltered.length}`}
          </button>
        ) : null}
      </section>
      <section>
        <h2>Source</h2>
        <Facet items={[...sourceCounts.entries()].map(([s, n]) => ({ key: s, text: s, n }))} pressed={(k) => f.source === k} onPick={onSource} />
      </section>
    </div>
  );
}

function Facet({ items, pressed, onPick }: { items: { key: string; text: ReactNode; n: number }[]; pressed: (k: string) => boolean; onPick: (k: string) => void }) {
  return (
    <ul className="facet">
      {items.map((it) => (
        <li key={it.key}>
          <button type="button" aria-pressed={pressed(it.key)} onClick={() => onPick(it.key)}>
            <span className="facet-name">{it.text}</span>
            <span className="num">{it.n}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}
