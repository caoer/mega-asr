// A speaker's name, shown and named in place. The legend and every transcript
// turn render this; the people store decides what the label means.
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { isPlaceholder, nameSpeaker, RESOLVER_LIVE, shortName, unnameSpeaker, useKnownNames, usePeople, useSpeaker, type SpeakerRef, type WikiPerson } from "../people/store";
import { explain } from "../registry";
import { toast } from "../ui";

export type PersonHref = (p: WikiPerson) => string;

export function SpeakerName({ recId, label, personHref, compact }: { recId: string; label: string; personHref?: PersonHref; compact?: boolean }) {
  const ref = useMemo<SpeakerRef>(() => ({ rec: recId, label }), [recId, label]);
  const view = useSpeaker(ref);
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLSpanElement>(null);
  const unnamed = view.state === "unnamed";
  const title = view.state === "linked" && view.person ? `${view.person.name}${view.person.role ? ` — ${view.person.role}` : ""} · wiki people page` : view.state === "resolving" ? "The agent is finding or creating this person's page…" : view.state === "named" ? "Named; no wiki page yet" : "Not identified — click to name this speaker";

  return (
    <span className={`up-spk up-spk-${view.state}${compact ? " compact" : ""}`} ref={anchor}>
      <button type="button" className="up-spk-btn" onClick={() => setOpen((o) => !o)} aria-haspopup="dialog" aria-expanded={open} title={title}>
        <span className="up-spk-name">{view.display}</span>
        {view.state === "resolving" ? <span className="up-spk-mark resolving" aria-label="finding the person's page" /> : null}
        {view.state === "linked" ? <span className="up-spk-mark linked" aria-hidden="true" /> : null}
        {unnamed ? <span className="up-spk-cta">name…</span> : null}
      </button>
      {view.state === "linked" && view.person && personHref && !compact ? (
        <a className="up-spk-link" href={personHref(view.person)} title={`Open ${shortName(view.person)}'s page`} aria-label={`Open ${shortName(view.person)}'s page`}>
          ↗
        </a>
      ) : null}
      {open ? <NamePopover recRef={ref} current={view.display} state={view.state} onClose={() => setOpen(false)} anchor={anchor} /> : null}
    </span>
  );
}

/** One autocomplete option: a wiki person from the index, or a name already on a recording. */
type Option = { key: string; name: string; person?: WikiPerson };

function NamePopover({ recRef, current, state, onClose, anchor }: { recRef: SpeakerRef; current: string; state: string; onClose: () => void; anchor: React.RefObject<HTMLSpanElement | null> }) {
  const people = usePeople();
  const known = useKnownNames();
  const [busy, setBusy] = useState(false);
  const [q, setQ] = useState(isPlaceholder(current) ? "" : current);
  const input = useRef<HTMLInputElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const [active, setActive] = useState(0);

  useEffect(() => {
    input.current?.focus();
    const onDoc = (e: MouseEvent) => {
      if (!box.current?.contains(e.target as Node) && !anchor.current?.contains(e.target as Node)) onClose();
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [onClose, anchor]);

  const matches = useMemo<Option[]>(() => {
    const n = q.trim().toLowerCase();
    const hit = (a: string) => !n || a.toLowerCase().includes(n);
    const fromIndex = people.filter((p) => [p.name, ...p.aliases].some(hit)).map((p) => ({ key: `p:${p.slug}`, name: shortName(p), person: p }));
    const fromRecords = known.filter(hit).map((k) => ({ key: `n:${k}`, name: k }));
    return [...fromIndex, ...fromRecords].slice(0, 6);
  }, [people, known, q]);
  const exact = matches.find((o) => (o.person ? [o.person.name, shortName(o.person), ...o.person.aliases] : [o.name]).some((a) => a.toLowerCase() === q.trim().toLowerCase()));

  const run = async (write: () => Promise<void>, done: string) => {
    setBusy(true);
    try {
      await write();
      toast(done);
      onClose();
    } catch (e) {
      toast(`Could not save the name: ${explain(e)}`);
      setBusy(false);
    }
  };
  const commit = (name: string) => {
    const n = name.trim();
    const linked = people.some((p) => [p.name, shortName(p), ...p.aliases].some((a) => a.trim().toLowerCase() === n.toLowerCase()));
    void run(() => nameSpeaker(recRef, n), linked ? `${recRef.label} named ${n} — linked to their wiki page.` : `${recRef.label} named ${n}.`);
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!busy) commit(exact ? exact.name : q);
  };

  return (
    <div className="up-pop" role="dialog" aria-label={`Name ${current}`} ref={box} onKeyDown={(e) => e.key === "Escape" && onClose()}>
      <form onSubmit={submit}>
        <label className="up-pop-label" htmlFor={`nm-${recRef.label}`}>
          {isPlaceholder(current) ? `Who is ${current}?` : `Rename ${current}`}
        </label>
        <input
          id={`nm-${recRef.label}`}
          ref={input}
          className="up-pop-input"
          type="text"
          name="speaker"
          autoComplete="off"
          spellCheck={false}
          placeholder="Type a name — a wiki person or a new one…"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setActive(0);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setActive((a) => Math.min(matches.length - 1, a + 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setActive((a) => Math.max(0, a - 1));
            } else if (e.key === "Tab" && matches[active]) {
              e.preventDefault();
              setQ(matches[active].name);
            }
          }}
          aria-autocomplete="list"
          aria-controls="up-pop-list"
        />
        {matches.length ? (
          <ul className="up-pop-list" id="up-pop-list" role="listbox" aria-label="People in the wiki and names on recordings">
            {matches.map((o, i) => (
              <li key={o.key} role="option" aria-selected={i === active}>
                <button type="button" className={`up-pop-opt${i === active ? " active" : ""}`} disabled={busy} onMouseEnter={() => setActive(i)} onClick={() => commit(o.name)}>
                  <span className="nm">{o.person ? o.person.name : o.name}</span>
                  {o.person ? o.person.role ? <span className="role">{o.person.role}</span> : null : <span className="role">no wiki page yet</span>}
                </button>
              </li>
            ))}
          </ul>
        ) : q.trim() ? (
          <p className="up-pop-new">
            No wiki person called “{q.trim()}”. {RESOLVER_LIVE ? "Saving it asks the agent to find or create the page." : "The name is saved on the recording; it links to a wiki page once one exists."}
          </p>
        ) : null}
        <div className="up-pop-actions">
          <button type="submit" className="btn primary" disabled={!q.trim() || busy}>
            {busy ? "Saving…" : exact?.person ? `Link to ${exact.name}` : "Save name"}
          </button>
          {state !== "unnamed" ? (
            <button
              type="button"
              className="btn ghost"
              disabled={busy}
              onClick={() => void run(() => unnameSpeaker(recRef), `${recRef.label} unnamed.`)}
            >
              Clear
            </button>
          ) : null}
        </div>
      </form>
    </div>
  );
}
