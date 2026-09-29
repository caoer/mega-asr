// Alignment: Feishu's minute (the reference) beside our ASR, one row per Feishu
// turn. Each of our tokens gets a time interpolated inside its segment; the
// timeline is cut into ~2-minute chunks at Feishu turn starts and a longest
// common subsequence inside each chunk pairs the two texts. A matched ASR
// token sits in its partner's row; the others fall between their matched
// neighbours by time. A reading aid, not the scored CER.
import type { Segment } from "./types";

export interface Tok {
  t: string;
  k: string | null;
  hit: boolean | null;
  time?: number;
  seg?: number;
  row?: number;
  mate?: Tok;
}

export interface AlignRow {
  ref: Segment;
  refToks: Tok[];
  ours: Tok[];
  oursSpeakers: string[];
  agree: number | null;
  refChars: number;
}

const WORD = /[\p{L}\p{N}]/u;
export function tokens(text: string): Tok[] {
  const out: Tok[] = [];
  const re = /[㐀-鿿豈-﫿]|[\p{L}\p{N}]+(?:['’]\p{L}+)?|\s+|[^\s]/gu;
  let m: RegExpExecArray | null;
  while ((m = re.exec(text))) out.push({ t: m[0], k: WORD.test(m[0]) ? m[0].toLowerCase() : null, hit: null });
  return out;
}
export const tokLen = (x: Tok) => [...x.t].length;

function timedTokens(segs: Segment[], pick: (s: Segment) => string): Tok[] {
  const out: Tok[] = [];
  segs.forEach((s, si) => {
    const toks = tokens(pick(s));
    const total = toks.reduce((n, x) => n + tokLen(x), 0) || 1;
    let at = 0;
    for (const x of toks) {
      x.time = s.start + ((s.end - s.start) * (at + tokLen(x) / 2)) / total;
      x.seg = si;
      at += tokLen(x);
      out.push(x);
    }
  });
  return out;
}

function lcsPair(A: Tok[], B: Tok[]): boolean {
  const n = A.length, m = B.length;
  if (!n || !m) return true;
  if (n * m > 6_000_000) return false;
  const dp = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) dp[i][j] = A[i].k === B[j].k ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  let i = 0, j = 0;
  while (i < n && j < m) {
    if (A[i].k === B[j].k) {
      A[i].mate = B[j];
      B[j].mate = A[i];
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) i++;
    else j++;
  }
  return true;
}

function trim(toks: Tok[]): Tok[] {
  let a = 0, b = toks.length;
  while (a < b && /^\s+$/.test(toks[a].t)) a++;
  while (b > a && /^\s+$/.test(toks[b - 1].t)) b--;
  return toks.slice(a, b);
}

export function alignRows(ref: Segment[], ours: Segment[], useRaw: boolean): { rows: AlignRow[]; overall: number | null } {
  const R = ref.map((s, i) => {
    const toks = tokens(s.text);
    for (const x of toks) x.row = i;
    return toks;
  });
  const O = timedTokens(ours, (s) => (useRaw && s.raw ? s.raw : s.text));
  const starts = ref.map((s) => s.start);
  const rowAt = (t: number) => {
    let lo = 0, hi = starts.length - 1, at = 0;
    while (lo <= hi) {
      const m = (lo + hi) >> 1;
      if (starts[m] <= t) {
        at = m;
        lo = m + 1;
      } else hi = m - 1;
    }
    return at;
  };
  const cuts = [0];
  for (let i = 1; i < ref.length; i++) if (ref[i].start - ref[cuts[cuts.length - 1]].start >= 120) cuts.push(i);
  cuts.push(ref.length);
  let oi = 0;
  for (let c = 0; c + 1 < cuts.length; c++) {
    const r0 = cuts[c], r1 = cuts[c + 1];
    const tEnd = r1 < ref.length ? ref[r1].start : Infinity;
    const A = R.slice(r0, r1).flat().filter((x) => x.k);
    const B: Tok[] = [];
    while (oi < O.length && O[oi].time! < tEnd) {
      if (O[oi].k) B.push(O[oi]);
      oi++;
    }
    const done = lcsPair(A, B);
    for (const x of A) x.hit = done ? !!x.mate : null;
    for (const x of B) x.hit = done ? !!x.mate : null;
  }
  const next = new Array<number>(O.length);
  let nr = ref.length - 1;
  for (let i = O.length - 1; i >= 0; i--) {
    if (O[i].mate) nr = O[i].mate!.row!;
    next[i] = nr;
  }
  let pr = 0, last = 0;
  const buckets: Tok[][] = ref.map(() => []);
  O.forEach((x, i) => {
    if (!x.k) {
      buckets[last]?.push(x);
      return;
    }
    if (x.mate) last = pr = x.mate.row!;
    else last = Math.min(Math.max(rowAt(x.time!), pr), Math.max(pr, next[i]));
    buckets[last].push(x);
  });
  let fa = 0, fb = 0;
  const rows = ref.map((s, i): AlignRow => {
    const words = R[i].filter((x) => x.k);
    const chars = words.reduce((n, x) => n + tokLen(x), 0);
    const known = words.length > 0 && words.every((x) => x.hit !== null);
    const agree = known ? words.filter((x) => x.hit).reduce((n, x) => n + tokLen(x), 0) / chars : null;
    if (agree !== null) {
      fa += chars;
      fb += agree * chars;
    }
    const ourToks = trim(buckets[i]);
    const oursSpeakers = [...new Set(buckets[i].filter((x) => x.k).map((x) => ours[x.seg!].speaker).filter(Boolean))];
    return { ref: s, refToks: R[i], ours: ourToks, oursSpeakers, agree, refChars: chars };
  });
  return { rows, overall: fa ? fb / fa : null };
}
