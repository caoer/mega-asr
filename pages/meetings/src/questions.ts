// The label agent's questions (`labels.questions`), answered on this page.
// An answer is written onto its question in the `labels` record, the
// same fields `megameet labels question --approve|--decline` writes, then the
// page's inbox is rung. The page's host hook (mega-asr pages/meetings-hook)
// runs `megameet labels apply --approved`: the question becomes `applied`, or
// keeps its `error`. The record is the truth; the ring only wakes the hook.
import { getRec, putRec, ring, Refused, type Entry } from "./registry";
import type { LabelQuestion, LabelsRecord, Rec } from "./types";

export type Verdict = "approved" | "declined";
export type Phase = "open" | "applying" | "failed" | "applied" | "declined" | "expired";

/** Where a question stands, from its status and answer fields. */
export function phase(q: LabelQuestion): Phase {
  switch (q.status) {
    case "open":
      return "open";
    case "approved":
      return q.error ? "failed" : "applying";
    case "applied":
      return "applied";
    case "declined":
      return "declined";
    default:
      return "expired";
  }
}

const q = (s: string) => `“${s}”`;
const KIND: Record<string, string> = { type: "a meeting type", project: "a project", person: "a person", topic: "a topic" };

/** The proposal in one line: the change an approval makes. */
export function proposalText(p: string[] | undefined): string {
  if (!p?.length) return "A question with no change attached.";
  const [op, ...a] = p;
  switch (op) {
    case "promote":
      return `Add ${q(a[0])} to the closed list as ${KIND[a[1]] ?? a[1]}.`;
    case "demote":
      return `Take ${q(a[0])} off the closed list; recordings keep it as an open label.`;
    case "rename":
      return `Rename ${q(a[0])} to ${q(a[1])} on every recording.`;
    case "merge": {
      const i = a.indexOf("--into");
      return `Merge ${a.slice(0, i).map(q).join(", ")} into ${q(a[i + 1])} on every recording.`;
    }
    case "split":
      return `Split ${q(a[0])} across ${a.length - 1} ${a.length === 2 ? "recording" : "recordings"}, each getting its own labels.`;
    case "set":
      return `Set the labels of one recording to ${a.slice(1).map(q).join(", ") || "none"}.`;
  }
  return p.join(" ");
}

/** The live recordings an approval would relabel. */
export function affected(p: string[] | undefined, recs: Rec[]): Rec[] {
  if (!p?.length) return [];
  const [op, ...a] = p;
  const live = recs.filter((r) => r.state !== "deleted");
  const carrying = (names: string[]) => live.filter((r) => (r.labels ?? []).some((l) => names.includes(l)));
  switch (op) {
    case "rename":
    case "demote":
    case "promote":
      return carrying([a[0]]);
    case "merge":
      return carrying(a.slice(0, a.indexOf("--into")));
    case "split":
      return carrying([a[0]]);
    case "set":
      return live.filter((r) => r.id === a[0]?.replace(/^rec\./, ""));
  }
  return [];
}

export class NotOpen extends Error {}

/** The `labels` record with question `id` answered; every other field kept. */
export function withAnswer(data: LabelsRecord & Record<string, unknown>, id: string, verdict: Verdict, answer: string, now: Date): LabelsRecord & Record<string, unknown> {
  const qs = data.questions ?? [];
  const i = qs.findIndex((x) => x.id === id);
  if (i < 0) throw new NotOpen(`There is no question ${id} any more.`);
  if (qs[i].status !== "open") throw new NotOpen(`${id} was answered meanwhile (${qs[i].status}).`);
  const at = now.toISOString().replace(/\.\d{3}Z$/, "Z");
  const questions = qs.map((x, j) => (j === i ? { ...x, status: verdict, answer, answered: at } : x));
  return { ...data, questions, updated_at: at, updated_by: "meetings page" };
}

const answerText = (v: Verdict) => (v === "approved" ? "Approve (meetings page)" : "Decline (meetings page)");

/** labellog.<UTC stamp to the microsecond>.<n>.<6 hex>, as megameet writes them. */
function logKey(now: Date, n: number): string {
  const p = (x: number, w = 2) => String(x).padStart(w, "0");
  const stamp = `${now.getUTCFullYear()}${p(now.getUTCMonth() + 1)}${p(now.getUTCDate())}T${p(now.getUTCHours())}${p(now.getUTCMinutes())}${p(now.getUTCSeconds())}.${p(now.getUTCMilliseconds(), 3)}000Z`;
  const hex = Array.from(crypto.getRandomValues(new Uint8Array(3)), (b) => b.toString(16).padStart(2, "0")).join("");
  return `labellog.${stamp}.${p(n % 10000, 4)}.${hex}`;
}
let logSeq = 0;

export interface Answered {
  /** The ring failed: the answer is saved, the hook runs on the next ring or the agent's daily pass. */
  rang: boolean;
}

/**
 * Answer question `id`: a CAS on `labels` touching that question alone
 * (re-read and retried on version_conflict, at most five times), its log
 * entry, then the ring. Throws NotOpen when someone answered first.
 */
export async function answerQuestion(id: string, verdict: Verdict, now = () => new Date()): Promise<Answered> {
  const answer = answerText(verdict);
  for (let tries = 0; ; tries++) {
    const cur: Entry<LabelsRecord & Record<string, unknown>> = await getRec("labels");
    const next = withAnswer(cur.value?.data ?? {}, id, verdict, answer, now());
    try {
      await putRec("labels", cur.value?.schema ?? "labels@1", next, cur.version);
      break;
    } catch (e) {
      if (e instanceof Refused && e.code === "version_conflict" && tries < 4) continue;
      throw e;
    }
  }
  const at = now();
  await putRec(logKey(at, ++logSeq), "labellog@1", {
    at: at.toISOString().replace(/\.\d{3}Z$/, "Z"),
    by: "meetings page",
    op: ["question", id, verdict],
    reason: answer,
    question: id,
    records: [],
  }, 0).catch(() => undefined); // the log is history; the answer already stands
  try {
    await ring(`labels-${id}-${verdict}`, { text: `labels ${id} ${verdict}`, by: "page", channel: "page" });
    return { rang: true };
  } catch {
    return { rang: false };
  }
}
