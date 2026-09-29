import { useEffect, useRef, useState } from "react";
import { href, lastListHref, navigate, useRoute } from "./route";
import { setTheme, setTz, useStore, type Theme } from "./store";
import { ago, tzOffset, zoneLabel, ZONES } from "./time";
import { Toasts } from "./ui";
import { List } from "./List";
import { Detail } from "./Detail";
import { ContributorPage, ShareDialog, UploadDialog } from "./Upload";
import { ProposalsDialog } from "./Proposals";
import { Overview } from "./Overview";
import { People } from "./People";

let listScroll = 0;

export function App() {
  const route = useRoute();
  const loadError = useStore((s) => s.loadError);
  const loaded = useStore((s) => s.loaded);
  const contributor = useStore((s) => s.contributor);
  const section = route.path[0] ?? "";
  const detailId = section === "m" ? route.path[1] : undefined;
  const mainRef = useRef<HTMLElement>(null);
  const view = detailId ? `m/${detailId}` : route.path.join("/");

  // The full list keeps its place: leaving it remembers the scroll, coming back restores it.
  const prev = useRef(view);
  useEffect(() => {
    const was = prev.current;
    prev.current = view;
    if (was === view) return;
    if (view === "") requestAnimationFrame(() => window.scrollTo(0, listScroll));
    else window.scrollTo(0, 0);
  }, [view]);
  useEffect(() => {
    const onScroll = () => {
      if (prev.current === "") listScroll = window.scrollY;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  return (
    <>
      <button type="button" className="skip" onClick={() => mainRef.current?.focus()}>
        Skip to content
      </button>
      <TopBar contributor={contributor} />
      <main id="main" ref={mainRef} tabIndex={-1} data-section={section || "list"}>
        {loadError ? (
          <div className="page-msg">
            <h1>The meetings did not load</h1>
            <p>{loadError} Reload the page; if it persists, open your share link again.</p>
          </div>
        ) : !loaded ? (
          <div className="page-msg" aria-busy="true">
            <p>Loading meetings…</p>
          </div>
        ) : contributor ? (
          <ContributorPage />
        ) : section === "overview" ? (
          <Overview />
        ) : section === "people" ? (
          <People slug={route.path[1]} />
        ) : (
          <div className="split" data-open={detailId ? "" : undefined}>
            <List selectedId={detailId} />
            {detailId ? <Detail id={detailId} /> : null}
          </div>
        )}
      </main>
      {!contributor && loaded ? <Footer /> : null}
      {!contributor ? <ProposalsDialog /> : null}
      {!contributor ? <BottomNav /> : null}
      <Toasts />
    </>
  );
}

function useSections() {
  const route = useRoute();
  const section = route.path[0] ?? "";
  return [
    { id: "list", label: "Meetings", href: lastListHref(), current: section === "" || section === "m" },
    { id: "overview", label: "Overview", href: href(["overview"]), current: section === "overview" },
    { id: "people", label: "People", href: href(["people"]), current: section === "people" },
  ];
}

function Sections() {
  const items = useSections();
  return (
    <nav className="sections" aria-label="Sections">
      {items.map((i) => (
        <a key={i.id} href={i.href} aria-current={i.current ? "page" : undefined}>
          {i.label}
        </a>
      ))}
    </nav>
  );
}

function BottomNav() {
  const items = useSections();
  return (
    <nav className="bottom-nav" aria-label="Sections">
      {items.map((i) => (
        <a key={i.id} href={i.href} aria-current={i.current ? "page" : undefined}>
          <span>{i.label}</span>
        </a>
      ))}
    </nav>
  );
}

function Mark() {
  // The product's mark is its signature: a day ruler with a meeting on it.
  return (
    <svg className="mark" width="28" height="16" viewBox="0 0 28 16" aria-hidden="true">
      <rect x="0.5" y="6.5" width="27" height="3" rx="1.5" fill="none" stroke="currentColor" opacity=".45" />
      <rect x="9" y="4" width="7" height="8" rx="1.5" fill="var(--cobalt)" />
    </svg>
  );
}

function TopBar({ contributor }: { contributor: boolean }) {
  const route = useRoute();
  const tz = useStore((s) => s.tz);
  const theme = useStore((s) => s.theme);
  const [upload, setUpload] = useState(false);
  const [share, setShare] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);
  const onList = !route.path[0] || route.path[0] === "m";
  const q = onList ? route.query.get("q") ?? "" : "";

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) && !t.isContentEditable) {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // On the list and on a meeting the search narrows the index in place.
  const setQ = (v: string) => {
    const base = onList ? new URLSearchParams(route.query) : new URLSearchParams(lastListHref().split("?")[1] ?? "");
    if (v) base.set("q", v);
    else base.delete("q");
    navigate(href(onList ? route.path : [], base), onList);
  };

  const zones = ZONES.some((z) => z.tz === Intl.DateTimeFormat().resolvedOptions().timeZone)
    ? ZONES
    : [...ZONES, { tz: Intl.DateTimeFormat().resolvedOptions().timeZone, label: `${zoneLabel(Intl.DateTimeFormat().resolvedOptions().timeZone)} (this device)` }];
  const nextTheme: Record<Theme, Theme> = { system: "light", light: "dark", dark: "system" };

  return (
    <header className="topbar">
      <div className="topbar-in">
        <a className="brand" href={contributor ? href([]) : lastListHref()}>
          <Mark />
          <span>Meetings</span>
        </a>
        {!contributor ? <Sections /> : <span />}
        <div className="find-row">
        {!contributor ? (
          <div className="search">
            <svg width="15" height="15" viewBox="0 0 16 16" aria-hidden="true">
              <circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" strokeWidth="1.6" />
              <path d="M11 11l3.5 3.5" stroke="currentColor" strokeWidth="1.6" />
            </svg>
            <input
              ref={searchRef}
              type="search"
              name="q"
              autoComplete="off"
              spellCheck={false}
              aria-label="Search meetings"
              placeholder="Search meetings, people…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape" && q) setQ("");
              }}
            />
            <kbd aria-hidden="true">/</kbd>
          </div>
        ) : (
          <div className="topbar-note">Upload link</div>
        )}
        <label className="tz">
          <span className="tz-cap">Times in</span>
          <select value={tz} onChange={(e) => setTz(e.target.value)} aria-label="Time zone">
            {zones.map((z) => (
              <option key={z.tz} value={z.tz}>
                {z.label} · {tzOffset(z.tz)}
              </option>
            ))}
          </select>
        </label>
        </div>
        <div className="tools">
          <button type="button" className="icon-btn" onClick={() => setTheme(nextTheme[theme])} aria-label={`Theme: ${theme}. Switch to ${nextTheme[theme]}`} title={`Theme: ${theme}`}>
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.5" />
              {theme === "system" ? <path d="M8 2a6 6 0 0 1 0 12z" fill="currentColor" /> : theme === "dark" ? <circle cx="8" cy="8" r="4" fill="currentColor" /> : null}
            </svg>
          </button>
          {!contributor ? (
            <>
              <button type="button" className="btn ghost share-btn" onClick={() => setShare(true)} aria-label="Upload Link for Colleagues">
                <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M6.5 9.5l3-3M5 7.5L3.8 8.7a2.5 2.5 0 0 0 3.5 3.5L8.5 11M11 8.5l1.2-1.2a2.5 2.5 0 0 0-3.5-3.5L7.5 5" fill="none" stroke="currentColor" strokeWidth="1.5" />
                </svg>
                <span>Upload Link</span>
              </button>
              <button type="button" className="btn primary" onClick={() => setUpload(true)}>
                <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M8 12V3M4 7l4-4 4 4M3 14h10" fill="none" stroke="currentColor" strokeWidth="1.6" />
                </svg>
                <span>Upload</span>
              </button>
            </>
          ) : null}
        </div>
      </div>
      {!contributor ? (
        <>
          <UploadDialog open={upload} onClose={() => setUpload(false)} />
          <ShareDialog open={share} onClose={() => setShare(false)} />
        </>
      ) : null}
    </header>
  );
}

function Footer() {
  const rows = useStore((s) => s.rows);
  const lastLoad = useStore((s) => s.lastLoad);
  const pollError = useStore((s) => s.pollError);
  const live = rows.filter((r) => r.rec.state !== "deleted").length;
  return (
    <footer className="footer">
      <p role={pollError ? "alert" : undefined}>
        {pollError ? (
          <>
            {pollError} Showing what was read {lastLoad ? ago(lastLoad) : "earlier"}.
          </>
        ) : (
          <>
            {live} {live === 1 ? "recording" : "recordings"}
            {lastLoad ? ` · read ${ago(lastLoad)}` : ""}
          </>
        )}
      </p>
    </footer>
  );
}
