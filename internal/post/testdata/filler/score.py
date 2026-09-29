#!/usr/bin/env python3
"""Score a cleanup output dir against gold.md, on content characters (punctuation and spaces excluded).

Gold marks: [x] must-remove, {x} may-remove, [a|b] / {a|b} replace a with b.
G_min = raw with must applied; G_max = raw with must+may applied.

  wrong    : content chars of G_max missing from the output   (content loss — the metric that matters)
  inserted : content chars of the output absent from raw       (hallucination signal)
  miss     : must-chars the output kept  (= content chars of the output absent from G_min, minus inserted)
  hit      : must content chars - miss

usage: score.py OUT_DIR [--show]      score.py --taxonomy
"""
import re, sys, difflib, pathlib
from collections import Counter

HERE = pathlib.Path(__file__).parent
MARK = re.compile(r"([\[{])(.*?)(?:\|(.*?))?([\]}])(?:\^([a-z]))?", re.S)
NONCONTENT = set("，。？！、,.?!'\" \t\n")


def content(s):
    return "".join(ch for ch in s if ch not in NONCONTENT)


def parse_gold():
    """-> {name: (raw, g_min, g_max, spans)}; spans = [(kind, tag, text)]"""
    gold = {}
    text = (HERE / "gold.md").read_text()
    for block in re.split(r"^## ", text, flags=re.M)[1:]:
        name, _, body = block.partition("\n")
        body = body.strip()
        raw, gmin, gmax, spans = [], [], [], []
        pos = 0
        for m in MARK.finditer(body):
            plain = body[pos:m.start()]
            raw.append(plain); gmin.append(plain); gmax.append(plain)
            kind = "must" if m.group(1) == "[" else "may"
            span, repl = m.group(2), m.group(3) or ""
            raw.append(span)
            gmin.append(repl if kind == "must" else span)
            gmax.append(repl)
            spans.append((kind, m.group(5) or "?", span))
            pos = m.end()
        tail = body[pos:]
        raw.append(tail); gmin.append(tail); gmax.append(tail)
        gold[name.strip()] = ("".join(raw), "".join(gmin), "".join(gmax), spans)
    return gold


def missing(a, b):
    """content chars of a that are not present in b (a -> b alignment, deleted/replaced on a's side)"""
    n = 0
    for op, i1, i2, j1, j2 in difflib.SequenceMatcher(None, a, b, autojunk=False).get_opcodes():
        if op in ("delete", "replace"):
            n += len(content(a[i1:i2]))
    return n


def main():
    gold = parse_gold()

    if "--taxonomy" in sys.argv:
        c = Counter((k, t) for *_x, spans in gold.values() for k, t, _ in spans)
        print("category    must  may")
        for t, label in [("f", "filler"), ("r", "repeat"), ("s", "false-start"), ("v", "vad-break")]:
            print(f"{label:11s} {c[('must', t)]:4d} {c[('may', t)]:4d}")
        print(f"files with marks: {sum(1 for *_x, s in gold.values() if s)} / {len(gold)}")
        return

    out_dir = pathlib.Path(sys.argv[1])
    tot = Counter()
    print(f"{'file':17s} hit miss wrong ins")
    for name, (raw, gmin, gmax, spans) in gold.items():
        p = out_dir / f"{name}.txt"
        if not p.exists():
            continue
        out = p.read_text().strip()
        c = Counter()
        c["wrong"] = missing(gmax, out)
        c["ins"] = missing(out, raw)
        c["miss"] = max(0, missing(out, gmin) - c["ins"])
        must_chars = sum(len(content(t)) for k, _, t in spans if k == "must")
        c["hit"] = must_chars - c["miss"]
        tot.update(c)
        print(f"{name:17s} {c['hit']:3d} {c['miss']:4d} {c['wrong']:5d} {c['ins']:3d}")
        if "--show" in sys.argv:
            print("   ", out)
    h, m, w = tot["hit"], tot["miss"], tot["wrong"]
    print(f"{'TOTAL':17s} {h:3d} {m:4d} {w:5d} {tot['ins']:3d}")
    print(f"recall(must) = {h/(h+m):.2f}   content-loss chars = {w}   inserted chars = {tot['ins']}")


main()
