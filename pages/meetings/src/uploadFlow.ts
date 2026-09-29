// Upload, in three steps: create `rec.<id>` `uploading` first (so a
// recording in flight shows), POST the bytes to /f/ with progress, then CAS the
// record to `uploaded` with the file and create `q.<id>` for the drain.
import type { Rec } from "./types";
import { explain, putRec, Refused, SLUG } from "./registry";
import { load, setProgress } from "./store";

/** ccc-pages' single-request body cap. */
export const FILE_MAX = 100 * 1024 * 1024;
const TRIES = 5;

export interface UploadMeta {
  file: File;
  title: string;
  speakers: string[];
  recordedAt: Date;
  device: string;
  /** Seconds, when the browser could read it from the file. */
  duration?: number | null;
}

export interface Upload {
  id: string;
  /** Settles when the record is `uploaded` and queued. */
  done: Promise<void>;
}

const pad = (n: number) => String(n).padStart(2, "0");
/** The record id's stamp, in this device's wall time (as megameet's phone ids). */
export const idStamp = (d: Date) => `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
/** A device name as the id's host part: "Room recorder" → "room-recorder". */
export const hostOf = (device: string) => (device.trim().toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "") || "phone").slice(0, 40);
const codecOf = (name: string) => (/\.([a-z0-9]+)$/i.exec(name)?.[1] ?? "bin").toLowerCase();

export async function sha256Hex(file: Blob): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
  return [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** The file's length from its metadata, or null within 8 s. */
export function mediaDuration(file: File): Promise<number | null> {
  return new Promise((resolve) => {
    const a = document.createElement("audio");
    const url = URL.createObjectURL(file);
    const done = (v: number | null) => {
      URL.revokeObjectURL(url);
      resolve(v);
    };
    const t = setTimeout(() => done(null), 8000);
    a.preload = "metadata";
    a.muted = true;
    a.onloadedmetadata = () => {
      clearTimeout(t);
      done(Number.isFinite(a.duration) && a.duration > 0 ? a.duration : null);
    };
    a.onerror = () => {
      clearTimeout(t);
      done(null);
    };
    a.src = url;
  });
}

interface Posted {
  id: string;
  file?: { sha256?: string };
}

/** POST the bytes by XHR, for upload progress. */
export function postFile(file: File, meta: Record<string, string>, onProgress: (p: number) => void): Promise<Posted> {
  return new Promise((resolve, reject) => {
    const q = new URLSearchParams({ name: file.name, meta: JSON.stringify(meta) });
    const x = new XMLHttpRequest();
    x.open("POST", `/f/${encodeURIComponent(SLUG)}?${q}`);
    x.withCredentials = true;
    x.setRequestHeader("x-ccc-auth", "cookie");
    x.setRequestHeader("content-type", file.type || "application/octet-stream");
    x.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total);
    };
    x.onload = () => {
      let body: unknown = null;
      try {
        body = JSON.parse(x.responseText);
      } catch {
        /* not JSON */
      }
      if (x.status >= 200 && x.status < 300) resolve(body as Posted);
      else reject(new Refused(x.status, body as { error?: string; code?: string } | null));
    };
    x.onerror = () => reject(new Error("the upload was interrupted"));
    x.send(file);
  });
}

/** Why an upload stopped; the record, when created, stays `uploading`. */
export class UploadFailed extends Error {
  constructor(reason: string, created: boolean) {
    super(`Upload failed: ${reason}.${created ? " The recording stays listed as uploading; upload the file again." : ""}`);
  }
}

const reasonOf = (e: unknown) => (e instanceof Refused ? explain(e) : e instanceof Error ? e.message : String(e)).replace(/\.$/, "");

/**
 * Hash the file and create the record; resolves with its id once the record
 * exists, while the bytes go up behind `done`. Progress is in the store under
 * the id until the upload settles.
 */
export async function startUpload(m: UploadMeta): Promise<Upload> {
  const { file } = m;
  if (!file.size) throw new UploadFailed("this file is empty", false);
  if (file.size > FILE_MAX) throw new UploadFailed(`this file is larger than ${FILE_MAX / 1024 / 1024} MiB`, false);
  const host = hostOf(m.device);
  const sum = await sha256Hex(file);
  const dur = m.duration && m.duration > 0 ? m.duration : null;
  const started = m.recordedAt;

  // 1. The record, created first (v=0). A taken id moves one second on.
  let id = "";
  let version = 0;
  let rec: Rec | null = null;
  for (let i = 0; i < TRIES && !id; i++) {
    const cand = `${idStamp(new Date(started.getTime() + i * 1000))}-phone-${host}`;
    const next: Rec = {
      id: cand,
      source: "phone",
      host,
      title: m.title,
      started: started.toISOString(),
      stopped: dur ? new Date(started.getTime() + dur * 1000).toISOString() : undefined,
      duration_s: dur ? Math.round(dur * 10) / 10 : undefined,
      speakers: m.speakers.map((name) => ({ name })),
      files: [],
      state: "uploading",
      attempts: 0,
      updated: new Date().toISOString(),
    };
    try {
      version = (await putRec(`rec.${cand}`, "meeting@1", next, 0)).version;
      id = cand;
      rec = next;
    } catch (e) {
      if (!(e instanceof Refused && e.code === "version_conflict")) throw e;
    }
  }
  if (!id || !rec) throw new UploadFailed("five recordings already exist at this time on this device", false);
  setProgress(id, 0);
  void load();

  const created = rec;
  const done = (async () => {
    try {
      // 2. The bytes.
      const up = await postFile(file, { rec: id, role: "media" }, (p) => setProgress(id, p));
      const serverSum = up?.file?.sha256;
      if (serverSum && serverSum !== sum) throw new Error(`the server stored sha256 ${serverSum.slice(0, 12)}…, the file has ${sum.slice(0, 12)}…`);
      // 3. Uploaded, then the queue index.
      const uploaded: Rec = { ...created, files: [{ role: "media", file: up.id, bytes: file.size, sha256: sum, verified: false, codec: codecOf(file.name) }], state: "uploaded", updated: new Date().toISOString() };
      await putRec(`rec.${id}`, "meeting@1", uploaded, version);
      try {
        await putRec(`q.${id}`, "queue@1", { state: "uploaded" }, 0);
      } catch (e) {
        if (!(e instanceof Refused && e.code === "version_conflict")) throw e;
      }
    } catch (e) {
      throw new UploadFailed(reasonOf(e), true);
    } finally {
      setProgress(id, undefined);
      await load();
    }
  })();
  return { id, done };
}
