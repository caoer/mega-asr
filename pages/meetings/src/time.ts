// Every time on the page renders in the viewer's chosen zone; until they
// choose, the page config's `tz`, else the device's zone.

export const DEVICE_TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

export const ZONES: { tz: string; label: string }[] = [
  { tz: "UTC", label: "UTC" },
  { tz: "Europe/London", label: "London" },
  { tz: "America/New_York", label: "New York" },
  { tz: "America/Los_Angeles", label: "Los Angeles" },
  { tz: "Asia/Shanghai", label: "Shanghai" },
  { tz: "Asia/Taipei", label: "Taipei" },
  { tz: "Asia/Tokyo", label: "Tokyo" },
];

const cache = new Map<string, Intl.DateTimeFormat>();
function fmt(tz: string, opts: Intl.DateTimeFormatOptions, locale = "en-GB"): Intl.DateTimeFormat {
  const k = `${locale}|${tz}|${JSON.stringify(opts)}`;
  let f = cache.get(k);
  if (!f) {
    f = new Intl.DateTimeFormat(locale, { timeZone: tz, ...opts });
    cache.set(k, f);
  }
  return f;
}

export function parseTime(v: string | number | undefined | null): Date | null {
  if (v === undefined || v === null || v === "") return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

function parts(d: Date, tz: string) {
  const p = fmt(tz, { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" }, "en-CA").formatToParts(d);
  const get = (t: string) => Number(p.find((x) => x.type === t)?.value ?? 0);
  return { y: get("year"), mo: get("month"), d: get("day"), h: get("hour"), mi: get("minute"), s: get("second") };
}

/** YYYY-MM-DD of d in tz — the day a recording belongs to. */
export function dayKey(d: Date, tz: string): string {
  const p = parts(d, tz);
  return `${p.y}-${String(p.mo).padStart(2, "0")}-${String(p.d).padStart(2, "0")}`;
}

/** Minutes since local midnight in tz. */
export function minuteOfDay(d: Date, tz: string): number {
  const p = parts(d, tz);
  return p.h * 60 + p.mi + p.s / 60;
}

export const fmtClock = (d: Date, tz: string) => fmt(tz, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(d);

/** "Mon 6 Jan" (and the year when asked). */
export function fmtDay(d: Date, tz: string, withYear = false): string {
  const p = fmt(tz, { weekday: "short", day: "numeric", month: "short", year: "numeric" }, "en-US").formatToParts(d);
  const get = (t: string) => p.find((x) => x.type === t)?.value ?? "";
  return `${get("weekday")} ${get("day")} ${get("month")}${withYear ? ` ${get("year")}` : ""}`;
}

export function fmtDateTime(d: Date, tz: string): string {
  return `${fmtDay(d, tz, true)}, ${fmtClock(d, tz)}`;
}

/** "GMT+5:30" for the zone at that instant. */
export function tzOffset(tz: string, at = new Date()): string {
  const p = fmt(tz, { timeZoneName: "shortOffset" }, "en-US").formatToParts(at);
  return p.find((x) => x.type === "timeZoneName")?.value ?? tz;
}

export function zoneLabel(tz: string): string {
  return ZONES.find((z) => z.tz === tz)?.label ?? tz.split("/").pop()!.replace(/_/g, " ");
}

/** Duration as "1 h 24 min", "20 min", "43 s". */
export function fmtDur(s: number | undefined): string {
  if (typeof s !== "number" || !Number.isFinite(s)) return "—";
  if (s < 60) return `${Math.round(s)} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  const r = m % 60;
  return r ? `${h} h ${r} min` : `${h} h`;
}

/** Position in a recording: "4:05", "1:02:10". */
export function fmtPos(s: number): string {
  const t = Math.max(0, Math.floor(s));
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const sec = t % 60;
  return h ? `${h}:${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}` : `${m}:${String(sec).padStart(2, "0")}`;
}

/** A Date whose wall-clock fields in tz equal the datetime-local value. */
export function fromLocalInput(v: string, tz: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(v);
  if (!m) return null;
  const guess = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
  const p = parts(new Date(guess), tz);
  const asUTC = Date.UTC(p.y, p.mo - 1, p.d, p.h, p.mi, p.s);
  return new Date(guess - (asUTC - guess));
}

export function toLocalInput(d: Date, tz: string): string {
  const p = parts(d, tz);
  const z = (n: number) => String(n).padStart(2, "0");
  return `${p.y}-${z(p.mo)}-${z(p.d)}T${z(p.h)}:${z(p.mi)}`;
}

/** "just now", "3 min ago", "2 h ago". */
export function ago(d: Date, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - d.getTime()) / 1000));
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}
