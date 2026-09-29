#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11,<3.13"
# dependencies = ["meeteval==0.4.3"]
# ///
"""Speaker-attributed error rates of a local transcript against Feishu's.

  uv run --script scripts/megameet/cpwer.py IN.json

IN.json: {"collar": 5, "ref": [{speaker, start, end, words}], "hyp": [...]}, the
words already normalised and split (a token per Chinese character, one per
Latin word, space-separated), both on one timeline. Prints JSON:

  {"cp": {errors, length, error_rate, assignment}, "tcp": {...}}

cp is meeteval's cpWER over the whole meeting: each hypothesis speaker's text
against the reference speaker it best matches, time playing no part. tcp is its
tcpWER: a word matches only within the collar of its reference, word times
spread over each segment by character count. assignment pairs (reference
speaker, hypothesis speaker).
"""
import json, sys

from meeteval.io import SegLST
from meeteval.wer import cpwer, tcpwer


def seglst(rows):
    return SegLST([{"session_id": "m", "speaker": r["speaker"], "start_time": r["start"], "end_time": r["end"],
                    "words": r["words"]} for r in rows])


def out(er):
    return {"errors": er.errors, "length": er.length, "error_rate": er.error_rate,
            "assignment": [list(p) for p in er.assignment]}


def main():
    d = json.load(open(sys.argv[1]))
    ref, hyp = seglst(d["ref"]), seglst(d["hyp"])
    cp = cpwer(ref, hyp)["m"]
    tcp = tcpwer(ref, hyp, collar=d["collar"])["m"]
    json.dump({"cp": out(cp), "tcp": out(tcp)}, sys.stdout)
    print()


if __name__ == "__main__":
    main()
