import { useEffect, useMemo, useRef, useState, type KeyboardEvent as RKeyboardEvent } from "react";
import { href, lastListHref, listHref, meetingHref, navigate, useRoute } from "./route";
import { useSpeakersOf } from "./people/store";
import { SpeakerChip } from "./people/SpeakerChip";
import { getState, transcriptSources, useStore, type Row } from "./store";
import { wikiLink } from "./wiki";
import { deleteRecording, queueAction } from "./actions";
import { explain, fileURL } from "./registry";
import { ACTIONS, type ActionName, type Rec } from "./types";
import { fmtClock, fmtDateTime, fmtDur, parseTime, tzOffset } from "./time";
import { displayTitle, startedOf, typesOf } from "./model";
import { Markdown } from "./Markdown";
import { PlayerProvider, UnifiedPlayer } from "./player";
import { Dialog, fmtBytes, StateBadge, toast } from "./ui";
import { LabelEditor } from "./Labels";
import { Alignment } from "./Alignment";

/** A tag jump that keeps the meeting open: the index narrows to the label. */
function withLabel(query: URLSearchParams, id: string, label: string): string {
  const q = new URLSearchParams(query);
  if (!q.getAll("l").includes(label)) q.append("l", label);
  return href(["m", id], q);
}
/** A path in the ingest wiki, or a full URL, as a web address; "" when the page config has no template for it. */
function wikiPageURL(page: string, commit?: string): string {
  const cfg = getState().config;
  return wikiLink(cfg, cfg?.ingest_wiki, page, commit);
}
function wikiURL(rec: Rec): string | null {
  const page = rec.wiki?.page;
  return (page && wikiPageURL(page, rec.wiki?.commit)) || null;
}

type Tab = "summary" | "align" | "details";

export function Detail({ id }: { id: string }) {
  const rows = useStore((s) => s.rows);
  const row = rows.find((r) => r.rec.id === id);
  if (!row)
    return (
      <div className="page-msg">
        <p>
          <a href={lastListHref()}>← All meetings</a>
        </p>
        <h1>No recording “{id}”</h1>
        <p>It is not on this page, or this link cannot read it. Check the link, or go back to the list.</p>
      </div>
    );
  return (
    <PlayerProvider key={id}>
      <DetailBody row={row} />
    </PlayerProvider>
  );
}

function DetailBody({ row }: { row: Row }) {
  const rec = row.rec;
  const route = useRoute();
  const tz = useStore((s) => s.tz);
  const src = transcriptSources(rec);
  const deleted = rec.state === "deleted";
  const tabs: { id: Tab; label: string; show: boolean }[] = [
    { id: "summary", label: "Summary", show: true },
    { id: "align", label: "Alignment", show: !deleted && !!(src.ours && src.feishu) },
    { id: "details", label: "Details & files", show: true },
  ];
  const asked = route.query.get("tab") as Tab | null;
  const tab: Tab = tabs.find((t) => t.id === asked && t.show)?.id ?? "summary";
  const setTab = (t: Tab) => {
    const q = new URLSearchParams(route.query);
    if (t === "summary") q.delete("tab");
    else q.set("tab", t);
    navigate(href(["m", rec.id], q), true);
  };
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});
  const onTabKey = (e: RKeyboardEvent) => {
    const shown = tabs.filter((t) => t.show);
    const i = shown.findIndex((t) => t.id === tab);
    const j = e.key === "ArrowRight" ? (i + 1) % shown.length : e.key === "ArrowLeft" ? (i - 1 + shown.length) % shown.length : -1;
    if (j < 0) return;
    e.preventDefault();
    setTab(shown[j].id);
    tabRefs.current[shown[j].id]?.focus();
  };

  const start = parseTime(rec.started);
  const end = parseTime(rec.stopped) ?? (start && rec.duration_s ? new Date(start.getTime() + rec.duration_s * 1000) : null);
  const wiki = wikiURL(rec);
  const titled = !!rec.display_title?.trim();
  const types = typesOf(rec);
  const rows = useStore((s) => s.rows);
  const series = useMemo(() => {
    const t = types[0];
    if (!t) return null;
    const list = rows.filter((r) => r.rec.state !== "deleted" && typesOf(r.rec).includes(t)).sort((a, b) => (startedOf(a.rec)?.getTime() ?? 0) - (startedOf(b.rec)?.getTime() ?? 0));
    const i = list.findIndex((r) => r.rec.id === rec.id);
    return i < 0 ? null : { t, n: list.length, i, prev: list[i - 1], next: list[i + 1] };
  }, [rows, types.join("|"), rec.id]);

  useEffect(() => {
    document.title = `${displayTitle(rec)} · Meetings`;
    return () => {
      document.title = "Meetings";
    };
  }, [rec]);

  return (
    <article className="detail">
      <p className="back">
        <a href={listHref(route.query)}>
          <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
            <path d="M7.5 2L3.5 6l4 4" fill="none" stroke="currentColor" strokeWidth="1.6" />
          </svg>
          All meetings
        </a>
        <span className="back-keys muted" aria-hidden="true">
          <kbd>j</kbd> <kbd>k</kbd> next · previous
        </span>
      </p>
      <header className="d-head">
        <div className="d-titleline">
          {types.map((t) => (
            <a key={t} className="type big" href={withLabel(route.query, rec.id, t)} title={`Show the ${t} series in the index`}>
              {t}
            </a>
          ))}
          <h1>{displayTitle(rec)}</h1>
        </div>
        {series && series.n > 1 ? (
          <nav className="series" aria-label={`${series.t} series`}>
            {series.prev ? (
              <a href={meetingHref(series.prev.rec.id, route.query)} className="series-step">
                ← <span>{displayTitle(series.prev.rec)}</span>
              </a>
            ) : (
              <span className="series-step muted">First in series</span>
            )}
            <a className="series-pos num" href={withLabel(route.query, rec.id, series.t)}>
              {series.i + 1} of {series.n} · {series.t}
            </a>
            {series.next ? (
              <a href={meetingHref(series.next.rec.id, route.query)} className="series-step next">
                <span>{displayTitle(series.next.rec)}</span> →
              </a>
            ) : (
              <span className="series-step next muted">Latest</span>
            )}
          </nav>
        ) : null}
        <p className="d-orig">
          {titled ? (
            <>
              <span className="k">Original name</span> <span>「{rec.title || rec.id}」</span>
            </>
          ) : (
            <span className="pending-title">Title pending — the agent writes one after the transcript.</span>
          )}
        </p>
        <div className="d-meta">
          <StateBadge state={rec.state} extra={rec.action && ACTIONS[rec.action] ? <span className="queued"> · {ACTIONS[rec.action].done.toLowerCase()}</span> : null} />
          {start ? (
            <span>
              <time dateTime={start.toISOString()}>{fmtDateTime(start, tz)}</time>
              {end ? `–${fmtClock(end, tz)}` : ""} <span className="muted">{tzOffset(tz, start)}</span>
            </span>
          ) : null}
          <span>{fmtDur(rec.duration_s)}</span>
          <span>
            {rec.source}
            {rec.host ? ` on ${rec.host}` : ""}
          </span>
          {wiki ? (
            <a href={wiki} target="_blank" rel="noopener" title={rec.wiki?.page}>
              Wiki page ↗
            </a>
          ) : null}
        </div>
        {!deleted ? <LabelEditor row={row} /> : null}
        {!deleted ? <ActionBar rec={rec} /> : null}
      </header>

      {rec.state === "failed" || (rec.error && rec.state !== "deleted") ? (
        <section className="alert" data-kind="failed">
          <h2>{rec.stage ? `Failed at ${rec.stage}` : "Failed"}</h2>
          {rec.error ? <pre>{rec.error}</pre> : <p>No reason recorded.</p>}
          {rec.state === "failed" ? <p className="muted">Retry queues it for the server’s next pass.</p> : null}
        </section>
      ) : null}
      {deleted ? (
        <section className="alert" data-kind="deleted">
          <h2>Deleted</h2>
          <p>
            {parseTime(rec.deleted_at) ? fmtDateTime(parseTime(rec.deleted_at)!, tz) : "At an unknown time"}
            {rec.deleted_by ? ` by ${rec.deleted_by}` : ""}. Its audio, transcripts and summary are gone; the entry stays so no source imports this recording again.
          </p>
        </section>
      ) : null}

      {!deleted && rec.ingest ? <IngestBlock ingest={rec.ingest} /> : null}

      {!deleted ? <UnifiedPlayer rec={rec} personHref={(p) => href(["people", p.slug])} /> : null}

      <div className="tabs" role="tablist" aria-label="Recording views" onKeyDown={onTabKey}>
        {tabs
          .filter((t) => t.show)
          .map((t) => (
            <button key={t.id} ref={(el) => void (tabRefs.current[t.id] = el)} type="button" role="tab" id={`tab-${t.id}`} aria-controls={`panel-${t.id}`} aria-selected={tab === t.id} tabIndex={tab === t.id ? 0 : -1} onClick={() => setTab(t.id)}>
              {t.label}
            </button>
          ))}
      </div>
      <div className="panel" role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`}>
        {tab === "summary" ? <SummaryPanel rec={rec} /> : tab === "align" ? <Alignment rec={rec} /> : <DetailsPanel row={row} />}
      </div>
    </article>
  );
}

// ---------------------------------------------------------------------------

const INGEST_STATE: Record<string, string> = { queued: "Queued", running: "Running", done: "Done", partial: "Partly done", failed: "Failed" };

/** The drain's ingest run for this recording, mirrored onto the record. */
function IngestBlock({ ingest }: { ingest: NonNullable<Rec["ingest"]> }) {
  const tz = useStore((s) => s.tz);
  const at = parseTime(ingest.updated_at);
  const state = ingest.state ?? "unknown";
  return (
    <section className="alert ingest" data-kind={state === "failed" ? "failed" : "ingest"} aria-label="Wiki ingest">
      <h2>
        Wiki ingest · {INGEST_STATE[state] ?? state}
        {ingest.step ? <span className="muted"> · {ingest.step}</span> : null}
      </h2>
      <p className="ingest-meta">
        {typeof ingest.usd === "number" ? <span className="num">${ingest.usd.toFixed(2)}</span> : null}
        {ingest.wiki_page && wikiPageURL(ingest.wiki_page) ? (
          <a href={wikiPageURL(ingest.wiki_page)} target="_blank" rel="noopener" title={ingest.wiki_page}>
            Wiki page ↗
          </a>
        ) : null}
        {ingest.run ? <span className="mono" title="Run">{ingest.run}</span> : null}
        {at ? <span className="muted">updated {fmtDateTime(at, tz)}</span> : null}
      </p>
      {ingest.error ? <pre>{ingest.error}</pre> : null}
    </section>
  );
}

// ---------------------------------------------------------------------------

function ActionBar({ rec }: { rec: Rec }) {
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const contributor = useStore((s) => s.contributor);
  const files = (rec.files ?? []).filter((f) => f.file).length;
  const write = (p: Promise<void>, done: string) => {
    setBusy(true);
    p.then(
      () => toast(done),
      (e) => toast(explain(e)),
    ).finally(() => setBusy(false));
  };
  const avail = (Object.keys(ACTIONS) as ActionName[]).filter((a) => ACTIONS[a].states.includes(rec.state) && !rec.action);
  return (
    <div className="actions">
      {avail.map((a) => (
        <button key={a} type="button" className="btn ghost" disabled={busy} onClick={() => write(queueAction(rec.id, a), `${ACTIONS[a].done}. The server picks it up on its next pass.`)}>
          {ACTIONS[a].label}
        </button>
      ))}
      {!contributor ? (
        <button type="button" className="btn danger-ghost" disabled={busy} onClick={() => setConfirm(true)}>
          Delete…
        </button>
      ) : null}
      <Dialog open={confirm} onClose={() => setConfirm(false)} title="Delete this recording?">
        <div className="confirm">
          <p>
            Delete <strong>“{displayTitle(rec)}”</strong> and its {files} {files === 1 ? "file" : "files"}? This cannot be undone. It stays listed under Deleted, so it is not imported again.
          </p>
          <div className="confirm-actions">
            <button type="button" className="btn ghost" onClick={() => setConfirm(false)} autoFocus>
              Keep Recording
            </button>
            <button
              type="button"
              className="btn danger"
              onClick={() => {
                setConfirm(false);
                write(deleteRecording(rec.id), "Deleted. Its files are gone; it is listed under Deleted.");
              }}
            >
              Delete Recording
            </button>
          </div>
        </div>
      </Dialog>
    </div>
  );
}

// ---------------------------------------------------------------------------

function SummaryPanel({ rec }: { rec: Rec }) {
  const speakers = useSpeakersOf(rec.id);
  if (rec.state === "deleted") return <p className="muted">The summary went with the recording’s files.</p>;
  const unnamed = speakers.filter((s) => s.state === "unnamed").length;
  return (
    <div className="summary">
      <div className="summary-text">
        {rec.summary === undefined || rec.summary === null ? (
          <p className="placeholder">Summary pending. It is written once the meeting is transcribed.</p>
        ) : !rec.summary.trim() ? (
          <p className="placeholder">No summary for this recording — there was too little speech to summarise.</p>
        ) : (
          <Markdown source={rec.summary} />
        )}
      </div>
      <aside className="summary-side">
        <h2>Speakers</h2>
        {speakers.length ? (
          <ul className="speakers">
            {speakers.map((v, i) => (
              <li key={v.ref.label}>
                <SpeakerChip v={v} dot={i % 8} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="muted">None recognised.</p>
        )}
        {unnamed ? <p className="speakers-hint">Name a speaker in the player above — it links to their wiki page.</p> : null}
      </aside>
    </div>
  );
}

function DetailsPanel({ row }: { row: Row }) {
  const rec = row.rec;
  const tz = useStore((s) => s.tz);
  const s = rec.scores ?? {};
  const kv: [string, string | null][] = [
    ["Status", `${rec.state}${rec.stage ? ` · ${rec.stage}` : ""}`],
    ["Started", parseTime(rec.started) ? fmtDateTime(parseTime(rec.started)!, tz) : null],
    ["Stopped", parseTime(rec.stopped) ? fmtDateTime(parseTime(rec.stopped)!, tz) : null],
    ["Duration", fmtDur(rec.duration_s)],
    ["Source", [rec.source, rec.host].filter(Boolean).join(" on ")],
    ["Attempts", typeof rec.attempts === "number" && rec.attempts ? String(rec.attempts) : null],
    ["RTF", typeof s.rtf === "number" && s.rtf > 0 ? `${s.rtf.toFixed(3)} (processing ÷ audio time)` : null],
    ["Loops", typeof s.loops === "number" ? `${s.loops} re-decoded` : null],
    ["Engine", rec.engine?.root ? rec.engine.root.replace(/^\/nix\/store\/[a-z0-9]{32}-/, "") : null],
    ["Feishu minute", rec.feishu?.token ?? null],
    ["Wiki", rec.wiki?.page ? `${rec.wiki.page}${rec.wiki.commit ? ` @ ${rec.wiki.commit.slice(0, 10)}` : ""}` : null],
    ["Record", `${rec.id} · v${row.version}`],
  ];
  const files = rec.files ?? [];
  return (
    <div className="details">
      <dl className="kv">
        {kv
          .filter(([, v]) => v)
          .map(([k, v]) => (
            <div key={k}>
              <dt>{k}</dt>
              <dd className={k === "Record" || k === "Engine" || k === "Feishu minute" || k === "Wiki" ? "mono" : undefined} translate={k === "Record" || k === "Engine" ? "no" : undefined}>
                {v}
              </dd>
            </div>
          ))}
      </dl>
      <h2>Files</h2>
      {files.length ? (
        <div className="scroller">
          <table className="files">
            <thead>
              <tr>
                <th>Role</th>
                <th className="num">Size</th>
                <th>Codec</th>
                <th>sha256</th>
                <th>Verified</th>
                <th>
                  <span className="sr-only">Download</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {files.map((f) => {
                const url = f.file ? fileURL(f.file) : null;
                return (
                  <tr key={`${f.role}-${f.file}`}>
                    <td>{f.role}</td>
                    <td className="num">{fmtBytes(f.bytes)}</td>
                    <td>{f.codec}</td>
                    <td className="mono" title={f.sha256}>
                      {f.sha256 ? f.sha256.slice(0, 10) : "—"}
                    </td>
                    <td>{f.verified ? "yes" : "no"}</td>
                    <td>
                      {url ? (
                        <a href={url} download={`${rec.id}-${f.role}.${f.codec ?? "bin"}`}>
                          Download
                        </a>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="muted">{rec.state === "deleted" ? "Files were removed with the recording." : "No files yet."}</p>
      )}
    </div>
  );
}

