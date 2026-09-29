// Overview: the whole archive at once, in the list's language — time of day
// on the 24-hour ruler, the series, topics and people. Every count opens the
// list, filtered.
import { useMemo } from "react";
import { href } from "./route";
import { useStore } from "./store";
import { STATES } from "./types";
import { dayKey, fmtDay, fmtDur, minuteOfDay, parseTime } from "./time";
import { startedOf, topicsOf, typesOf } from "./model";
import { HourScale, stateText } from "./ui";
import { shortName, usePeopleDirectory, useWaiting } from "./people/store";
import { openProposals, useOpenQuestions } from "./Proposals";

const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

export function Overview() {
  const rows = useStore((s) => s.rows);
  const tz = useStore((s) => s.tz);
  const vocab = useStore((s) => s.vocabulary);
  const people = usePeopleDirectory();
  const waiting = useWaiting();
  const asks = useOpenQuestions().length;

  const m = useMemo(() => {
    const live = rows.filter((r) => r.rec.state !== "deleted");
    const hours = live.reduce((n, r) => n + (r.rec.duration_s ?? 0), 0) / 3600;
    const byState: Record<string, number> = {};
    for (const r of rows) byState[r.rec.state] = (byState[r.rec.state] ?? 0) + 1;
    // The drain's wiki ingest runs, where it has mirrored one onto the record.
    const byIngest: Record<string, number> = {};
    for (const r of live) if (r.rec.ingest?.state) byIngest[r.rec.ingest.state] = (byIngest[r.rec.ingest.state] ?? 0) + 1;
    // Weekday rhythm: each meeting drawn on its weekday's ruler.
    const week: { left: number; width: number; id: string }[][] = WEEKDAYS.map(() => []);
    const wdFmt = new Intl.DateTimeFormat("en-US", { timeZone: tz, weekday: "short" });
    for (const r of live) {
      const d = startedOf(r.rec);
      if (!d) continue;
      const wd = WEEKDAYS.indexOf(wdFmt.format(d));
      const m0 = minuteOfDay(d, tz);
      week[wd]?.push({ left: (m0 / 1440) * 100, width: Math.min(100 - (m0 / 1440) * 100, ((r.rec.duration_s ?? 0) / 86400) * 100), id: r.rec.id });
    }
    // Weeks: hours per week, oldest first.
    const weeks = new Map<string, { start: Date; s: number; n: number }>();
    for (const r of live) {
      const d = startedOf(r.rec);
      if (!d) continue;
      const k = dayKey(new Date(d.getTime() - ((WEEKDAYS.indexOf(wdFmt.format(d)) + 7) % 7) * 86400000), tz);
      const w = weeks.get(k) ?? { start: parseTime(`${k}T12:00:00Z`)!, s: 0, n: 0 };
      w.s += r.rec.duration_s ?? 0;
      w.n++;
      weeks.set(k, w);
    }
    const series = vocab.closed.map((t) => {
      const list = live.filter((r) => typesOf(r.rec).includes(t));
      const last = list.map((r) => startedOf(r.rec)).filter(Boolean).sort((a, b) => b!.getTime() - a!.getTime())[0] ?? null;
      return { t, n: list.length, s: list.reduce((n, r) => n + (r.rec.duration_s ?? 0), 0), last };
    });
    const topics = new Map<string, number>();
    for (const r of live) for (const t of topicsOf(r.rec)) topics.set(t, (topics.get(t) ?? 0) + 1);
    return { live: live.length, hours, byState, byIngest, week, weeks: [...weeks.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([, w]) => w), series, topics: [...topics].sort((a, b) => b[1] - a[1]) };
  }, [rows, tz, vocab]);

  const linked = people.filter((e) => e.meetings.length);
  const unnamed = waiting.unnamed.reduce((n, u) => n + u.views.length, 0);
  const maxWeek = Math.max(1, ...m.weeks.map((w) => w.s));
  const maxSeries = Math.max(1, ...m.series.map((x) => x.n));

  return (
    <div className="overview">
      <header className="ov-head">
        <h1>Overview</h1>
        <p className="lede">What the team has recorded, when it meets, and who speaks. Every count opens the list.</p>
      </header>

      <div className="ov-stats">
        <a className="ov-stat" href={href([])}>
          <span className="num">{m.live}</span>
          <span>recordings</span>
        </a>
        <div className="ov-stat">
          <span className="num">{m.hours.toFixed(1)}</span>
          <span>hours of audio</span>
        </div>
        <a className="ov-stat" href={href(["people"])}>
          <span className="num">{linked.length}</span>
          <span>people with a wiki page spoke</span>
        </a>
        <a className="ov-stat" href={href(["people"], { v: "waiting" })} data-warn={unnamed ? "" : undefined}>
          <span className="num">{unnamed}</span>
          <span>speakers still unnamed</span>
        </a>
      </div>

      <div className="ov-grid">
        <section className="ov-card ov-rhythm">
          <h2>When the team meets</h2>
          <p className="muted small">Every meeting on its weekday’s 24 hours, in {tz.replace(/_/g, " ")} time. Darker where meetings stack.</p>
          <div className="rhythm">
            <span />
            <HourScale />
            <span />
            {m.week.map((bars, i) => (
              <div className="rhythm-row" key={WEEKDAYS[i]}>
                <span className="rhythm-day">{WEEKDAYS[i]}</span>
                <div className="ruler rhythm-ruler">
                  {bars.map((b) => (
                    <span key={b.id} className="ruler-bar stack" style={{ left: `${b.left}%`, width: `max(3px, ${b.width}%)` }} />
                  ))}
                </div>
                <span className="rhythm-n num">{bars.length}</span>
              </div>
            ))}
          </div>
        </section>

        <section className="ov-card">
          <h2>Hours per week</h2>
          <div className="weeks" role="img" aria-label={`Hours of meetings per week, ${m.weeks.length} weeks`}>
            {m.weeks.map((w) => (
              <div key={w.start.toISOString()} className="week" title={`Week of ${fmtDay(w.start, tz)}: ${fmtDur(w.s)}, ${w.n} meetings`}>
                <span className="week-bar" style={{ height: `${(w.s / maxWeek) * 100}%` }} />
              </div>
            ))}
          </div>
          <div className="weeks-axis muted small num">
            <span>{m.weeks[0] ? fmtDay(m.weeks[0].start, tz) : ""}</span>
            <span>{m.weeks.length ? fmtDay(m.weeks[m.weeks.length - 1].start, tz) : ""}</span>
          </div>
        </section>

        <section className="ov-card">
          <h2>Series</h2>
          <p className="muted small">Meeting types from the closed list.</p>
          <ul className="ov-bars">
            {m.series.map((x) => (
              <li key={x.t}>
                <a href={href([], { l: x.t })} data-zero={x.n ? undefined : ""}>
                  <span className="type">{x.t}</span>
                  <span className="ov-bar">
                    <i style={{ width: `${(x.n / maxSeries) * 100}%` }} />
                  </span>
                  <span className="num">{x.n}</span>
                  <span className="muted small">{x.last ? `last ${fmtDay(x.last, tz)}` : "none yet"}</span>
                </a>
              </li>
            ))}
          </ul>
        </section>

        <section className="ov-card">
          <h2>Topics</h2>
          <p className="muted small">The open list the agent keeps.</p>
          <div className="ov-cloud">
            {m.topics.map(([t, n]) => (
              <a key={t} className="label" data-kind="topic" href={href([], { l: t })}>
                <span className="label-body">
                  {t} <span className="num muted">{n}</span>
                </span>
              </a>
            ))}
          </div>
        </section>

        <section className="ov-card">
          <h2>Who speaks most</h2>
          <ul className="ov-people">
            {linked.slice(0, 10).map((e) => (
              <li key={e.person.slug}>
                <a href={href(["people", e.person.slug])}>
                  <span>{shortName(e.person)}</span>
                  <span className="ov-bar">
                    <i style={{ width: `${(e.meetings.length / Math.max(1, linked[0]?.meetings.length ?? 1)) * 100}%` }} />
                  </span>
                  <span className="num">{e.meetings.length}</span>
                </a>
              </li>
            ))}
          </ul>
          {waiting.named.length ? (
            <p className="muted small">
              Also named but without a wiki page: {waiting.named.slice(0, 3).map((n) => `${n.display} (${n.meetings.length})`).join("、")}.{" "}
              <a href={href(["people"], { v: "waiting" })}>Waiting for a name →</a>
            </p>
          ) : null}
        </section>

        <section className="ov-card">
          <h2>Pipeline</h2>
          <ul className="ov-states">
            {STATES.map((s) => (
              <li key={s}>
                <a href={href([], { s })} data-zero={m.byState[s] ? undefined : ""}>
                  <span className="state" data-s={s}>
                    {stateText(s)}
                  </span>
                  <span className="num">{m.byState[s] ?? 0}</span>
                </a>
              </li>
            ))}
          </ul>
          {Object.keys(m.byIngest).length ? (
            <>
              <h3 className="ov-sub">Wiki ingest</h3>
              <ul className="ov-states">
                {["queued", "running", "done", "partial", "failed"]
                  .filter((s) => m.byIngest[s])
                  .map((s) => (
                    <li key={s}>
                      <span>
                        <span className="state" data-s={s === "done" ? "ingested" : s === "failed" ? "failed" : "processing"}>
                          {s}
                        </span>
                        <span className="num">{m.byIngest[s]}</span>
                      </span>
                    </li>
                  ))}
              </ul>
            </>
          ) : null}
          {asks ? (
            <button type="button" className="btn ghost sm" onClick={openProposals}>
              {asks} Agent Asks Waiting
            </button>
          ) : null}
        </section>
      </div>
    </div>
  );
}
