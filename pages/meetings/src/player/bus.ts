// The playback bus: one playhead for the recording on screen. Tracks register
// their <audio>; a seek moves every track; the chosen track is the one heard.
// Module-level (not a React provider) so any view on the page — the alignment
// tab, a timestamp in a summary — can seek the player without being inside it.
// With no playable audio the bus keeps a virtual clock, so the transcript and
// the lanes still follow "playback" on every record on the page.
import { useSyncExternalStore } from "react";

export const RATES = [0.75, 1, 1.25, 1.5, 1.75, 2] as const;
export type Rate = (typeof RATES)[number];
const RATE_KEY = "meetings.player.rate";

export interface BusState {
  time: number;
  duration: number;
  playing: boolean;
  rate: Rate;
  muted: boolean;
  /** Track ids in registration order; `track` is the one heard. */
  tracks: string[];
  track: string | null;
  /** True when at least one track has audio; false → virtual clock. */
  hasAudio: boolean;
  ready: boolean;
}

function loadRate(): Rate {
  try {
    const v = Number(localStorage.getItem(RATE_KEY));
    return (RATES as readonly number[]).includes(v) ? (v as Rate) : 1;
  } catch {
    return 1;
  }
}

let state: BusState = { time: 0, duration: 0, playing: false, rate: loadRate(), muted: false, tracks: [], track: null, hasAudio: false, ready: false };
const els = new Map<string, HTMLAudioElement>();
const listeners = new Set<() => void>();
let raf: number | null = null;
let virtual: { t0: number; at: number } | null = null;

function emit(patch: Partial<BusState>) {
  state = { ...state, ...patch };
  listeners.forEach((l) => l());
}
const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => listeners.delete(l);
};
export const useBus = () => useSyncExternalStore(subscribe, () => state);
export const busState = () => state;

const heard = () => (state.track ? els.get(state.track) : undefined) ?? els.values().next().value;

/** A new recording: forget the old tracks; the duration comes from the record until audio says otherwise. */
export function resetBus(duration: number) {
  stopVirtual();
  for (const el of els.values()) el.pause();
  els.clear();
  emit({ time: 0, duration, playing: false, tracks: [], track: null, hasAudio: false, ready: false });
}

export function registerTrack(id: string, el: HTMLAudioElement | null) {
  if (!el) {
    const old = els.get(id);
    old?.pause();
    els.delete(id);
    const tracks = state.tracks.filter((t) => t !== id);
    emit({ tracks, track: state.track === id ? (tracks[0] ?? null) : state.track, hasAudio: els.size > 0 });
    return;
  }
  if (els.has(id)) return;
  els.set(id, el);
  el.playbackRate = state.rate;
  el.muted = state.muted;
  el.preservesPitch = true;
  if (state.time) el.currentTime = state.time;
  const onTime = () => {
    if (heard() !== el) return;
    if (raf !== null) return;
    raf = requestAnimationFrame(() => {
      raf = null;
      emit({ time: el.currentTime });
    });
  };
  const onMeta = () => {
    if (heard() === el && Number.isFinite(el.duration)) emit({ duration: Math.max(state.duration, el.duration), ready: true });
  };
  el.addEventListener("timeupdate", onTime);
  el.addEventListener("seeked", onTime);
  el.addEventListener("loadedmetadata", onMeta);
  el.addEventListener("play", () => {
    if (heard() !== el) el.pause();
    else emit({ playing: true });
  });
  el.addEventListener("pause", () => {
    if (heard() === el) emit({ playing: false });
  });
  el.addEventListener("ended", () => emit({ playing: false }));
  el.addEventListener("ratechange", () => {
    if (heard() === el && (RATES as readonly number[]).includes(el.playbackRate) && el.playbackRate !== state.rate) setRate(el.playbackRate as Rate);
  });
  const tracks = [...state.tracks, id];
  emit({ tracks, track: state.track ?? id, hasAudio: true, ready: state.ready || Number.isFinite(el.duration) });
  if (Number.isFinite(el.duration)) onMeta();
}

export function seek(t: number) {
  const to = Math.min(state.duration || Infinity, Math.max(0, t));
  for (const el of els.values()) {
    try {
      el.currentTime = to;
    } catch {
      /* metadata not loaded yet */
    }
  }
  if (virtual) virtual = { t0: to, at: performance.now() };
  emit({ time: to });
}

export function play() {
  const el = heard();
  if (el) {
    el.currentTime = state.time;
    void el.play().catch(() => {});
    return;
  }
  if (!state.duration) return;
  virtual = { t0: state.time, at: performance.now() };
  emit({ playing: true });
  tickVirtual();
}
export function pause() {
  const el = heard();
  if (el) el.pause();
  stopVirtual();
  emit({ playing: false });
}
export const toggle = () => (state.playing ? pause() : play());

function tickVirtual() {
  if (!virtual) return;
  const t = virtual.t0 + ((performance.now() - virtual.at) / 1000) * state.rate;
  if (t >= state.duration) {
    virtual = null;
    emit({ time: state.duration, playing: false });
    return;
  }
  emit({ time: t });
  raf = requestAnimationFrame(() => {
    raf = null;
    tickVirtual();
  });
}
function stopVirtual() {
  virtual = null;
  if (raf !== null) {
    cancelAnimationFrame(raf);
    raf = null;
  }
}

export function setRate(rate: Rate) {
  for (const el of els.values()) el.playbackRate = rate;
  if (virtual) virtual = { t0: state.time, at: performance.now() };
  try {
    localStorage.setItem(RATE_KEY, String(rate));
  } catch {
    /* private mode */
  }
  emit({ rate });
}
export function stepRate(dir: 1 | -1) {
  const i = RATES.indexOf(state.rate);
  const next = RATES[Math.min(RATES.length - 1, Math.max(0, i + dir))];
  if (next !== state.rate) setRate(next);
}

export function setMuted(muted: boolean) {
  for (const el of els.values()) el.muted = muted;
  emit({ muted });
}

/** Switch the track that is heard; position and play state carry over. */
export function chooseTrack(id: string) {
  if (!els.has(id) || id === state.track) return;
  const was = state.playing;
  const prev = heard();
  prev?.pause();
  emit({ track: id });
  const el = els.get(id)!;
  el.currentTime = state.time;
  if (Number.isFinite(el.duration)) emit({ duration: Math.max(state.duration, el.duration) });
  if (was) void el.play().catch(() => {});
}

export const nudge = (d: number) => seek(state.time + d);
