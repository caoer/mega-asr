// The agent asks only when a change needs consent. Pending asks surface
// in the filter rail and in this review dialog. Label questions come from
// `labels.questions` and are answered here (questions.ts); the page's
// hook applies an approval within a minute or two.
import { useState, useSyncExternalStore } from "react";
import { Dialog, toast } from "./ui";
import { meetingHref } from "./route";
import { load, useStore } from "./store";
import { explain, Refused } from "./registry";
import { displayTitle } from "./model";
import { fmtDateTime, parseTime } from "./time";
import { affected, answerQuestion, NotOpen, phase, proposalText, type Phase, type Verdict } from "./questions";
import type { LabelQuestion } from "./types";

/** The label questions still waiting for an answer. */
export const useOpenQuestions = () => useStore((s) => s.questions).filter((q) => q.status === "open");

let open = false;
const subs = new Set<() => void>();
export function openProposals() {
  open = true;
  subs.forEach((l) => l());
}
function closeProposals() {
  open = false;
  subs.forEach((l) => l());
}
const useOpen = () =>
  useSyncExternalStore(
    (l) => {
      subs.add(l);
      return () => subs.delete(l);
    },
    () => open,
  );

export function ProposalsBanner() {
  const questions = useOpenQuestions().length;
  if (!questions) return null;
  return (
    <section className="asks" aria-label="Agent asks awaiting you">
      <h2>
        Agent asks <span className="num">{questions}</span>
      </h2>
      <ul>
        <li>
          <span className="ask-kind">labels</span> {questions} {questions === 1 ? "change needs" : "changes need"} your answer
        </li>
      </ul>
      <button type="button" className="btn ghost ask-btn" onClick={openProposals}>
        Review Agent Asks
      </button>
    </section>
  );
}

export function ProposalsDialog() {
  const isOpen = useOpen();
  return (
    <Dialog open={isOpen} onClose={closeProposals} title="Agent asks awaiting you" className="sheet">
      <div className="sheet-body asks-body">
        <p className="muted">The agent keeps labels and people tidy on its own. These changes touch the closed list, many recordings or two wiki pages, so it asks first.</p>
        <LabelQuestions />
      </div>
    </Dialog>
  );
}

const PHASE: Record<Phase, string> = {
  open: "Waiting for you",
  applying: "Approved · applying",
  failed: "Approved · not applied",
  applied: "Applied",
  declined: "Declined",
  expired: "Expired",
};

/** Open questions first, then the ones in flight, then the last three settled. */
export function LabelQuestions() {
  const questions = useStore((s) => s.questions);
  const open = questions.filter((q) => phase(q) === "open");
  const inFlight = questions.filter((q) => phase(q) === "applying" || phase(q) === "failed");
  const settled = questions
    .filter((q) => ["applied", "declined", "expired"].includes(phase(q)))
    .sort((a, b) => String(b.answered ?? b.asked ?? "").localeCompare(String(a.answered ?? a.asked ?? "")))
    .slice(0, 3);
  if (!open.length && !inFlight.length && !settled.length) return null;
  return (
    <section className="label-asks" aria-label="Label questions from the agent">
      <h3 className="asks-sub">Labels</h3>
      {[...open, ...inFlight, ...settled].map((q) => (
        <QuestionCard key={q.id} q={q} />
      ))}
    </section>
  );
}

function QuestionCard({ q }: { q: LabelQuestion }) {
  const tz = useStore((s) => s.tz);
  const recs = useStore((s) => s.rows).map((r) => r.rec);
  const [busy, setBusy] = useState<Verdict | null>(null);
  const p = phase(q);
  const touched = affected(q.proposal, recs);
  const when = parseTime(p === "applied" ? q.applied : q.answered);
  const decide = async (v: Verdict) => {
    setBusy(v);
    try {
      const r = await answerQuestion(q.id, v);
      toast(
        v === "declined"
          ? `${q.id} declined.`
          : r.rang
            ? `${q.id} approved — the change applies within a minute or two.`
            : `${q.id} approved. The change applies on the agent's next pass (the page could not wake it now).`,
      );
    } catch (e) {
      toast(
        e instanceof NotOpen
          ? e.message
          : e instanceof Refused && (e.status === 401 || e.status === 403)
            ? "Only the page owner can answer the agent. This link cannot."
            : `Could not save the answer: ${explain(e)}`,
      );
    } finally {
      setBusy(null);
      void load();
    }
  };
  return (
    <article className="ask" data-phase={p}>
      <h3>
        <span className="ask-kind">{q.id}</span> {proposalText(q.proposal)}
      </h3>
      {q.text ? <p lang="zh">{q.text}</p> : null}
      {touched.length ? (
        <>
          <p className="ask-outcome">
            On <span className="num">{touched.length}</span> {touched.length === 1 ? "recording" : "recordings"}
          </p>
          <ul className="ask-list">
            {touched.slice(0, 4).map((r) => (
              <li key={r.id}>
                <a href={meetingHref(r.id, new URLSearchParams())}>{displayTitle(r)}</a>
              </li>
            ))}
            {touched.length > 4 ? <li className="muted">and {touched.length - 4} more</li> : null}
          </ul>
        </>
      ) : null}
      {p === "open" ? (
        <div className="confirm-actions">
          <button type="button" className="btn ghost" disabled={busy !== null} onClick={() => void decide("declined")}>
            {busy === "declined" ? "Declining…" : "Decline"}
          </button>
          <button type="button" className="btn primary" disabled={busy !== null} onClick={() => void decide("approved")}>
            {busy === "approved" ? "Approving…" : "Approve"}
          </button>
        </div>
      ) : (
        <p className="ask-phase" data-phase={p}>
          <b>{PHASE[p]}</b>
          {when ? ` · ${fmtDateTime(when, tz)}` : null}
          {p === "failed" ? ` — ${q.error}. It is retried on the next pass.` : null}
        </p>
      )}
    </article>
  );
}
