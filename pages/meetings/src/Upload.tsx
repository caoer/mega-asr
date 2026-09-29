import { useId, useRef, useState, type FormEvent } from "react";
import { href } from "./route";
import { useStore } from "./store";
import { FILE_MAX, mediaDuration, startUpload } from "./uploadFlow";
import { queueAction } from "./actions";
import { explain, SLUG } from "./registry";
import { fmtClock, fmtDay, fmtDur, fromLocalInput, parseTime, toLocalInput, tzOffset, zoneLabel } from "./time";
import { Dialog, fmtBytes, StateBadge, toast } from "./ui";

const DEVICES = ["Phone", "Mac", "Room recorder", "Other"];

function UploadForm({ by, onDone }: { by: "owner" | "contributor"; onDone?: (id: string) => void }) {
  const tz = useStore((s) => s.tz);
  const progress = useStore((s) => s.progress);
  const [file, setFile] = useState<File | null>(null);
  const [title, setTitle] = useState("");
  const [speakers, setSpeakers] = useState("");
  const [at, setAt] = useState(() => toLocalInput(new Date(), tz));
  const [device, setDevice] = useState("Phone");
  const [err, setErr] = useState<string | null>(null);
  const [duration, setDuration] = useState<number | null>(null);
  // idle → reading (hashing, creating the record) → sending (bytes, progress) → done
  const [phase, setPhase] = useState<"idle" | "reading" | "sending" | "done">("idle");
  const [current, setCurrent] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const uid = useId();
  const p = current ? progress[current] : undefined;
  const busy = phase === "reading" || phase === "sending";

  const onFile = (f: File | null) => {
    setFile(f);
    setErr(null);
    setDuration(null);
    if (!f) return;
    if (!title) setTitle(f.name.replace(/\.[a-z0-9]+$/i, ""));
    // A saved voice memo's modification time is its end; the start is that minus its length.
    const end = f.lastModified ? new Date(f.lastModified) : new Date();
    setAt(toLocalInput(end, tz));
    void mediaDuration(f).then((d) => {
      setDuration(d);
      if (d) setAt(toLocalInput(new Date(end.getTime() - d * 1000), tz));
    });
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!file) {
      setErr("Choose an audio file to upload.");
      fileRef.current?.focus();
      return;
    }
    if (file.size > FILE_MAX) {
      setErr(`That file is ${fmtBytes(file.size)}; one upload takes up to 100 MB. Trim it or split it into parts.`);
      fileRef.current?.focus();
      return;
    }
    const recordedAt = fromLocalInput(at, tz) ?? new Date();
    setErr(null);
    setPhase("reading");
    startUpload({ file, title: title.trim() || file.name, speakers: speakers.split(/[,，、]/).map((s) => s.trim()).filter(Boolean), recordedAt, device, duration }).then(
      ({ id, done }) => {
        setCurrent(id);
        setPhase("sending");
        onDone?.(id);
        done.then(
          () => setPhase("done"),
          (e) => {
            setErr(explain(e));
            setPhase("idle");
          },
        );
      },
      (e) => {
        setErr(explain(e));
        setPhase("idle");
      },
    );
  };

  const done = phase === "done";

  if (done)
    return (
      <div className="upload-done" role="status">
        <p>
          <strong>Uploaded.</strong> It is transcribed and titled on the server’s next pass.
        </p>
        <div className="confirm-actions">
          {by === "owner" ? (
            <a className="btn ghost" href={href(["m", current!])}>
              Open Recording
            </a>
          ) : null}
          <button
            type="button"
            className="btn primary"
            onClick={() => {
              setCurrent(null);
              setPhase("idle");
              setFile(null);
              setDuration(null);
              setTitle("");
              setSpeakers("");
            }}
          >
            Upload Another
          </button>
        </div>
      </div>
    );

  return (
    <form className="upload-form" onSubmit={submit} noValidate>
      <div className="field">
        <label htmlFor={`${uid}-file`}>Audio file</label>
        <label className={`drop${file ? " has" : ""}`} htmlFor={`${uid}-file`}>
          {file ? (
            <>
              <strong>{file.name}</strong>
              <span className="muted">
                {fmtBytes(file.size)}
                {duration ? ` · ${fmtDur(duration)}` : ""} · tap to choose another
              </span>
            </>
          ) : (
            <>
              <strong>Choose a recording</strong>
              <span className="muted">m4a, mp3, wav, mp4 — up to 100&nbsp;MB</span>
            </>
          )}
        </label>
        <input ref={fileRef} id={`${uid}-file`} className="sr-only" type="file" name="file" accept="audio/*,video/mp4,.m4a,.mp3,.wav,.flac,.ogg,.opus" aria-invalid={!!err} aria-describedby={err ? `${uid}-err` : undefined} onChange={(e) => onFile(e.target.files?.[0] ?? null)} disabled={busy} />
        {err ? (
          <p className="field-err" id={`${uid}-err`}>
            {err}
          </p>
        ) : null}
      </div>
      <div className="field">
        <label htmlFor={`${uid}-title`}>Title</label>
        <input id={`${uid}-title`} name="title" autoComplete="off" placeholder="Defaults to the file name" value={title} onChange={(e) => setTitle(e.target.value)} disabled={busy} />
        <p className="hint">The agent writes a display title from the content; this name is kept as the original.</p>
      </div>
      <div className="field">
        <label htmlFor={`${uid}-spk`}>Speakers</label>
        <input id={`${uid}-spk`} name="speakers" autoComplete="off" placeholder="Comma-separated names" value={speakers} onChange={(e) => setSpeakers(e.target.value)} disabled={busy} />
      </div>
      <div className="field-row">
        <div className="field">
          <label htmlFor={`${uid}-at`}>
            Recorded at <span className="muted">({zoneLabel(tz)} {tzOffset(tz)})</span>
          </label>
          <input id={`${uid}-at`} name="recorded-at" type="datetime-local" value={at} onChange={(e) => setAt(e.target.value)} disabled={busy} />
        </div>
        <div className="field">
          <label htmlFor={`${uid}-dev`}>Device</label>
          <select id={`${uid}-dev`} name="device" value={device} onChange={(e) => setDevice(e.target.value)} disabled={busy}>
            {DEVICES.map((d) => (
              <option key={d}>{d}</option>
            ))}
          </select>
        </div>
      </div>
      {busy ? (
        <div className="upload-progress">
          <div className="progress big" role="progressbar" aria-label="Upload progress" aria-valuenow={Math.round((p ?? 0) * 100)} aria-valuemin={0} aria-valuemax={100}>
            <span style={{ transform: `scaleX(${p ?? 0})` }} />
          </div>
          <span className="num">{phase === "reading" ? "Reading the file…" : `Uploading… ${Math.round((p ?? 0) * 100)}%`}</span>
        </div>
      ) : (
        <button type="submit" className="btn primary wide">
          Upload Recording
        </button>
      )}
    </form>
  );
}

export function UploadDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Dialog open={open} onClose={onClose} title="Upload a recording" className="sheet">
      <div className="sheet-body">
        <UploadForm by="owner" onDone={() => toast("Upload started — it appears in the list as uploading.")} />
      </div>
    </Dialog>
  );
}

export function ShareDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  // A browser cannot mint links: the page owner's identity does, one link per colleague.
  const cmd = `page-token mint ${SLUG} --role contributor --label "<colleague>"`;
  const [copied, setCopied] = useState(false);
  return (
    <Dialog open={open} onClose={onClose} title="Upload link for colleagues">
      <div className="share">
        <p>An upload link lets its holder upload recordings and see their own uploads. They cannot see other meetings, and cannot delete anything.</p>
        <p>Each colleague gets their own link, minted by the page owner (ask an agent to run this):</p>
        <div className="share-row">
          <input readOnly value={cmd} aria-label="Command that mints an upload link" onFocus={(e) => e.target.select()} name="share-cmd" />
          <button
            type="button"
            className="btn primary"
            onClick={() => {
              void navigator.clipboard?.writeText(cmd).then(() => setCopied(true));
            }}
          >
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
      </div>
    </Dialog>
  );
}

export function ContributorPage() {
  const rows = useStore((s) => s.rows);
  const tz = useStore((s) => s.tz);
  const progress = useStore((s) => s.progress);
  // The registry serves an upload link only what it created.
  const mine = rows;
  return (
    <div className="contrib">
      <header className="contrib-head">
        <h1>Upload a recording</h1>
        <p>Your recordings go to the team’s meetings, where they are transcribed, titled and summarised.</p>
        <p className="scope-note">This link shows only the recordings it uploaded. It can upload and retry them, not delete them.</p>
      </header>
      <section className="contrib-form">
        <UploadForm by="contributor" />
      </section>
      <section className="contrib-mine" aria-labelledby="mine-h">
        <h2 id="mine-h">
          Your uploads <span className="num muted">{mine.length}</span>
        </h2>
        {mine.length === 0 ? (
          <p className="muted">Nothing yet. What you upload with this link appears here.</p>
        ) : (
          <ul className="mine">
            {mine.map((r) => {
              const d = parseTime(r.rec.started);
              const p = progress[r.rec.id];
              return (
                <li key={r.key}>
                  <div className="mine-title">{r.rec.display_title || r.rec.title}</div>
                  <div className="mine-meta">
                    <StateBadge state={r.rec.state} />
                    {d ? (
                      <span>
                        {fmtDay(d, tz)} {fmtClock(d, tz)}
                      </span>
                    ) : null}
                    <span>{fmtDur(r.rec.duration_s)}</span>
                    {r.rec.state === "failed" && !r.rec.action ? (
                      <button type="button" className="btn ghost sm" onClick={() => void queueAction(r.rec.id, "retry").then(() => toast("Retry queued. The server picks it up on its next pass."), (e) => toast(explain(e)))}>
                        Retry
                      </button>
                    ) : null}
                  </div>
                  {p !== undefined ? (
                    <div className="progress" role="progressbar" aria-label="Upload progress" aria-valuenow={Math.round(p * 100)} aria-valuemin={0} aria-valuemax={100}>
                      <span style={{ transform: `scaleX(${p})` }} />
                    </div>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </div>
  );
}
