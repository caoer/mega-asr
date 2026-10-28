# Evaluated alternative: NetEase Youdao Confucius4-R2T2

**Not adopted.** On our dictation set R2T2 is less accurate than megavoice's
deployed engine, 2–3× slower on CPU and uses 4× the memory. Measured
2026-10-03.

## What R2T2 is

[Confucius4-R2T2](https://github.com/netease-youdao/Confucius4-R2T2) is
Qwen3-ASR-1.7B fine-tuned for append-only streaming: text it has emitted is
never revised, and it stays usable at 160 ms chunks where its base model
collapses. Its point is streaming latency, not accuracy; on NetEase's own 14
test sets it is less accurate than its base with 2 s chunks. Code is
Apache-2.0; the weights are under the NetEase Youdao Model Use License, which
forbids using R2T2's output to improve another commercial model.

## Setup

- Set: the 360 dictation takes of 60 s or less (Chinese, English,
  code-switched), each decoded whole as megavoice does; plus the takes over
  60 s cut at pauses as megavoice does (`--window deployed`).
- Baseline: the deployed configuration — Fun-ASR-Nano, run-4 fine-tuned
  decoder, q8_0, live hotwords (`llama-funasr-cli`). Its decoder was
  fine-tuned on the same speaker (none of the eval takes), an in-domain
  advantage R2T2 lacks.
- R2T2: `Confucius4-R2T2-GGUF` Q8_0 + mmproj on a CPU-only rebuild of
  llama.cpp `ad6c6683`, code at `26d55a54`. Q8_0 scored within noise of fp32
  PyTorch on a 60-take subset.
- Scoring: `asrbench score` (TER = mixed token error rate); intervals are a
  paired bootstrap over clips, 2,000 resamples.

## Results

Short takes (≤ 60 s, 360 takes); Nano with hotwords scores 4.08 % TER.

| R2T2 setting | TER | Δ vs Nano, pp [95 %] | Δ, spoken numbers normalised to digits |
|---|---|---|---|
| Chinese forced (best) | 5.46 % | +1.38 [+0.80, +1.96] | +0.80 [+0.36, +1.24] |
| Auto language (default) | 6.38 % | +2.30 [+1.27, +3.55] | +1.73 [+0.79, +2.98] |
| Default + hotword list as context | 8.15 % | +4.07 [+2.37, +6.07] | +3.48 [+1.84, +5.47] |

- Losses are on English (best setting 11.2 % WER vs 8.8 %) and punctuation
  (F1 0.62 vs 0.82); Chinese with digits normalised is within noise.
- On auto language the llama.cpp route returns nothing for some whole
  utterances (6 of 360, mostly English); forcing Chinese stops it. No setting
  hallucinated.
- Long takes (> 60 s, 101 with references, default setting): 5.79 % vs 5.26 %,
  +0.53 [−0.21, +1.24] — within noise. All 461 takes together: still behind,
  +1.16 [+0.59, +1.85].

Speed and memory, each engine alone on a 16-vCPU x86 VM at 8 threads (Nano as
FunASR v1.3.30 + `runtime/funasr-cli-serve.patch`):

| | R2T2 Q8_0 | Nano | ratio |
|---|---|---|---|
| 5–9 s take | 2.66 s | 0.96 s | 2.8× |
| 55–60 s take | 18.4 s | 8.5 s | 2.2× |
| load | 8.6 s | 1.5 s | |
| peak RSS | 6.85 GB | 1.66 GB | 4.1× |

Not measured: the 160 ms streaming mode. With one-shot accuracy more than a
point behind, it was not run.

## Verdict

Keep Fun-ASR-Nano. R2T2 loses on accuracy where megavoice spends most of its
time (whole takes ≤ 60 s), costs 2–3× the latency and 4× the memory, and has
no Mac build out of the box. Worth revisiting only for a streaming,
never-revised live-caption use, which megavoice does not have today.

## Sources

The full report, the per-clip hypotheses, scores and run scripts are kept
with the private evaluation records, outside this repository; the eval audio
is private dictation and is not published.
