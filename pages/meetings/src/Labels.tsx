import { useId, useMemo, useRef, useState } from "react";
import { setLabels, useStore, type Row } from "./store";
import { explain } from "./registry";
import { addLabel, labelKind, labelText, removeLabel, typesOf, type LabelKind } from "./model";
import { LabelChip, toast } from "./ui";

const KIND_NAME: Record<LabelKind, string> = { type: "Meeting type · closed list", topic: "Topics · open list", person: "Person" };

export function LabelEditor({ row }: { row: Row }) {
  const vocab = useStore((s) => s.vocabulary);
  const vocabulary = useMemo(() => [...vocab.closed, ...vocab.open], [vocab]);
  const labels = row.rec.labels ?? [];
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  const options = useMemo(() => {
    const n = q.trim().toLowerCase();
    const pool = vocabulary.filter((l) => !labels.includes(l) && (!n || l.toLowerCase().includes(n) || labelText(l).toLowerCase().includes(n)));
    const order: LabelKind[] = ["type", "topic", "person"];
    // The editor never adds to the closed list; a new label joins the open list.
    // What was typed exactly comes first, so Enter takes it.
    const exact = (l: string) => (n && (l.toLowerCase() === n || labelText(l).toLowerCase() === n) ? 0 : 1);
    const sorted = pool.sort((a, b) => exact(a) - exact(b) || order.indexOf(labelKind(a)) - order.indexOf(labelKind(b)) || a.localeCompare(b, "zh-Hans-CN"));
    const out: { value: string; isNew?: boolean }[] = sorted.map((value) => ({ value }));
    const typed = q.trim();
    if (typed && !vocabulary.includes(typed) && !labels.includes(typed)) out.push({ value: typed, isNew: true });
    return out;
  }, [q, vocabulary, labels]);

  const add = (l: string) => {
    if (!l || labels.includes(l)) return;
    setLabels(row.rec.id, addLabel(l)).catch((e) => toast(`Labels not saved: ${explain(e)}`));
    setQ("");
    setActive(0);
    inputRef.current?.focus();
  };
  const remove = (l: string) => void setLabels(row.rec.id, removeLabel(l)).catch((e) => toast(`Labels not saved: ${explain(e)}`));
  // One type per recording: picking another type replaces it.
  const current = typesOf(row.rec)[0];
  const close = () => {
    setOpen(false);
    setQ("");
  };

  let lastKind: LabelKind | null = null;
  return (
    <div className="labels-edit">
      <div className="labels-row">
        {labels.map((l) => (
          <LabelChip key={l} label={l} agent={row.agentLabels.includes(l)} onRemove={() => remove(l)} />
        ))}
        <div className="combo">
          {open ? (
            <input
              ref={inputRef}
              autoFocus
              role="combobox"
              aria-expanded="true"
              aria-controls={listId}
              aria-autocomplete="list"
              aria-activedescendant={options[active] ? `${listId}-${active}` : undefined}
              aria-label="Add a label"
              name="label"
              autoComplete="off"
              spellCheck={false}
              placeholder="Type or pick a label…"
              value={q}
              onChange={(e) => {
                setQ(e.target.value);
                setActive(0);
              }}
              onBlur={close}
              onKeyDown={(e) => {
                if (e.key === "ArrowDown") {
                  e.preventDefault();
                  setActive((a) => Math.min(options.length - 1, a + 1));
                } else if (e.key === "ArrowUp") {
                  e.preventDefault();
                  setActive((a) => Math.max(0, a - 1));
                } else if (e.key === "Enter") {
                  e.preventDefault();
                  if (options[active]) add(options[active].value);
                } else if (e.key === "Escape") {
                  e.preventDefault();
                  close();
                } else if (e.key === "Backspace" && !q && labels.length) remove(labels[labels.length - 1]);
              }}
            />
          ) : (
            <button type="button" className="add-label" onClick={() => setOpen(true)}>
              <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
                <path d="M5 1v8M1 5h8" stroke="currentColor" strokeWidth="1.5" />
              </svg>
              Add Label
            </button>
          )}
          {open ? (
            <ul className="listbox" role="listbox" id={listId} aria-label="Labels">
              {options.length === 0 ? <li className="lb-empty">Every label is already on this recording.</li> : null}
              {options.map((o, i) => {
                const kind = labelKind(o.value);
                const head = !o.isNew && kind !== lastKind ? (kind === "type" && current ? `${KIND_NAME[kind]} · replaces ${current}` : KIND_NAME[kind]) : null;
                lastKind = kind;
                return (
                  <li key={o.value} role="presentation">
                    {head ? <div className="lb-head" aria-hidden="true">{head}</div> : null}
                    <div
                      role="option"
                      id={`${listId}-${i}`}
                      aria-selected={i === active}
                      className="lb-opt"
                      data-kind={kind}
                      onMouseDown={(e) => {
                        e.preventDefault();
                        add(o.value);
                      }}
                      onMouseEnter={() => setActive(i)}
                    >
                      {o.isNew ? (
                        <>
                          New topic “{o.value}” <span className="muted">· open list</span>
                        </>
                      ) : (
                        <>
                          {kind === "person" ? <span className="label-prefix">人</span> : null}
                          {labelText(o.value)}
                        </>
                      )}
                    </div>
                  </li>
                );
              })}
            </ul>
          ) : null}
        </div>
      </div>
      <p className="labels-note">Dashed: set by the agent. Filled: added by a person.</p>
    </div>
  );
}
