// The ASR-quality view: Feishu's minute beside our ASR, one row per Feishu
// turn. The strip across the top is the whole meeting coloured by agreement;
// the legend is the highlight switch; weak rows can be stepped through.
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { loadSegments, transcriptSources } from "./store";
import type { Rec, Segment } from "./types";
import { alignRows, type Tok } from "./align";
import { speakerPalette } from "./model";
import { fmtPos } from "./time";
import { useSeek } from "./seek";
import { isPlaceholder, useSpeakersOf } from "./people/store";
import { SpeakerName } from "./player/SpeakerName";
import { href } from "./route";

function marked(toks: Tok[], cls: string, on: boolean): ReactNode[] {
  const out: ReactNode[] = [];
  let buf = "";
  let inMark = false;
  let k = 0;
  const flush = () => {
    if (buf) out.push(inMark && on ? <mark key={k++} className={cls}>{buf}</mark> : buf);
    buf = "";
  };
  for (const x of toks) {
    if (x.k !== null) {
      const miss = x.hit === false;
      if (miss !== inMark) {
        flush();
        inMark = miss;
      }
    }
    buf += x.t;
  }
  flush();
  return out;
}

const pct = (x: number | undefined) => (typeof x === "number" ? `${(x <= 1.5 ? x * 100 : x).toFixed(1)}%` : null);
type RowFilter = "all" | "90" | "60";
const THRESH: Record<RowFilter, number> = { all: 2, "90": 0.9, "60": 0.6 };
/** Agreement → a colour step along the jade…cinnabar ramp. */
const band = (a: number | null) => (a === null ? "none" : a >= 0.9 ? "hi" : a >= 0.6 ? "mid" : "low");

export function Alignment({ rec }: { rec: Rec }) {
  const src = transcriptSources(rec);
  const seek = useSeek();
  const speakers = useSpeakersOf(rec.id);
  const [data, setData] = useState<{ ref: Segment[]; ours: Segment[] } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [showMiss, setShowMiss] = useState(true);
  const [showExtra, setShowExtra] = useState(true);
  const [raw, setRaw] = useState(false);
  const [filter, setFilter] = useState<RowFilter>("all");
  const [cursor, setCursor] = useState<number | null>(null);
  const gridRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let live = true;
    if (!src.ours || !src.feishu) return;
    Promise.all([loadSegments(src.feishu), loadSegments(src.ours)]).then(
      ([ref, ours]) => live && setData({ ref, ours }),
      (e: Error) => live && setErr(e.message),
    );
    return () => {
      live = false;
    };
  }, [src.ours?.file, src.feishu?.file]);

  const result = useMemo(() => (data ? alignRows(data.ref, data.ours, raw) : null), [data, raw]);
  const palette = useMemo(() => speakerPalette((data?.ref ?? []).map((s) => s.speaker || "Unknown")), [data]);
  const oursName = useMemo(() => new Map(speakers.map((s) => [s.ref.label, s.display])), [speakers]);
  const dur = Math.max(rec.duration_s ?? 0, data?.ref.length ? data.ref[data.ref.length - 1].end : 0) || 1;
  const rows = result ? result.rows.map((r, i) => ({ ...r, i })).filter((r) => filter === "all" || (r.agree !== null && r.agree < THRESH[filter])) : [];
  const weak = result ? result.rows.map((r, i) => ({ r, i })).filter(({ r }) => r.agree !== null && r.agree < 0.6).map(({ i }) => i) : [];
  const counts = result ? { hi: result.rows.filter((r) => band(r.agree) === "hi").length, mid: result.rows.filter((r) => band(r.agree) === "mid").length, low: weak.length } : null;

  const go = (i: number) => {
    if (!result) return;
    setCursor(i);
    const row = result.rows[i];
    seek(row.ref.start);
    if (filter !== "all" && !(row.agree !== null && row.agree < THRESH[filter])) setFilter("all");
    requestAnimationFrame(() => gridRef.current?.querySelector<HTMLElement>(`[data-row="${i}"]`)?.scrollIntoView({ block: "center" }));
  };
  const step = (dir: 1 | -1) => {
    if (!weak.length) return;
    const from = cursor ?? (dir === 1 ? -1 : result!.rows.length);
    const next = dir === 1 ? weak.find((i) => i > from) ?? weak[0] : [...weak].reverse().find((i) => i < from) ?? weak[weak.length - 1];
    go(next);
  };
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) return;
      if (e.key === "n") step(1);
      else if (e.key === "p") step(-1);
      else return;
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  if (!src.ours || !src.feishu) return <p className="muted">No Feishu minute to compare with.</p>;
  const s = rec.scores ?? {};
  const scored = [
    ["CER", pct(s.cer)],
    ["cpCER", pct(s.cpcer)],
    ["Offset", typeof s.offset_s === "number" ? `${s.offset_s >= 0 ? "+" : ""}${s.offset_s.toFixed(2)} s` : null],
    ["Drift", typeof s.drift_ppm === "number" ? `${s.drift_ppm.toFixed(0)} ppm` : null],
  ] as const;
  const hasScores = scored.some(([, v]) => v);

  return (
    <div className="align">
      <div className="al-summary">
        <div className="al-score">
          <span className="al-score-v num">{result?.overall != null ? `${Math.round(result.overall * 100)}%` : result ? "—" : "…"}</span>
          <span className="al-score-k">agreement — Feishu’s characters our ASR also has, in order</span>
        </div>
        {counts ? (
          <div className="al-bands num">
            <span data-band="hi">{counts.hi} rows ≥ 90%</span>
            <span data-band="mid">{counts.mid} rows 60–90%</span>
            <span data-band="low">{counts.low} rows under 60%</span>
          </div>
        ) : null}
        <div className="al-scored small">
          {hasScores ? scored.map(([k, v]) => (v ? <span key={k}>{k} <b className="num">{v}</b></span> : null)) : <span className="muted">CER, cpCER, offset and drift: not scored for this recording.</span>}
        </div>
      </div>

      {result ? (
        <div className="al-strip" role="group" aria-label="The meeting coloured by agreement; choose a moment to jump there">
          {result.rows.map((r, i) => (
            <button
              key={i}
              type="button"
              className="al-tick"
              data-band={band(r.agree)}
              data-cur={cursor === i ? "" : undefined}
              style={{ left: `${(r.ref.start / dur) * 100}%`, width: `${Math.max(0.35, ((r.ref.end - r.ref.start) / dur) * 100)}%` }}
              aria-label={`${fmtPos(r.ref.start)}, ${r.agree === null ? "no text" : `${Math.round(r.agree * 100)}% agreement`}`}
              onClick={() => go(i)}
            />
          ))}
        </div>
      ) : null}

      <div className="al-controls">
        <div className="al-legend" role="group" aria-label="Highlight">
          <button type="button" className="al-key" aria-pressed={showMiss} onClick={() => setShowMiss(!showMiss)}>
            <mark className="miss swatch" /> In Feishu, not ours
          </button>
          <button type="button" className="al-key" aria-pressed={showExtra} onClick={() => setShowExtra(!showExtra)}>
            <mark className="extra swatch" /> In ours, not Feishu
          </button>
        </div>
        <div className="segs" role="group" aria-label="Rows">
          {(["all", "90", "60"] as RowFilter[]).map((f) => (
            <button key={f} type="button" aria-pressed={filter === f} onClick={() => setFilter(f)}>
              {f === "all" ? "All rows" : `Under ${f}%`}
            </button>
          ))}
        </div>
        {src.ours.role === "segments" ? (
          <div className="segs" role="group" aria-label="Our text">
            <button type="button" aria-pressed={!raw} onClick={() => setRaw(false)}>
              Cleaned
            </button>
            <button type="button" aria-pressed={raw} onClick={() => setRaw(true)}>
              Raw ASR
            </button>
          </div>
        ) : null}
        <div className="al-step">
          <button type="button" className="btn ghost sm" onClick={() => step(-1)} disabled={!weak.length} aria-label="Previous weak row">
            ↑
          </button>
          <button type="button" className="btn ghost sm" onClick={() => step(1)} disabled={!weak.length}>
            Next Weak Row ↓
          </button>
          <span className="muted small" aria-hidden="true">
            <kbd>n</kbd> <kbd>p</kbd>
          </span>
        </div>
      </div>

      {err ? (
        <p className="err">Could not load both transcripts: {err}</p>
      ) : !result ? (
        <p className="muted">Aligning both transcripts…</p>
      ) : (
        <div className="al-grid" role="table" aria-label="Feishu’s minute beside our ASR" ref={gridRef}>
          <div className="al-row al-head" role="row">
            <div role="columnheader">Time</div>
            <div role="columnheader">Feishu (reference)</div>
            <div role="columnheader">Our ASR</div>
          </div>
          {rows.length === 0 ? <p className="muted al-none">No rows under {filter}%.</p> : null}
          {rows.map((r) => {
            const ours = r.oursSpeakers.map((l) => oursName.get(l) ?? l);
            const theirs = oursName.get(r.ref.speaker) ?? r.ref.speaker;
            // Only two real names that disagree are worth a mark; "Speaker 3" vs "Speaker 1" is diarisation noise.
            const named = (x: string) => !!x && !isPlaceholder(x);
            const differs = named(theirs) && ours.length > 0 && ours.every(named) && !ours.includes(theirs) && !r.oursSpeakers.includes(r.ref.speaker);
            return (
              <div className="al-row" role="row" key={`${r.ref.start}-${r.i}`} data-row={r.i} data-band={band(r.agree)} data-cur={cursor === r.i ? "" : undefined}>
                <div className="al-time" role="cell">
                  <button type="button" className="ts" onClick={() => go(r.i)} aria-label={`Move the player to ${fmtPos(r.ref.start)}`}>
                    {fmtPos(r.ref.start)}
                  </button>
                  {r.agree !== null ? (
                    <span className={`agree${r.agree < 0.6 ? " low" : ""}`}>
                      <span className="agree-bar" aria-hidden="true">
                        <span style={{ transform: `scaleX(${r.agree})` }} />
                      </span>
                      {Math.round(r.agree * 100)}%
                    </span>
                  ) : null}
                </div>
                <div role="cell" data-col="Feishu" data-c={palette.get(r.ref.speaker || "Unknown") ?? 0}>
                  <div className="who">
                    <span className="dot" aria-hidden="true" />
                    {r.ref.speaker ? <SpeakerName recId={rec.id} label={r.ref.speaker} personHref={(p) => href(["people", p.slug])} compact /> : "Unknown"}
                  </div>
                  <p className="say">{marked(r.refToks, "miss", showMiss)}</p>
                </div>
                <div role="cell" data-col="Our ASR">
                  {r.ours.some((x) => x.k) ? (
                    <>
                      <div className="who">
                        {ours.join(", ") || "—"}
                        {differs ? <span className="spk-differs">speaker differs</span> : null}
                      </div>
                      <p className="say">{marked(r.ours, "extra", showExtra)}</p>
                    </>
                  ) : (
                    <span className="none">nothing recognised here</span>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
      <p className="note">Rows follow Feishu’s turns; our text sits where it matches. Agreement is a reading aid, not the scored CER. Any time, tick or row moves the player there.</p>
    </div>
  );
}
