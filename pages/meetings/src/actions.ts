// Actions and Delete on a recording. An action writes `action` onto the record
// by compare-and-swap, then creates `q.<id>` so the drain sees it. A delete is
// a tombstone: `rec.<id>` keeps what names and dedupes the recording, with
// state deleted, so no source registers it again and no drain claims it;
// `q.<id>` and every file of it go.
import { ACTIONS, type ActionName, type Rec } from "./types";
import { call, delRec, fileURL, getRec, listAll, putRec, Refused, type Entry } from "./registry";
import { getState, load } from "./store";

const SCHEMA = "meeting@1";
const TRIES = 5;

/** megameet's meeting.Tombstone keep list; `feishu` is reduced to its token. */
export const TOMBSTONE_KEEPS = ["id", "source", "host", "owner", "title", "started", "stopped", "duration_s", "wiki"] as const;

export function tombstone(rec: Rec, now = new Date()): Rec {
  const t: Record<string, unknown> = {};
  const src = rec as unknown as Record<string, unknown>;
  for (const k of TOMBSTONE_KEEPS) if (k in src) t[k] = src[k];
  if (rec.feishu && typeof rec.feishu === "object" && rec.feishu.token) t.feishu = { token: rec.feishu.token };
  const at = now.toISOString().replace(/\.\d{3}Z$/, "Z");
  return { ...t, state: "deleted", deleted_at: at, deleted_by: "meetings page", updated: at } as Rec;
}

/** A drain's lease on the record, while it has not expired. */
export function liveClaim(rec: Rec | null | undefined, now = Date.now()): Rec["claim"] | null {
  const c = rec?.claim;
  if (!c || typeof c !== "object" || !c.expires) return null;
  const exp = Date.parse(c.expires);
  return Number.isFinite(exp) && exp > now ? c : null;
}

/** Why a write did not land, from the record as it is now. */
export function conflictText(cur: Rec | null | undefined): string {
  if (cur?.state === "deleted") return "This recording was deleted meanwhile.";
  const c = liveClaim(cur);
  if (c) return `In progress on ${c.by || "another host"} — try again when it finishes.`;
  if (cur?.state === "processing") return "In progress on another host — try again when it finishes.";
  return `The recording changed since this page read it${cur?.state ? ` (now ${cur.state})` : ""}. The list is refreshed; check it and try again.`;
}

const isConflict = (e: unknown) => e instanceof Refused && e.code === "version_conflict";
const isGone = (e: unknown) => e instanceof Refused && e.status === 404;

/** The record as the store last read it. */
function rowOf(id: string) {
  const row = getState().rows.find((r) => r.rec.id === id);
  if (!row) throw new Error("This recording is no longer listed. The list is refreshed.");
  return row;
}

async function current(key: string): Promise<Entry<Rec> | null> {
  try {
    return await getRec<Rec>(key);
  } catch (e) {
    if (isGone(e)) return null;
    throw e;
  }
}

/** Create the queue index; one that exists stays. */
async function enqueue(id: string, state: string) {
  try {
    await putRec(`q.${id}`, "queue@1", { state }, 0);
  } catch (e) {
    if (!isConflict(e)) throw e;
  }
}

/**
 * Queue an action: `action` + `updated` on the record, then `q.<id>`. A
 * conflicting writer makes it re-read and reapply while the action still
 * applies; otherwise it stops and says why.
 */
export async function queueAction(id: string, name: ActionName): Promise<void> {
  const row = rowOf(id);
  let version = row.version;
  let rec: Rec = row.rec;
  try {
    for (let i = 0; ; i++) {
      if (!ACTIONS[name].states.includes(rec.state) || rec.action || liveClaim(rec)) throw new Error(conflictText(rec));
      try {
        await putRec(row.key, SCHEMA, { ...rec, action: name, updated: new Date().toISOString() }, version);
        break;
      } catch (e) {
        if (!isConflict(e) || i + 1 >= TRIES) throw isConflict(e) ? new Error(conflictText(rec)) : e;
      }
      const cur = await current(row.key);
      if (!cur?.value) throw new Error("This recording was deleted meanwhile.");
      version = cur.version;
      rec = { ...cur.value.data, id };
      if (rec.action === name) break; // another tab queued the same action
    }
    await enqueue(id, rec.state);
  } finally {
    await load();
  }
}

/**
 * Delete = tombstone. Re-reads first and stops, before any write, when the
 * record moved on since the page read it or a drain holds a live claim.
 */
export async function deleteRecording(id: string): Promise<void> {
  const row = rowOf(id);
  try {
    const cur = await current(row.key);
    if (!cur?.value) throw new Error("This recording was deleted meanwhile.");
    const rec = { ...cur.value.data, id };
    if (cur.version !== row.version || liveClaim(rec)) throw new Error(conflictText(rec));
    try {
      await putRec(row.key, SCHEMA, tombstone(rec), cur.version);
    } catch (e) {
      if (!isConflict(e)) throw e;
      throw new Error(conflictText((await current(row.key))?.value?.data));
    }
    try {
      await delRec(`q.${id}`);
    } catch (e) {
      if (!isGone(e)) throw e;
    }
    // The files it named, and any other file whose meta.rec is this record.
    const ids = new Set((rec.files ?? []).map((f) => f.file).filter((f): f is string => !!f));
    try {
      for (const it of await listAll<{ meta?: { rec?: string } }>("f.", true)) if (it.value?.data?.meta?.rec === id) ids.add(it.key.slice(2));
    } catch {
      /* the named ones still go */
    }
    for (const f of ids) {
      try {
        await call("DELETE", fileURL(f));
      } catch (e) {
        if (!isGone(e)) throw e;
      }
    }
  } finally {
    await load();
  }
}
