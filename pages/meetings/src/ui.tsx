import { useEffect, useRef, useSyncExternalStore, type ReactNode } from "react";
import type { Rec } from "./types";
import { minuteOfDay, parseTime } from "./time";
import { labelKind, labelText } from "./model";

// ---------------------------------------------------------------------------
// Status: text always, with a shape so it never rests on color alone.

const STATE_TEXT: Record<string, string> = {
  uploading: "Uploading",
  uploaded: "Uploaded",
  processing: "Processing",
  aligned: "Aligned",
  ingested: "Ingested",
  failed: "Failed",
  deleted: "Deleted",
};
export const stateText = (s: string) => STATE_TEXT[s] ?? s;

export function StateGlyph({ state }: { state: string }) {
  const common = { width: 10, height: 10, viewBox: "0 0 10 10", "aria-hidden": true as const, className: "glyph" };
  switch (state) {
    case "ingested":
      return (
        <svg {...common}>
          <circle cx="5" cy="5" r="4" fill="currentColor" />
        </svg>
      );
    case "aligned":
      return (
        <svg {...common}>
          <circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" strokeWidth="1.4" />
          <path d="M5 1.5a3.5 3.5 0 0 1 0 7z" fill="currentColor" />
        </svg>
      );
    case "failed":
      return (
        <svg {...common}>
          <path d="M1.5 1.5l7 7M8.5 1.5l-7 7" stroke="currentColor" strokeWidth="1.8" />
        </svg>
      );
    case "deleted":
      return (
        <svg {...common}>
          <path d="M1 5h8" stroke="currentColor" strokeWidth="1.8" />
        </svg>
      );
    case "uploading":
    case "uploaded":
      return (
        <svg {...common}>
          <path d="M5 1l4 7H1z" fill={state === "uploaded" ? "currentColor" : "none"} stroke="currentColor" strokeWidth="1.3" />
        </svg>
      );
    default:
      return (
        <svg {...common}>
          <circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" strokeWidth="1.4" strokeDasharray="2 1.6" />
        </svg>
      );
  }
}

export function StateBadge({ state, extra }: { state: string; extra?: ReactNode }) {
  return (
    <span className="state" data-s={state}>
      <StateGlyph state={state} />
      {stateText(state)}
      {extra}
    </span>
  );
}

// ---------------------------------------------------------------------------
// The day ruler: 24 hours of the chosen zone, the recording drawn where it sat.

export function DayRuler({ rec, tz }: { rec: Rec; tz: string }) {
  const start = parseTime(rec.started);
  if (!start) return <div className="ruler" aria-hidden="true" />;
  const m0 = minuteOfDay(start, tz);
  const dur = Math.max(0, (rec.duration_s ?? 0) / 60);
  const left = (m0 / 1440) * 100;
  const width = Math.min(100 - left, (dur / 1440) * 100);
  const spill = m0 + dur > 1440;
  return (
    <div className="ruler" aria-hidden="true">
      <span className="ruler-bar" style={{ left: `${left}%`, width: `max(3px, ${width}%)` }} />
      {spill ? <span className="ruler-bar spill" style={{ left: 0, width: `${Math.min(100, ((m0 + dur - 1440) / 1440) * 100)}%` }} /> : null}
    </div>
  );
}

export function HourScale() {
  return (
    <div className="hour-scale" aria-hidden="true">
      {["00", "06", "12", "18", "24"].map((h) => (
        <span key={h}>{h}</span>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------

export function LabelChip({ label, agent, onRemove, onPick, pressed }: { label: string; agent?: boolean; onRemove?: () => void; onPick?: () => void; pressed?: boolean }) {
  const kind = labelKind(label);
  const body = (
    <>
      {kind === "person" ? <span className="label-prefix">人</span> : null}
      {labelText(label)}
    </>
  );
  return (
    <span className="label" data-kind={kind} data-agent={agent ? "" : undefined} title={agent === undefined ? undefined : agent ? "Proposed by the agent" : "Added by a person"}>
      {onPick ? (
        <button type="button" className="label-pick" onClick={onPick} aria-pressed={pressed} aria-label={`Filter by label ${label}`}>
          {body}
        </button>
      ) : (
        <span className="label-body">{body}</span>
      )}
      {onRemove ? (
        <button type="button" className="label-x" onClick={onRemove} aria-label={`Remove label ${label}`}>
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
            <path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" strokeWidth="1.5" />
          </svg>
        </button>
      ) : null}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Modal dialog on the native <dialog>: focus trap, Esc and backdrop for free.

export function Dialog({ open, onClose, title, children, className }: { open: boolean; onClose: () => void; title: string; children: ReactNode; className?: string }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      className={`dialog ${className ?? ""}`}
      aria-label={title}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === ref.current) onClose();
      }}
    >
      <div className="dialog-inner">
        <header className="dialog-head">
          <h2>{title}</h2>
          <button type="button" className="icon-btn" onClick={onClose} aria-label="Close">
            <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">
              <path d="M3 3l8 8M11 3l-8 8" stroke="currentColor" strokeWidth="1.6" />
            </svg>
          </button>
        </header>
        {open ? children : null}
      </div>
    </dialog>
  );
}

// ---------------------------------------------------------------------------
// Toasts: one polite live region.

let toasts: { id: number; text: string }[] = [];
const toastListeners = new Set<() => void>();
let nextToast = 1;
export function toast(text: string) {
  const id = nextToast++;
  toasts = [...toasts, { id, text }];
  toastListeners.forEach((l) => l());
  setTimeout(() => {
    toasts = toasts.filter((t) => t.id !== id);
    toastListeners.forEach((l) => l());
  }, 4200);
}
export function Toasts() {
  const list = useSyncExternalStore(
    (l) => {
      toastListeners.add(l);
      return () => toastListeners.delete(l);
    },
    () => toasts,
  );
  return (
    <div className="toasts" aria-live="polite" role="status">
      {list.map((t) => (
        <div className="toast" key={t.id}>
          {t.text}
        </div>
      ))}
    </div>
  );
}

export const fmtBytes = (n: number | undefined) =>
  typeof n !== "number" ? "—" : n >= 1e9 ? `${(n / 1e9).toFixed(2)} GB` : n >= 1e6 ? `${(n / 1e6).toFixed(1)} MB` : n >= 1e3 ? `${Math.round(n / 1e3)} KB` : `${n} B`;
