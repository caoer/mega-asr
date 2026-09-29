// People: each person is a wiki people page, read from the `people.index`
// record and the `person` on each named speaker. The page shows the profile
// read-only, the meetings they spoke in, and the speakers still waiting for a
// name. Before the index is published, the directory lists the names given
// to speakers.
import { useMemo, useState } from "react";
import { href, meetingHref, navigate, useRoute } from "./route";
import { useStore, type Row } from "./store";
import { fmtClock, fmtDay, fmtDur } from "./time";
import { displayTitle, labelKind, startedOf, topicsOf, typesOf } from "./model";
import { DayRuler } from "./ui";
import { pageURL, RESOLVER_LIVE, roleLine, shortName, usePeopleDirectory, useWaiting, type PersonEntry, type Waiting as WaitingLists } from "./people/store";

export function People({ slug }: { slug?: string }) {
  return slug ? <Profile slug={slug} /> : <Directory />;
}

/** The recordings' time span, for the activity strips. */
function useSpan(): [number, number] {
  const rows = useStore((s) => s.rows);
  return useMemo(() => {
    const ts = rows.map((r) => startedOf(r.rec)?.getTime()).filter((x): x is number => !!x);
    return [Math.min(...ts), Math.max(...ts)];
  }, [rows]);
}

/** Every meeting of one person as a tick along the recordings' months. */
function Activity({ meetings, span, big }: { meetings: Row[]; span: [number, number]; big?: boolean }) {
  const [a, b] = span;
  return (
    <div className={`activity${big ? " big" : ""}`} aria-hidden="true">
      {meetings.map((r) => {
        const t = startedOf(r.rec)?.getTime() ?? a;
        return <span key={r.key} style={{ left: `${((t - a) / Math.max(1, b - a)) * 100}%` }} />;
      })}
    </div>
  );
}

function Directory() {
  const route = useRoute();
  const tz = useStore((s) => s.tz);
  const dir = usePeopleDirectory();
  const waiting = useWaiting();
  const span = useSpan();
  const [q, setQ] = useState("");
  const view = route.query.get("v") === "waiting" ? "waiting" : "people";
  const index = useStore((s) => s.peopleIndex);
  const indexed = dir.filter((e) => !e.person.unindexed);
  const has = (text: string) => !q || text.toLowerCase().includes(q.toLowerCase());
  const shown = dir.filter((e) => has([e.person.name, ...e.person.aliases, roleLine(e.person)].join(" ")));
  // No index yet: the names on recordings stand in for the directory.
  const names = index ? [] : waiting.named.filter((n) => has(n.display));
  const spoke = dir.filter((e) => e.meetings.length);
  const unnamedCount = waiting.unnamed.reduce((n, u) => n + u.views.length, 0);

  return (
    <div className="people-page">
      <header className="pp-head">
        <h1>People</h1>
        {index ? (
          <p className="lede">
            Each person has a page in the wiki, sometimes one in each wiki. {indexed.length} {indexed.length === 1 ? "person" : "people"} in the people index; {spoke.length} of them spoke in these meetings. Naming a speaker with a name the index knows links it to that page.
          </p>
        ) : (
          <p className="lede">
            Each person is a page in the wiki. The people index is not published yet, so a speaker links to a page only once the back end sets it. Below: the names given to speakers on these recordings.
          </p>
        )}
      </header>

      <div className="pp-tabs" role="tablist" aria-label="People views">
        <button type="button" role="tab" aria-selected={view === "people"} onClick={() => navigate(href(["people"]), true)}>
          People <span className="num">{dir.length + (index ? 0 : waiting.named.length)}</span>
        </button>
        <button type="button" role="tab" aria-selected={view === "waiting"} onClick={() => navigate(href(["people"], { v: "waiting" }), true)}>
          Waiting for a name <span className="num">{unnamedCount + waiting.named.length}</span>
        </button>
      </div>

      {view === "people" ? (
        <>
          <div className="pp-tools">
            <input type="search" name="people-q" autoComplete="off" aria-label="Find a person" placeholder="Find by name, alias, org…" value={q} onChange={(e) => setQ(e.target.value)} />
            <span className="muted small">
              Sorted by meetings. Timeline: {fmtDay(new Date(span[0]), tz, true)} – {fmtDay(new Date(span[1]), tz, true)}.
            </span>
          </div>
          <ul className="pp-grid">
            {shown.map((e) => (
              <PersonCard key={e.person.slug} e={e} span={span} tz={tz} />
            ))}
            {names.map((n) => (
              <NameCard key={n.display} n={n} span={span} tz={tz} />
            ))}
          </ul>
          {!shown.length && !names.length ? <p className="muted">{q ? "Nobody matches." : "No speaker is named yet."}</p> : null}
        </>
      ) : (
        <Waiting tz={tz} />
      )}
    </div>
  );
}

function PersonCard({ e, span, tz }: { e: PersonEntry; span: [number, number]; tz: string }) {
  const p = e.person;
  return (
    <li className="pcard" data-empty={e.meetings.length ? undefined : ""}>
      <a className="pcard-link" href={href(["people", p.slug])}>
        <span className="pcard-name">{shortName(p)}</span>
        {p.name !== shortName(p) ? <span className="pcard-full">{p.name}</span> : null}
      </a>
      <div className="pcard-meta" title={roleLine(p) || undefined}>{roleLine(p) || (p.unindexed ? "Not in the people index yet" : "—")}</div>
      <Activity meetings={e.meetings.map((m) => m.row)} span={span} />
      <div className="pcard-foot">
        <span className="num">
          {e.meetings.length} {e.meetings.length === 1 ? "meeting" : "meetings"}
        </span>
        {e.last ? <span className="muted">last {fmtDay(e.last, tz)}</span> : <span className="muted">not in these meetings</span>}
      </div>
    </li>
  );
}

/** A name on recordings that no wiki page holds yet; it opens the list filtered to it. */
function NameCard({ n, span, tz }: { n: WaitingLists["named"][number]; span: [number, number]; tz: string }) {
  const last = n.meetings.map((r) => startedOf(r.rec)).filter((d): d is Date => !!d).sort((a, b) => b.getTime() - a.getTime())[0];
  return (
    <li className="pcard">
      <a className="pcard-link" href={href([], { p: n.display })}>
        <span className="pcard-name">{n.display}</span>
      </a>
      <div className="pcard-meta">{n.state === "resolving" ? "agent finding the wiki page…" : "no wiki page yet"}</div>
      <Activity meetings={n.meetings} span={span} />
      <div className="pcard-foot">
        <span className="num">
          {n.meetings.length} {n.meetings.length === 1 ? "meeting" : "meetings"}
        </span>
        {last ? <span className="muted">last {fmtDay(last, tz)}</span> : null}
      </div>
    </li>
  );
}

function Waiting({ tz }: { tz: string }) {
  const w = useWaiting();
  return (
    <div className="waiting">
      <section>
        <h2>Names with no wiki page yet</h2>
        <p className="muted small">
          {RESOLVER_LIVE
            ? "The recording names them but no people page matches yet. The back end (the drain) finds or creates the page and links it."
            : "The recording names them but no people page matches. They link once the page exists in the wiki and the back end sets it."}
        </p>
        {w.named.length === 0 ? <p className="muted">Every name links to a page.</p> : null}
        <ul className="w-names">
          {w.named.map((n) => (
            <li key={n.display}>
              <span className="w-name">{n.display}</span>
              <span className="num muted">
                {n.meetings.length} {n.meetings.length === 1 ? "meeting" : "meetings"}
              </span>
              {n.state === "resolving" ? <span className="resolving">agent finding or creating the page…</span> : <span className="muted small">no wiki page yet</span>}
            </li>
          ))}
        </ul>
      </section>
      <section>
        <h2>Unnamed speakers</h2>
        <p className="muted small">Name them in the recording’s player, where you can hear who is talking.</p>
        <ul className="w-meetings">
          {w.unnamed.map(({ row, views }) => {
            const d = startedOf(row.rec);
            return (
              <li key={row.key}>
                <a href={meetingHref(row.rec.id, new URLSearchParams())} className="w-title">
                  {displayTitle(row.rec)}
                </a>
                <span className="muted small num">{d ? `${fmtDay(d, tz)} ${fmtClock(d, tz)}` : ""}</span>
                <span className="w-labels">
                  {views.map((v) => (
                    <span key={v.ref.label} className="spk" data-state="unnamed">
                      <span className="spk-name">{v.display}</span>
                    </span>
                  ))}
                </span>
              </li>
            );
          })}
        </ul>
      </section>
    </div>
  );
}

function Profile({ slug }: { slug: string }) {
  const tz = useStore((s) => s.tz);
  const dir = usePeopleDirectory();
  const span = useSpan();
  const e = dir.find((x) => x.person.slug === slug) ?? dir.find((x) => x.person.pages.some((pg) => pg.slug === slug));
  const stats = useMemo(() => {
    if (!e) return null;
    const types = new Map<string, number>();
    const topics = new Map<string, number>();
    const withWho = new Map<string, { entry: PersonEntry; n: number }>();
    for (const { row } of e.meetings) {
      for (const t of typesOf(row.rec)) types.set(t, (types.get(t) ?? 0) + 1);
      for (const t of topicsOf(row.rec)) if (labelKind(t) === "topic") topics.set(t, (topics.get(t) ?? 0) + 1);
      for (const o of dir) if (o !== e && o.meetings.some((m) => m.row === row)) withWho.set(o.person.slug, { entry: o, n: (withWho.get(o.person.slug)?.n ?? 0) + 1 });
    }
    const desc = (a: [string, number], b: [string, number]) => b[1] - a[1];
    return { types: [...types].sort(desc), topics: [...topics].sort(desc), withWho: [...withWho.values()].sort((a, b) => b.n - a.n).slice(0, 8), hours: e.meetings.reduce((n, m) => n + (m.row.rec.duration_s ?? 0), 0) };
  }, [e, dir]);

  if (!e || !stats)
    return (
      <div className="page-msg">
        <p>
          <a href={href(["people"])}>← People</a>
        </p>
        <h1>No wiki page “{slug}”</h1>
        <p>It is not in the people index, and no speaker links to it.</p>
      </div>
    );
  const p = e.person;
  const name = shortName(p);
  return (
    <article className="profile">
      <p className="back">
        <a href={href(["people"])}>
          <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
            <path d="M7.5 2L3.5 6l4 4" fill="none" stroke="currentColor" strokeWidth="1.6" />
          </svg>
          People
        </a>
      </p>
      <header className="pf-head">
        <h1>{name}</h1>
        {p.name !== name ? <p className="pf-full">{p.name}</p> : null}
        <p className="pf-meta">
          {roleLine(p)}
          {p.aliases.length ? <span className="muted"> · also {p.aliases.filter((a) => a !== name && a !== p.slug).slice(0, 5).join("、")}</span> : null}
        </p>
        <p className="pf-wiki">
          {p.pages.length ? (
            p.pages.map((pg, i) => {
              const url = pageURL(pg);
              return (
                <span key={`${pg.wiki}/${pg.slug}`}>
                  {i ? " · " : null}
                  {url ? (
                    <a href={url} target="_blank" rel="noopener">
                      {pg.wiki || pg.slug} ↗
                    </a>
                  ) : (
                    <span className="mono">{[pg.wiki, pg.slug].filter(Boolean).join(" · ")}</span>
                  )}
                </span>
              );
            })
          ) : (
            <span className="mono">{p.slug}</span>
          )}
          <span className="muted small"> {p.unindexed ? "Not in the people index yet; a speaker on a recording links here." : p.pages.length > 1 ? "Wiki pages of this person; read-only here." : "The profile lives in the wiki and is read-only here."}</span>
        </p>
      </header>

      <div className="pf-body">
        <div className="pf-main">
          <section className="pf-activity" aria-label="Meetings over time">
            <div className="pf-activity-head">
              <span>
                <span className="num">{e.meetings.length}</span> {e.meetings.length === 1 ? "meeting" : "meetings"} · <span className="num">{fmtDur(stats.hours)}</span>
              </span>
              <span className="muted small num">
                {fmtDay(new Date(span[0]), tz, true)} – {fmtDay(new Date(span[1]), tz, true)}
              </span>
            </div>
            <Activity meetings={e.meetings.map((m) => m.row)} span={span} big />
          </section>

          <section className="pf-profile">
            <h2>Profile</h2>
            {p.description ? <p className="pf-desc">{p.description}</p> : <p className="muted">The profile is on the wiki page.</p>}
          </section>

          <section className="pf-meetings">
            <h2>Meetings {name} spoke in</h2>
            {e.meetings.length === 0 ? <p className="muted">None on this page.</p> : null}
            <ul>
              {e.meetings.map(({ row }) => {
                const d = startedOf(row.rec);
                const stop = d && row.rec.duration_s ? new Date(d.getTime() + row.rec.duration_s * 1000) : null;
                return (
                  <li key={row.key} className="pm-row">
                    <span className="pm-when num">{d ? `${fmtDay(d, tz, d.getFullYear() !== new Date().getFullYear())}` : "—"}</span>
                    <span className="pm-type">
                      {typesOf(row.rec).map((t) => (
                        <span key={t} className="type">
                          {t}
                        </span>
                      ))}
                    </span>
                    <a className="pm-title" href={meetingHref(row.rec.id, new URLSearchParams())}>
                      {displayTitle(row.rec)}
                    </a>
                    <span className="pm-ruler">
                      <DayRuler rec={row.rec} tz={tz} />
                    </span>
                    <span className="pm-time num">
                      {d ? `${fmtClock(d, tz)}${stop ? `–${fmtClock(stop, tz)}` : ""}` : ""} · {fmtDur(row.rec.duration_s)}
                    </span>
                    <span className="pm-topics">
                      {topicsOf(row.rec).map((t) => (
                        <a key={t} className="label" data-kind="topic" href={href([], { p: name, l: t })}>
                          <span className="label-body">{t}</span>
                        </a>
                      ))}
                    </span>
                  </li>
                );
              })}
            </ul>
          </section>
        </div>

        <aside className="pf-side">
          <section>
            <h2>Meeting types</h2>
            {stats.types.length ? (
              <ul className="pf-counts">
                {stats.types.map(([t, n]) => (
                  <li key={t}>
                    <a href={href([], { p: name, l: t })}>
                      <span className="type">{t}</span>
                      <span className="num">{n}</span>
                    </a>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted small">No typed meetings.</p>
            )}
          </section>
          <section>
            <h2>Topics</h2>
            <ul className="pf-counts">
              {stats.topics.map(([t, n]) => (
                <li key={t}>
                  <a href={href([], { p: name, l: t })}>
                    <span className="topic-name">{t}</span>
                    <span className="num">{n}</span>
                  </a>
                </li>
              ))}
            </ul>
          </section>
          <section>
            <h2>Often with</h2>
            <ul className="pf-counts">
              {stats.withWho.map(({ entry, n }) => (
                <li key={entry.person.slug}>
                  <a href={href(["people", entry.person.slug])}>
                    <span>{shortName(entry.person)}</span>
                    <span className="num">{n}</span>
                  </a>
                </li>
              ))}
            </ul>
          </section>
          <p className="muted small">
            <a href={href([], { p: name })}>All {name}’s meetings in the list →</a>
          </p>
        </aside>
      </div>
    </article>
  );
}

