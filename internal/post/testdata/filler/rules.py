#!/usr/bin/env python3
"""Rules-only filler cleanup, v0. Deterministic; the shape a Go post.Stage would take.

usage: rules.py IN_DIR OUT_DIR    (every *.txt in IN_DIR -> OUT_DIR/<name>.txt)
       rules.py -                 (stdin -> stdout)
"""
import re, sys, pathlib

CJK = r"[一-鿿]"
SENT_END = "。？！"
PUNCT = "，。？！、"

# R1 English fillers (word-bounded, with trailing comma/space).
R_EN = re.compile(r"\b(?:u+m+|u+h+|erm|hmm+)\b[,]?\s*", re.I)

# R2 Chinese filler runs: 嗯/呃 with attached punctuation; one run may span a VAD break (a full stop between filler characters).
R_ZH = re.compile(r"(?:[嗯呃]+[，。、]?\s*)+")

# R3 stutter repeats of a closed set of function words (fixpoint loop).
REPEAT_WORDS = ["这个", "那个", "就是", "然后", "我们", "我", "你", "他", "她", "它", "这", "那"]
R_REP = [re.compile(rf"({w})(?:{w})+") for w in REPEAT_WORDS]

# R4 VAD-break join: a clause-final function word followed by 。 and more text is not a sentence end.
R_VAD = re.compile(r"(就是|然后|因为|但是|而且|或者|所以)。(?=" + CJK + ")")


def zh_fillers(s: str) -> str:
    # whole utterance is fillers only (嗯 = yes) -> leave it
    if R_ZH.fullmatch(s.strip()):
        return s
    out, pos = [], 0
    for m in R_ZH.finditer(s):
        run = m.group(0)
        prev = s[m.start() - 1] if m.start() > 0 else ""
        out.append(s[pos:m.start()])
        trailing = run.rstrip()[-1:] if run.rstrip() else ""
        if trailing in SENT_END:
            # keep the sentence end, drop a preceding "，" so we don't leave "，。"
            if out and out[-1].endswith("，"):
                out[-1] = out[-1][:-1]
            out.append(trailing)
        # else: delete whole run (ends with ，/、/nothing)
        pos = m.end()
    out.append(s[pos:])
    return "".join(out)


def repeats(s: str) -> str:
    prev = None
    while prev != s:
        prev = s
        for r in R_REP:
            s = r.sub(r"\1", s)
    return s


def cleanup(s: str) -> str:
    s = re.sub(r"，+", "，", s)
    s = re.sub(r"。+", "。", s)
    s = re.sub(r"，([。？！])", r"\1", s)
    s = re.sub(r"([。？！])，", r"\1", s)
    s = re.sub(r"^[，、\s]+", "", s)
    s = re.sub(r"[ \t]{2,}", " ", s)
    s = re.sub(r" ([,.?!])", r"\1", s)
    return s


def clean(s: str) -> str:
    s = R_EN.sub("", s)
    s = zh_fillers(s)
    s = repeats(s)
    s = R_VAD.sub(r"\1，", s)
    return cleanup(s)


if __name__ == "__main__":
    if sys.argv[1:] == ["-"]:
        sys.stdout.write(clean(sys.stdin.read()))
        sys.exit(0)
    src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    dst.mkdir(parents=True, exist_ok=True)
    for p in sorted(src.glob("*.txt")):
        (dst / p.name).write_text(clean(p.read_text()))
