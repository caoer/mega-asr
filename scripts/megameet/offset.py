#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11,<3.13"
# dependencies = ["audio-offset-finder==0.5.5"]
# ///
"""Where a Feishu minute's media sits on a local recording's timeline.

  uv run --script scripts/megameet/offset.py --expect S FEISHU_MEDIA LOCAL_TRACK...

The local tracks (a mac recording's remote and mic, frame-aligned) are mixed,
as Feishu's media mixes both sides. --expect is Feishu's 0 s on the local
timeline by the two wall clocks. A probe of --probe seconds of Feishu's audio
near each end of the overlap that --expect predicts is searched for in the local
mix within --slack seconds of where it should be (audio-offset-finder: MFCC
cross-correlation; its standard score says how sure: above 10 is a match).
Three positions are tried per end and the best kept. Prints JSON:

  {"feishu_s", "local_s", "probes": [{"at": "start"|"end", "feishu_s", "local_s", "score"}]}

a probe's local_s being the local time of its feishu_s. megameet fits the
offset and the clock drift from the probes. An overlap shorter than three
probes gets the start probe only.
"""
import argparse, json, math, subprocess, sys

import numpy as np
from audio_offset_finder.audio_offset_finder import find_offset_between_buffers

FS = 8000
HOP = 128  # audio-offset-finder's default: 16 ms frames


def decode(path):
    p = subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-i", path, "-vn", "-ac", "1", "-ar", str(FS),
                        "-f", "s16le", "-"], capture_output=True)
    if p.returncode:
        sys.exit(f"ffmpeg {path}: {p.stderr.decode().strip()}")
    return np.frombuffer(p.stdout, dtype=np.int16).astype(np.float32)


def mix(paths):
    tracks = [decode(p) for p in paths]
    out = np.zeros(max(len(t) for t in tracks), dtype=np.float32)
    for t in tracks:
        out[:len(t)] += t
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--expect", type=float, required=True, help="Feishu's 0 s on the local timeline, s")
    ap.add_argument("--probe", type=float, default=60, help="probe length, s")
    ap.add_argument("--slack", type=float, default=300, help="search this far either side of the prediction, s")
    ap.add_argument("feishu")
    ap.add_argument("local", nargs="+")
    a = ap.parse_args()
    feishu, local = decode(a.feishu), mix(a.local)
    tf, tl = len(feishu) / FS, len(local) / FS
    lo, hi = max(0.0, -a.expect), min(tf, tl - a.expect)  # the overlap, Feishu time
    span = hi - lo

    def search(f):
        probe = feishu[int(f * FS):int((f + a.probe) * FS)]
        w0 = max(0.0, a.expect + f - a.slack)
        w1 = min(tl, a.expect + f + a.probe + a.slack)
        window = local[int(w0 * FS):int(w1 * FS)]
        r = find_offset_between_buffers(window, probe, FS, hop_length=HOP, max_frames=len(probe) // HOP)
        s = float(r["standard_score"])
        return {"feishu_s": round(f, 3), "local_s": round(w0 + r["time_offset"], 3),
                "score": round(s, 2) if math.isfinite(s) else 0.0}

    probes = []
    if span >= a.probe:
        ends = [("start", [lo + k * span for k in (0.05, 0.10, 0.15)])]
        if span >= 3 * a.probe:
            ends.append(("end", [hi - a.probe - k * span for k in (0.05, 0.10, 0.15)]))
        for at, fs in ends:
            tries = [search(min(max(f, lo), hi - a.probe)) for f in fs]
            best = max(tries, key=lambda t: t["score"])
            probes.append({"at": at, **best})
    json.dump({"feishu_s": round(tf, 3), "local_s": round(tl, 3), "probes": probes}, sys.stdout)
    print()


if __name__ == "__main__":
    main()
