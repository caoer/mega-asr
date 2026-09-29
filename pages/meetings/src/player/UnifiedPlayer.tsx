// The recording player: transport, who-spoke-when, and the transcript, as one
// component. Every timestamp anywhere moves the one playhead; the transcript
// follows it; speakers are shown by name and named in place.
import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as RKeyboardEvent, type PointerEvent as RPointerEvent } from "react";
import { audioURL, canRead, loadSegments, transcriptSources } from "../store";
import { AUDIO_ROLES, ROLE_LABEL, type FileRef, type Rec, type Segment } from "../types";
import { fmtPos } from "../time";
import { fmtBytes } from "../ui";
import { speakerPalette } from "../model";
import { busState, chooseTrack, nudge, RATES, registerTrack, resetBus, seek, setMuted, setRate, stepRate, toggle, useBus, type Rate } from "./bus";
import { lanesOf, splitMatches, tickStep, turnsOf } from "./turns";
import { SpeakerName, type PersonHref } from "./SpeakerName";
import "./player.css";

export interface UnifiedPlayerProps {
  rec: Rec;
  /** Where a linked person's name goes (the People page); omitted → no link. */
  personHref?: PersonHref;
  /** Height cap of the docked transcript on desktop (CSS length). Default 56vh. */
  transcriptHeight?: string;
}

type SourceKey = "ours" | "feishu";

export function UnifiedPlayer({ rec, personHref, transcriptHeight }: UnifiedPlayerProps) {
  const bus = useBus();
  const rootRef = useRef<HTMLElement>(null);
  const tracks = useMemo(() => (rec.files ?? []).filter((f) => f.file && AUDIO_ROLES.includes(f.role)), [rec.files]);
  const src = transcriptSources(rec);

  // A new recording resets the bus; the record's duration stands until audio reports its own.
  useEffect(() => {
    resetBus(rec.duration_s ?? 0);
    return () => resetBus(0);
  }, [rec.id, rec.duration_s]);

  // Both transcripts, loaded once. Ours first; Feishu's minute when ours holds no speech.
  const [texts, setTexts] = useState<{ ours: Segment[] | null; feishu: Segment[] | null; loading: boolean; error: string | null }>({ ours: null, feishu: null, loading: true, error: null });
  useEffect(() => {
    let live = true;
    setTexts({ ours: null, feishu: null, loading: true, error: null });
    const load = (f?: FileRef) => (f ? loadSegments(f) : Promise.resolve(null));
    // Feishu's minute is a second opinion: a link that cannot read it still reads ours.
    Promise.all([load(src.ours), load(src.feishu).catch(() => null)]).then(
      ([ours, feishu]) => live && setTexts({ ours, feishu, loading: false, error: null }),
      (e: Error) => live && setTexts({ ours: null, feishu: null, loading: false, error: e.message }),
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [src.ours?.file, src.feishu?.file]);

  const [sourceChoice, setSourceChoice] = useState<SourceKey | null>(null);
  const source: SourceKey = sourceChoice ?? (texts.ours?.length ? "ours" : texts.feishu?.length ? "feishu" : "ours");
  const segs = source === "feishu" ? texts.feishu : texts.ours;
  const both = !!texts.ours?.length && !!texts.feishu?.length;

  const turns = useMemo(() => (segs ? turnsOf(segs) : []), [segs]);
  const lanes = useMemo(() => (segs ? lanesOf(segs) : []), [segs]);
  const palette = useMemo(() => speakerPalette((segs ?? []).map((s) => s.speaker || "Unknown")), [segs]);
  const dur = Math.max(bus.duration, rec.duration_s ?? 0, segs?.length ? segs[segs.length - 1].end : 0) || 1;

  const [find, setFind] = useState("");
  const [raw, setRaw] = useState(false);
  const [follow, setFollow] = useState(true);
  const hasRaw = source === "ours" && !!segs?.some((s) => s.raw && s.raw !== s.text);
  const shown = useMemo(() => {
    const n = find.trim().toLowerCase();
    if (!n) return turns;
    return turns.filter((t) => t.text.toLowerCase().includes(n) || (t.raw ?? "").toLowerCase().includes(n) || t.speaker.toLowerCase().includes(n));
  }, [turns, find]);
  const nowIdx = useMemo(() => {
    let at = -1;
    for (let i = 0; i < turns.length; i++) if (turns[i].start <= bus.time) at = i;
    return at;
  }, [turns, bus.time]);
  const now = nowIdx >= 0 ? turns[nowIdx] : null;

  // Follow playback: keep the current turn in view inside the docked transcript.
  const listRef = useRef<HTMLOListElement>(null);
  useEffect(() => {
    if (!follow || !bus.playing || !now) return;
    const li = listRef.current?.querySelector<HTMLElement>(`[data-start="${now.start}"]`);
    const box = listRef.current?.parentElement;
    if (!li || !box) return;
    const top = li.offsetTop - box.offsetTop;
    if (top < box.scrollTop || top + li.offsetHeight > box.scrollTop + box.clientHeight) box.scrollTo({ top: top - box.clientHeight * 0.3, behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  }, [now, follow, bus.playing]);

  // Keyboard, while the player has focus (a click inside gives it): Space,
  // arrows, speed, digits. Page-wide keys stay the page's (j/k walk meetings).
  const onKey = (e: RKeyboardEvent) => {
    if (isTyping(e.target) || e.metaKey || e.ctrlKey || e.altKey) return;
    if (handleKey(e.key, e.shiftKey, dur)) {
      e.preventDefault();
      e.stopPropagation();
    }
  };
  // Which tracks this link may read (a contributor reads only its own files).
  const [readable, setReadable] = useState<Record<string, boolean>>({});
  useEffect(() => {
    let live = true;
    setReadable({});
    for (const f of tracks) void canRead(f.file!).then((ok) => live && setReadable((r) => ({ ...r, [f.file!]: ok })));
    return () => {
      live = false;
    };
  }, [tracks]);
  const audioAvail = tracks.some((f) => readable[f.file!]);
  const probing = tracks.some((f) => readable[f.file!] === undefined);
  const heardName = bus.track ? (ROLE_LABEL[tracks.find((f) => f.file === bus.track)?.role ?? ""] ?? bus.track) : null;

  return (
    <section className="up" aria-label="Player" ref={rootRef} tabIndex={-1} onKeyDown={onKey} data-playing={bus.playing || undefined}>
      {tracks
        .filter((f) => readable[f.file!])
        .map((f) => (
          <HiddenTrack key={f.file} f={f} />
        ))}

      {/* ---- transport ---- */}
      <div className="up-bar">
        <button type="button" className="up-play" onClick={toggle} disabled={!audioAvail && !dur} aria-label={bus.playing ? "Pause" : "Play"} title={bus.playing ? "Pause (Space)" : "Play (Space)"}>
          {bus.playing ? (
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <path d="M3.5 2.5h3v11h-3zM9.5 2.5h3v11h-3z" fill="currentColor" />
            </svg>
          ) : (
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <path d="M4 2.5l9.5 5.5L4 13.5z" fill="currentColor" />
            </svg>
          )}
        </button>
        <span className="up-time num" aria-live="off">
          <span className="up-now">{fmtPos(bus.time)}</span>
          <span className="up-sep">/</span>
          <span>{fmtPos(dur)}</span>
        </span>
        <span className="up-skips">
          <button type="button" className="up-skip" onClick={() => nudge(-10)} aria-label="Back 10 seconds" title="Back 10 s (←)">
            −10
          </button>
          <button type="button" className="up-skip" onClick={() => nudge(10)} aria-label="Forward 10 seconds" title="Forward 10 s (→)">
            +10
          </button>
        </span>
        <RateControl rate={bus.rate} />
        {tracks.length > 1 ? (
          <div className="up-tracks" role="group" aria-label="Track heard">
            {tracks.map((f) => {
              const can = !!readable[f.file!];
              return (
                <button key={f.file} type="button" aria-pressed={bus.track === f.file} disabled={!can} title={can ? `${ROLE_LABEL[f.role] ?? f.role} · ${f.codec} · ${fmtBytes(f.bytes)}` : `${ROLE_LABEL[f.role] ?? f.role}: not readable with this link`} onClick={() => chooseTrack(f.file!)}>
                  {ROLE_LABEL[f.role] ?? f.role}
                </button>
              );
            })}
          </div>
        ) : null}
        <span className="up-grow" />
        {!tracks.length ? (
          <span className="up-note">{rec.state === "deleted" ? "Audio removed with the recording" : "No audio track — reading mode"}</span>
        ) : !audioAvail && !probing ? (
          <span className="up-note" title={tracks.map((f) => `${ROLE_LABEL[f.role] ?? f.role} · ${fmtBytes(f.bytes)}`).join(", ")}>
            Audio not readable with this link — reading mode
          </span>
        ) : !bus.hasAudio ? (
          <span className="up-note">Loading audio…</span>
        ) : heardName && tracks.length === 1 ? (
          <span className="up-note">{heardName} track</span>
        ) : null}
        <button type="button" className="up-mute" onClick={() => setMuted(!bus.muted)} aria-label={bus.muted ? "Unmute" : "Mute"} aria-pressed={bus.muted} title={bus.muted ? "Unmute (M)" : "Mute (M)"} disabled={!audioAvail}>
          {bus.muted ? (
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <path d="M2 6h3l4-3v10l-4-3H2z" fill="currentColor" />
              <path d="M11 6l3 4M14 6l-3 4" stroke="currentColor" strokeWidth="1.5" />
            </svg>
          ) : (
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <path d="M2 6h3l4-3v10l-4-3H2z" fill="currentColor" />
              <path d="M11 5.5a3.5 3.5 0 0 1 0 5M12.5 3.5a6 6 0 0 1 0 9" fill="none" stroke="currentColor" strokeWidth="1.4" />
            </svg>
          )}
        </button>
        <span className="up-keys" title="Keys while the player has focus: Space play/pause · ← → 10 s (Shift: 60 s) · , . speed · 0–9 jump · M mute" aria-hidden="true">
          Space · ← → · , .
        </span>
      </div>

      {/* ---- who spoke when ---- */}
      <div className="up-score">
        <div className="up-score-hd">
          <span className="up-h">Who spoke when</span>
          {segs ? (
            <span className="muted">
              {lanes.length} {lanes.length === 1 ? "voice" : "voices"} · from {source === "feishu" ? "Feishu’s minute" : "our ASR"} · click a lane to move there, a name to identify the person
            </span>
          ) : null}
        </div>
        {texts.loading ? (
          <p className="up-empty">Loading transcript…</p>
        ) : texts.error ? (
          <p className="up-empty err">Speaker timeline unavailable: {texts.error}</p>
        ) : !src.ours && !src.feishu ? (
          <p className="up-empty">Transcript pending — who spoke when appears once processing has run.</p>
        ) : !segs?.length ? (
          <p className="up-empty">No speech in this transcript, so there is no one to show.</p>
        ) : (
          <Lanes rec={rec} lanes={lanes} palette={palette} dur={dur} personHref={personHref} />
        )}
      </div>

      {/* ---- transcript, docked ---- */}
      {segs?.length ? (
        <div className="up-tr">
          <div className="up-tr-bar">
            {both ? (
              <div className="up-seg" role="group" aria-label="Transcript source">
                <button type="button" aria-pressed={source === "ours"} onClick={() => setSourceChoice("ours")}>
                  Our ASR
                </button>
                <button type="button" aria-pressed={source === "feishu"} onClick={() => setSourceChoice("feishu")}>
                  Feishu
                </button>
              </div>
            ) : (
              <span className="up-h">{source === "feishu" ? "Feishu’s minute" : "Transcript"}</span>
            )}
            <input className="up-find" type="search" name="find" autoComplete="off" aria-label="Find in transcript" placeholder="Find in transcript…" value={find} onChange={(e) => setFind(e.target.value)} />
            <label className="up-check">
              <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
            </label>
            {hasRaw ? (
              <label className="up-check">
                <input type="checkbox" checked={raw} onChange={(e) => setRaw(e.target.checked)} /> Raw ASR
              </label>
            ) : null}
            <span className="up-grow" />
            <span className="muted small num">
              {find.trim() ? `${shown.length} of ` : ""}
              {turns.length} turns
            </span>
          </div>
          <div className="up-turns-box" style={transcriptHeight ? { maxHeight: transcriptHeight } : undefined}>
            {shown.length === 0 ? <p className="up-empty">Nothing in the transcript matches “{find.trim()}”.</p> : null}
            <ol className="up-turns" ref={listRef}>
              {shown.map((t) => (
                <li key={`${t.start}-${t.speaker}`} className={`up-turn${t === now ? " now" : ""}`} data-start={t.start} data-c={palette.get(t.speaker || "Unknown") ?? 0}>
                  <button type="button" className="up-ts num" onClick={() => seek(t.start)} aria-label={`Move to ${fmtPos(t.start)}`}>
                    {fmtPos(t.start)}
                  </button>
                  <div className="up-turn-body">
                    <div className="up-who">
                      <span className="up-dot" aria-hidden="true" />
                      <SpeakerName recId={rec.id} label={t.speaker || "Unknown"} personHref={personHref} compact />
                    </div>
                    <p className="up-say" lang="zh-Hans">
                      {splitMatches(raw && t.raw ? t.raw : t.text, find).map((p, i) => (p.hit ? <mark key={i}>{p.t}</mark> : <span key={i}>{p.t}</span>))}
                    </p>
                  </div>
                </li>
              ))}
            </ol>
          </div>
        </div>
      ) : null}
    </section>
  );
}

// ---------------------------------------------------------------------------

function isTyping(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el) return false;
  const tag = el.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
}

/** Shared key map. Returns true when the key was taken. */
function handleKey(key: string, shift: boolean, dur: number): boolean {
  switch (key) {
    case " ":
      toggle();
      return true;
    case "ArrowLeft":
      nudge(shift ? -60 : -10);
      return true;
    case "ArrowRight":
      nudge(shift ? 60 : 10);
      return true;
    case "Home":
      seek(0);
      return true;
    case "End":
      seek(dur);
      return true;
    case "<":
    case ",":
      stepRate(-1);
      return true;
    case ">":
    case ".":
      stepRate(1);
      return true;
    case "m":
    case "M":
      setMuted(!busState().muted);
      return true;
    default:
      if (/^[0-9]$/.test(key)) {
        seek((Number(key) / 10) * dur);
        return true;
      }
      return false;
  }
}

/** The track streams from /f/, which answers Range requests, so it seeks anywhere. */
function HiddenTrack({ f }: { f: FileRef }) {
  // A stable ref callback: an inline one re-runs (null, then el) on every render and loops the bus.
  const ref = useCallback((el: HTMLAudioElement | null) => registerTrack(f.file!, el), [f.file]);
  return <audio ref={ref} src={audioURL(f.file!)} preload="metadata" className="up-audio" aria-label={`${ROLE_LABEL[f.role] ?? f.role} track`} />;
}

function RateControl({ rate }: { rate: Rate }) {
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (!box.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);
  return (
    <div className="up-rate" ref={box}>
      <button type="button" className="up-rate-btn num" onClick={() => setOpen((o) => !o)} aria-haspopup="listbox" aria-expanded={open} aria-label={`Speed ${rate}×`} title="Playback speed (< and > to step)">
        {rate}×
      </button>
      {open ? (
        <ul className="up-rate-list" role="listbox" aria-label="Playback speed">
          {RATES.map((r) => (
            <li key={r} role="option" aria-selected={r === rate}>
              <button
                type="button"
                className={`num${r === rate ? " active" : ""}`}
                onClick={() => {
                  setRate(r);
                  setOpen(false);
                }}
              >
                {r}×{r === 1 ? <span className="muted"> normal</span> : null}
              </button>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function Lanes({ rec, lanes, palette, dur, personHref }: { rec: Rec; lanes: ReturnType<typeof lanesOf>; palette: Map<string, number>; dur: number; personHref?: PersonHref }) {
  const bus = useBus();
  const trackRef = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<number | null>(null);
  const totalTalk = lanes.reduce((n, l) => n + l.talk, 0) || 1;
  const step = tickStep(dur);
  const ticks: number[] = [];
  for (let t = 0; t <= dur; t += step) ticks.push(t);
  const pct = (t: number) => `${(t / dur) * 100}%`;
  const at = (clientX: number) => {
    const el = trackRef.current;
    if (!el) return 0;
    const r = el.getBoundingClientRect();
    return Math.min(dur, Math.max(0, ((clientX - r.left) / r.width) * dur));
  };
  const onPointer = (e: RPointerEvent<HTMLDivElement>) => {
    if (e.buttons & 1) seek(at(e.clientX));
  };
  return (
    <div className="up-lanes">
      <div className="up-names">
        {lanes.map((l) => (
          <div className="up-name" key={l.label} data-c={palette.get(l.segs[0]?.speaker || "Unknown") ?? 0}>
            <span className="up-dot" aria-hidden="true" />
            {l.others ? <span className="up-spk-name muted" title={l.others.join(", ")}>{l.label}</span> : <SpeakerName recId={rec.id} label={l.label} personHref={personHref} />}
            <span className="up-talk num">{Math.round((l.talk / totalTalk) * 100)}%</span>
          </div>
        ))}
      </div>
      <div
        className="up-track"
        ref={trackRef}
        role="slider"
        tabIndex={0}
        aria-label="Position in the recording"
        aria-valuemin={0}
        aria-valuemax={Math.round(dur)}
        aria-valuenow={Math.round(bus.time)}
        aria-valuetext={fmtPos(bus.time)}
        onPointerDown={(e) => {
          try {
            e.currentTarget.setPointerCapture(e.pointerId);
          } catch {
            /* synthetic pointer */
          }
          seek(at(e.clientX));
        }}
        onPointerMove={(e) => {
          setHover(at(e.clientX));
          onPointer(e);
        }}
        onPointerLeave={() => setHover(null)}
      >
        {lanes.map((l) => (
          <div className="up-lane" key={l.label}>
            {l.segs.map((s, i) => (
              <span key={i} className="up-seg" data-c={palette.get(s.speaker || "Unknown") ?? 0} style={{ left: pct(s.start), width: `max(1.5px, ${pct(Math.max(0, s.end - s.start))})` }} />
            ))}
          </div>
        ))}
        <div className="up-ticks" aria-hidden="true">
          {ticks.map((t) => (
            <span key={t} style={{ left: pct(t), transform: t === 0 ? "none" : t / dur > 0.92 ? "translateX(-100%)" : undefined }}>
              {fmtPos(t)}
            </span>
          ))}
        </div>
        <span className="up-playhead" style={{ left: pct(bus.time) }} aria-hidden="true">
          <span className="up-ph-label num">{fmtPos(bus.time)}</span>
        </span>
        {hover !== null ? <span className="up-hover" style={{ left: pct(hover) }} aria-hidden="true" data-t={fmtPos(hover)} /> : null}
      </div>
    </div>
  );
}
