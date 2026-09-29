#!/usr/bin/env python3
"""Mirror your Feishu Minutes (飞书妙记) into a private archive, and cut a
calibration manifest from them.

    python3 scripts/feishu-minutes/fetch.py [fetch] [--out DIR] [--since DATE] [--only TOKEN,...]
    python3 scripts/feishu-minutes/fetch.py summaries [--out DIR]
    python3 scripts/feishu-minutes/fetch.py manifest [--out DIR]
    python3 scripts/feishu-minutes/fetch.py stats [--out DIR]

Python 3 stdlib, `lark-cli` (logged in as you) and `ffmpeg`. See
scripts/feishu-minutes/README.md for the layout, the state file and the row
schema.
"""
import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import time
import urllib.request
import wave
from collections import Counter

DEFAULT_OUT = "~/.local/share/mega-asr/corpora/feishu-minutes"
EPOCH_START = "2020-01-01"
SEARCH_CAP = 40        # the search API stops at 50 results a query; split a window that nears it
MIN_WINDOW = datetime.timedelta(hours=1)
API_PAUSE_S = 0.4      # between lark-cli calls
SETTLE = datetime.timedelta(hours=3)  # a younger minute may still be transcribing: fetch it next run

# Calibration segments.
SEG_MIN_S, SEG_MAX_S = 1.0, 30.0
SEG_PAD_S = 0.2        # audio before a segment's stated start
UNITS_PER_S_MIN = 1.0  # below this the clip is mostly silence or crosstalk
UNITS_PER_S_MAX = 9.0
RATE = 16000

HAN = re.compile(r"[㐀-鿿豈-﫿]")
LATIN_WORD = re.compile(r"[A-Za-z]+")
SEG_HEAD = re.compile(r"^(.*\S)\s+(\d+):(\d{2}):(\d{2})\.(\d{3})\s*$")
MEDIA_EXT = {"audio/mp4": ".m4a", "video/mp4": ".mp4", "audio/mpeg": ".mp3", "audio/wav": ".wav"}


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def lark(*args, cwd=None):
    """Run one lark-cli call as the user; return its JSON `data`."""
    time.sleep(API_PAUSE_S)
    p = subprocess.run(["lark-cli", *args, "--as", "user", "--format", "json"], capture_output=True, text=True, cwd=cwd)
    try:
        out = json.loads(p.stdout)
    except json.JSONDecodeError:
        raise RuntimeError(f"lark-cli {' '.join(args[:2])}: {p.stderr.strip() or p.stdout.strip()}")
    if not out.get("ok"):
        raise RuntimeError(f"lark-cli {' '.join(args[:2])}: {json.dumps(out.get('error'), ensure_ascii=False)}")
    return out["data"]


def units(text):
    return len(HAN.findall(text)) + len(LATIN_WORD.findall(text))


def language(text):
    han, lat = len(HAN.findall(text)), len(LATIN_WORD.findall(text))
    if han and lat:
        return "mixed"
    return "zh" if han else "en" if lat else "none"


# ---------------------------------------------------------------- listing


def search_window(role, start, end):
    items, token = [], None
    while True:
        args = ["minutes", "+search", "--page-size", "30", "--start", start.isoformat(), "--end", end.isoformat()]
        if role != "any":
            args += [f"--{role}-ids", "me"]
        if token:
            args += ["--page-token", token]
        data = lark(*args)
        items += data.get("items") or []
        token = data.get("page_token")
        if not (data.get("has_more") and token):
            return items


WEB_LIST_JS = """(async () => {
  let ts = "", all = [];
  for (let i = 0; i < 200; i++) {
    const r = await fetch("/minutes/api/space/list?size=50&space_name=1&rank=1&asc=false&owner_type=1" + (ts ? "&timestamp=" + ts : ""), {credentials: "include"});
    const d = (await r.json()).data;
    all.push(...d.list.map(x => x.object_token));
    if (!d.has_more || !d.list.length) break;
    ts = d.list[d.list.length - 1].share_time;
    await new Promise(z => setTimeout(z, 400));
  }
  return JSON.stringify(all);
})()"""


def web_list(cdp, host):
    """Tokens on the Minutes home page (owned by anyone), read through a logged-in
    browser: the one list that holds every minute shared with you."""
    ab = ["agent-browser", "--cdp", str(cdp)]

    def run(*a):
        return subprocess.run([*ab, *a], capture_output=True, text=True, check=True).stdout.strip()

    run("tab", "new", f"https://{host}/minutes/home")
    tab = re.search(r"→ \[(t\d+)\]", run("tab", "list")).group(1)  # ours: close only this one
    try:
        for _ in range(30):  # the new tab is about:blank until the page commits
            if host in run("eval", "location.host"):
                break
            time.sleep(1)
        out = json.loads(run("eval", WEB_LIST_JS).splitlines()[-1])
        return json.loads(out) if isinstance(out, str) else out
    finally:
        subprocess.run([*ab, "tab", "close", tab], capture_output=True)


def list_minutes(since, cdp=None):
    """Every minute visible to you since `since`: token -> the searches that found it.

    Each search returns at most 50 results and misses some even below that, so
    three searches (owner, participant, unfiltered) walk small date windows."""
    found, host = {}, None
    tz = datetime.datetime.now().astimezone().tzinfo
    now = datetime.datetime.now(tz) + datetime.timedelta(days=1)
    for role in ("owner", "participant", "any"):
        stack = [(since, now)]
        while stack:
            a, b = stack.pop()
            items = search_window(role, a, b)
            if len(items) >= SEARCH_CAP and b - a > MIN_WINDOW:
                mid = a + (b - a) / 2
                stack += [(a, mid), (mid, b)]
                continue
            for it in items:
                found.setdefault(it["token"], set()).add(role)
                host = it["meta_data"]["app_link"].split("/")[2]
    if cdp:
        for t in web_list(cdp, host):
            found.setdefault(t, set()).add("web")
    return found


# ---------------------------------------------------------------- fetching


def download(url, dest):
    """Resumable download to dest (via dest.part); returns (path, bytes, type)."""
    part = dest + ".part"
    have = os.path.getsize(part) if os.path.exists(part) else 0
    req = urllib.request.Request(url, headers={"Range": f"bytes={have}-"} if have else {})
    with urllib.request.urlopen(req, timeout=60) as r:
        ctype = r.headers.get("Content-Type", "").split(";")[0]
        if have and r.status != 206:
            have = 0
        total = have + int(r.headers.get("Content-Length", 0))
        with open(part, "ab" if have else "wb") as f:
            while chunk := r.read(1 << 20):
                f.write(chunk)
    size = os.path.getsize(part)
    if total and size != total:
        raise RuntimeError(f"short download: {size} of {total} bytes")
    final = dest + MEDIA_EXT.get(ctype, ".bin")
    os.replace(part, final)
    return final, size, ctype


def parse_transcript(path):
    """Feishu's transcript.txt -> (header, keywords, [{speaker, start_s, text}])."""
    lines = open(path, encoding="utf-8").read().splitlines()
    header = lines[0].strip() if lines else ""
    keywords, segs, cur = "", [], None
    for i, line in enumerate(lines[1:], 1):
        if line.strip() == "Keywords:":
            keywords = lines[i + 1].strip() if i + 1 < len(lines) else ""
            continue
        m = SEG_HEAD.match(line)
        if m:
            h, mi, s, ms = map(int, m.groups()[1:])
            cur = {"speaker": m.group(1), "start_s": h * 3600 + mi * 60 + s + ms / 1000, "text": ""}
            segs.append(cur)
        elif cur is not None and line.strip():
            cur["text"] = (cur["text"] + "\n" + line.strip()).strip()
    return header, keywords, segs


def fetch_one(token, roles, me, root):
    d = os.path.join(root, "minutes", token)
    os.makedirs(d, exist_ok=True)
    meta = lark("minutes", "minutes", "get", "--params", json.dumps({"minute_token": token}))["minute"]
    created = datetime.datetime.fromtimestamp(int(meta["create_time"]) / 1000).astimezone()
    if datetime.datetime.now().astimezone() - created < SETTLE:
        return {"state": "pending"}
    rec = {
        "token": token,
        "title": meta.get("title", ""),
        "url": meta.get("url", ""),
        "owner_id": meta.get("owner_id", ""),
        "relation": "owner" if meta.get("owner_id") == me else "shared",
        "roles": sorted(roles),
        "source": (meta.get("generated_source") or {}).get("source_type", "recording"),
        "created": created.isoformat(timespec="seconds"),
        "duration_s": round(int(meta.get("duration", 0)) / 1000, 1),
    }
    json.dump(meta, open(os.path.join(d, "meta.json"), "w"), ensure_ascii=False, indent=1)

    # lark-cli writes only below its working directory, into a folder named after the title.
    tmp = os.path.join(d, ".detail")
    data = lark("minutes", "+detail", "--minute-tokens", token, "--transcript", "--summary",
                "--output-dir", ".detail", "--overwrite", cwd=d)
    artifacts = (data.get("minutes") or [{}])[0].get("artifacts") or {}
    rec["summary"] = artifacts.get("summary") or ""
    src = artifacts.get("transcript_file")
    src = src and os.path.join(d, src)
    if src and os.path.exists(src):
        os.replace(src, os.path.join(d, "transcript.txt"))
        subprocess.run(["rm", "-rf", tmp])
        header, keywords, segs = parse_transcript(os.path.join(d, "transcript.txt"))
        json.dump({"header": header, "keywords": keywords, "segments": segs},
                  open(os.path.join(d, "transcript.json"), "w"), ensure_ascii=False, indent=1)
        rec["segments"] = len(segs)
    else:
        rec["segments"] = 0

    # lark-cli refuses to download the URL itself when a fake-IP proxy (Surge,
    # Clash) resolves Feishu's stream host to 198.18.x, so fetch it here.
    try:
        url = lark("minutes", "+download", "--minute-tokens", token, "--url-only")["download_url"]
    except RuntimeError as e:
        if "2091005" not in str(e):
            raise
        rec.update(media=None, media_error="export_denied")  # a viewer of someone else's minute
    else:
        media, size, ctype = download(url, os.path.join(d, "media"))
        rec.update(media=os.path.basename(media), media_bytes=size, media_type=ctype)
    rec["state"] = "done"
    rec["fetched"] = datetime.datetime.now().astimezone().isoformat(timespec="seconds")
    return rec


def load_state(root):
    p = os.path.join(root, "state.json")
    return json.load(open(p)) if os.path.exists(p) else {"me": "", "minutes": {}}


def save_state(root, state):
    p = os.path.join(root, "state.json")
    json.dump(state, open(p + ".tmp", "w"), ensure_ascii=False, indent=1)
    os.replace(p + ".tmp", p)


def cmd_fetch(args, root):
    state = load_state(root)
    if not state["me"]:
        state["me"] = whoami()
    if args.only:
        listed = {t: set(state["minutes"].get(t, {}).get("roles", [])) or {"owner"} for t in args.only.split(",")}
    else:
        since = datetime.datetime.fromisoformat(args.since).astimezone()
        listed = list_minutes(since, args.cdp)
        log(f"listed {len(listed)} minutes since {args.since}")
    todo = [t for t in sorted(listed) if state["minutes"].get(t, {}).get("state") != "done"]
    log(f"{len(todo)} to fetch, {len(listed) - len(todo)} already archived")
    failed = 0
    for i, token in enumerate(todo, 1):
        try:
            rec = fetch_one(token, listed[token], state["me"], root)
        except Exception as e:  # keep going; the state file records it and the next run retries
            rec, failed = {"state": "error", "error": str(e)[:500]}, failed + 1
        rec["roles"] = sorted(set(rec.get("roles", [])) | listed[token])
        state["minutes"][token] = {**state["minutes"].get(token, {}), **rec}
        if rec["state"] == "done":
            state["minutes"][token].pop("error", None)
        save_state(root, state)
        log(f"[{i}/{len(todo)}] {token} {rec['state']} {rec.get('title', '')} {rec.get('error', '')}")
    fill_summaries(state, root)  # a failed summary is logged and retried next run; it does not fail the fetch
    state["last_run"] = datetime.datetime.now().astimezone().isoformat(timespec="seconds")
    save_state(root, state)
    return 1 if failed else 0


SUMMARY_BATCH = 20


def fill_summaries(state, root):
    """Fetch Feishu's summary for every archived minute that has none recorded
    yet ("" is recorded when the minute has no summary). Returns the failures."""
    todo = [t for t, r in sorted(state["minutes"].items()) if r.get("state") == "done" and "summary" not in r]
    failed = 0
    for i in range(0, len(todo), SUMMARY_BATCH):
        batch = todo[i:i + SUMMARY_BATCH]
        try:
            data = lark("minutes", "+detail", "--minute-tokens", ",".join(batch), "--summary")
        except RuntimeError as e:
            log(f"summaries {batch[0]}…: {e}")
            failed += len(batch)
            continue
        got = {m.get("minute_token"): (m.get("artifacts") or {}).get("summary") for m in data.get("minutes") or []}
        for t in batch:
            if got.get(t) is None:
                log(f"summary {t}: not in the answer")
                failed += 1
                continue
            state["minutes"][t]["summary"] = got[t]
        save_state(root, state)
    if todo:
        log(f"summaries: {len(todo) - failed} fetched ({sum(state['minutes'][t].get('summary') == '' for t in todo)} empty), {failed} failed")
    return failed


def cmd_summaries(args, root):
    return 1 if fill_summaries(load_state(root), root) else 0


def whoami():
    p = subprocess.run(["lark-cli", "auth", "status"], capture_output=True, text=True)
    return json.loads(p.stdout)["identities"]["user"]["openId"]


# ---------------------------------------------------------------- manifest


def decode_16k(media):
    p = subprocess.run(["ffmpeg", "-v", "error", "-i", media, "-vn", "-ac", "1", "-ar", str(RATE), "-f", "s16le", "-"],
                       capture_output=True, check=True)
    return p.stdout


def minute_segments(token, rec, root):
    """Rows for one archived minute; writes segment WAVs under segments/<token>/."""
    d = os.path.join(root, "minutes", token)
    tj = os.path.join(d, "transcript.json")
    if rec.get("state") != "done" or not rec.get("media") or not os.path.exists(tj):
        return []
    segs = json.load(open(tj))["segments"]
    rows = []
    for i, s in enumerate(segs):
        end = segs[i + 1]["start_s"] if i + 1 < len(segs) else rec["duration_s"]
        start = max(0.0, s["start_s"] - SEG_PAD_S, segs[i - 1]["start_s"] if i else 0.0)
        dur = end - start
        text = s["text"].replace("\n", " ").strip()
        n = units(text)
        reason = "ok"
        if not text:
            reason = "empty"
        elif dur < SEG_MIN_S:
            reason = "too_short"
        elif dur > SEG_MAX_S:
            reason = "too_long"
        elif n / dur < UNITS_PER_S_MIN:
            reason = "rate_low"
        elif n / dur > UNITS_PER_S_MAX:
            reason = "rate_high"
        rows.append({
            "id": f"{token}-{i:04d}", "wav": os.path.join(root, "segments", token, f"{i:04d}.wav"),
            "ref": text, "lang": language(text), "dur_s": round(dur, 2), "source": "feishu-minutes",
            "minute": token, "speaker": s["speaker"], "start_s": round(start, 2),
            "relation": rec["relation"], "usable": reason == "ok", "reason": reason,
        })
    use = [r for r in rows if r["usable"]]
    if use and not all(os.path.exists(r["wav"]) for r in use):
        pcm = decode_16k(os.path.join(d, rec["media"]))
        os.makedirs(os.path.join(root, "segments", token), exist_ok=True)
        for r in use:
            a = int(r["start_s"] * RATE) * 2
            b = min(len(pcm), a + int(r["dur_s"] * RATE) * 2)
            with wave.open(r["wav"], "wb") as w:
                w.setnchannels(1), w.setsampwidth(2), w.setframerate(RATE)
                w.writeframes(pcm[a:b])
    return rows


def cmd_manifest(args, root):
    state = load_state(root)
    rows = []
    for token, rec in sorted(state["minutes"].items(), key=lambda kv: kv[1].get("created", "")):
        got = minute_segments(token, rec, root)
        rows += got
        log(f"{token} {sum(r['usable'] for r in got)}/{len(got)} segments")
    with open(os.path.join(root, "segments.jsonl"), "w") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    with open(os.path.join(root, "calibration.jsonl"), "w") as f:
        for r in rows:
            if r["usable"]:
                f.write(json.dumps(r, ensure_ascii=False) + "\n")
    log(f"{sum(r['usable'] for r in rows)} calibration rows of {len(rows)} segments")
    return 0


# ---------------------------------------------------------------- stats


def minute_profile(token, root):
    tj = os.path.join(root, "minutes", token, "transcript.json")
    if not os.path.exists(tj):
        return None
    segs = json.load(open(tj))["segments"]
    by_speaker = Counter()
    han = lat = 0
    for s in segs:
        by_speaker[s["speaker"]] += units(s["text"])
        han += len(HAN.findall(s["text"]))
        lat += len(LATIN_WORD.findall(s["text"]))
    total = sum(by_speaker.values()) or 1
    main = [sp for sp, n in by_speaker.items() if n / total >= 0.10]
    return {"speakers": len(by_speaker), "main_speakers": len(main),
            "latin_share": round(lat / ((han + lat) or 1), 3), "units": han + lat}


def cmd_stats(args, root):
    state = load_state(root)
    ms = state["minutes"]
    done = {t: r for t, r in ms.items() if r.get("state") == "done"}
    hours = lambda rs: round(sum(r.get("duration_s", 0) for r in rs) / 3600, 2)
    prof = {t: minute_profile(t, root) for t in done}
    with_tx = [r for t, r in done.items() if prof[t] and prof[t]["units"]]

    def kind(p):
        if not p or not p["units"]:
            return "no_transcript"
        return {1: "solo", 2: "1:1"}.get(p["main_speakers"], "group")

    def mix(p):
        if not p or not p["units"]:
            return "none"
        x = p["latin_share"]
        return "zh" if x < 0.05 else "zh+en" if x < 0.30 else "en+zh" if x < 0.80 else "en"

    inv = {
        "minutes": len(ms), "archived": len(done),
        "states": dict(Counter(r.get("state") for r in ms.values())),
        "hours": hours(done.values()),
        "media_gb": round(sum(r.get("media_bytes", 0) for r in done.values()) / 1e9, 2),
        "date_range": [min((r["created"] for r in done.values()), default=None),
                       max((r["created"] for r in done.values()), default=None)],
        "with_transcript": {"minutes": len(with_tx), "hours": hours(with_tx)},
        "relation": {k: {"minutes": len(g), "hours": hours(g)} for k in ("owner", "shared")
                     if (g := [r for r in done.values() if r["relation"] == k])},
        "source": dict(Counter(r["source"] for r in done.values())),
        "media": {"downloaded": sum(bool(r.get("media")) for r in done.values()),
                  "export_denied": sum(r.get("media_error") == "export_denied" for r in done.values())},
        "kind": {k: {"minutes": len(g), "hours": hours(g)} for k in ("solo", "1:1", "group", "no_transcript")
                 if (g := [r for t, r in done.items() if kind(prof[t]) == k])},
        "language_mix": dict(Counter(mix(prof[t]) for t in done)),
    }
    cal = os.path.join(root, "segments.jsonl")
    if os.path.exists(cal):
        rows = [json.loads(l) for l in open(cal)]
        use = [r for r in rows if r["usable"]]
        inv["segments"] = {
            "all": len(rows), "usable": len(use), "usable_hours": round(sum(r["dur_s"] for r in use) / 3600, 2),
            "reasons": dict(Counter(r["reason"] for r in rows)),
            "usable_lang": dict(Counter(r["lang"] for r in use)),
        }
    json.dump(inv, open(os.path.join(root, "inventory.json"), "w"), ensure_ascii=False, indent=1)
    print(json.dumps(inv, ensure_ascii=False, indent=1))
    per = [{"token": t, "created": r["created"], "title": r["title"], "duration_min": round(r["duration_s"] / 60, 1),
            "relation": r["relation"], "source": r["source"], "kind": kind(prof[t]), "mix": mix(prof[t]),
            **(prof[t] or {})} for t, r in sorted(done.items(), key=lambda kv: kv[1]["created"])]
    with open(os.path.join(root, "minutes.jsonl"), "w") as f:
        for p in per:
            f.write(json.dumps(p, ensure_ascii=False) + "\n")
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", nargs="?", default="fetch", choices=["fetch", "summaries", "manifest", "stats"])
    ap.add_argument("--out", default=DEFAULT_OUT, help=f"archive root (default {DEFAULT_OUT})")
    ap.add_argument("--since", default=EPOCH_START, help="list minutes created on or after this date")
    ap.add_argument("--cdp", help="also read the Minutes home list through a logged-in browser on this CDP port "
                    "(agent-browser); it holds shared minutes the search API never returns")
    ap.add_argument("--only", help="fetch these comma-separated minute tokens, skipping the listing")
    args = ap.parse_args()
    root = os.path.expanduser(args.out)
    os.makedirs(root, mode=0o700, exist_ok=True)
    return {"fetch": cmd_fetch, "summaries": cmd_summaries, "manifest": cmd_manifest,
            "stats": cmd_stats}[args.command](args, root)


if __name__ == "__main__":
    sys.exit(main())
