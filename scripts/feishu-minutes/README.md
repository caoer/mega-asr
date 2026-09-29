# Feishu Minutes archive

`fetch.py` mirrors every Feishu Minutes (飞书妙记) item you can see (the media and Feishu's speaker-labelled transcript) into a private folder. It then cuts the transcripts into a calibration manifest that `asrbench score` reads. Feishu's closed ASR is strong, which makes its transcripts a reference for measuring megavoice on real meetings. The script needs Python 3 (stdlib only), [`lark-cli`](https://github.com/larksuite/cli) logged in as you (`lark-cli auth login`), and `ffmpeg`. Reading the web list additionally needs `agent-browser` and a Chromium logged in to Feishu.

```bash
python3 scripts/feishu-minutes/fetch.py                  # list + fetch what is new (incremental, resumable)
python3 scripts/feishu-minutes/fetch.py --cdp 9222       # also read the web list via a logged-in browser (complete)
python3 scripts/feishu-minutes/fetch.py summaries        # Feishu's summary for archived minutes that have none recorded
python3 scripts/feishu-minutes/fetch.py manifest         # segments/ + segments.jsonl + calibration.jsonl
python3 scripts/feishu-minutes/fetch.py stats            # inventory.json + minutes.jsonl
go run ./cmd/asrbench score -manifest ~/.local/share/mega-asr/corpora/feishu-minutes/calibration.jsonl HYP.jsonl
```

The default output is `~/.local/share/mega-asr/corpora/feishu-minutes/` (`--out` changes it). It holds other people's voices and words, so keep it out of every repository. The script only reads from Feishu: it never edits, shares, or requests permissions.

## Listing

No single Feishu call lists everything, so the script takes the union of several:

- `minutes +search` returns at most 50 results per query. Even below that cap it silently drops items from wide date ranges. The script therefore runs three searches (owner = me, participant = me, unfiltered) and halves any date window that returns 40 or more results.
- Some minutes shared with you never appear in search at all, not even by exact title. They do appear on the Minutes home page. With `--cdp PORT`, the script opens one tab in that browser, pages through `/minutes/api/space/list` with the page's own session, and closes that tab.

Without `--cdp` the listing is the search union. That misses the occasional shared meeting.

## Fetching

For each new minute, the script:

1. `minutes minutes get` → `meta.json`. A minute younger than 3 h may still be transcribing, so it stays `pending` for the next run.
2. `minutes +detail --transcript --summary` → Feishu's summary (markdown, as Feishu wrote it; `""` when the minute has none) into `state.json`, and `transcript.txt` (Feishu's text: `<speaker> HH:MM:SS.mmm` then the sentence), parsed into `transcript.json` segments `{speaker, start_s, text}`.
3. `minutes +download --url-only`, then an HTTP download with `Range` resume (`media.part` → `media.m4a` for recordings, `media.mp4` for video meetings). The script downloads the URL itself because lark-cli refuses it when a fake-IP proxy (Surge, Clash) resolves Feishu's stream host to `198.18.x.x`. A viewer of someone else's minute gets `2091005 permission deny` on the media and keeps the transcript only (`media_error: export_denied`).

`state.json` records each minute's state (`done`, `pending`, `error`) after every item. An interrupted run resumes where it stopped, a re-run fetches only minutes not yet `done`, and an `error` is retried on the next run. Every run (and `summaries` alone) then fetches the summary of each `done` minute without a `summary` key, 20 minutes a call. A summary that fails is logged and retried on the next run; it does not change the run's exit code (`summaries` alone still exits 1 on a failure). To refetch a minute, delete its entry from `state.json`.

## Layout

```
state.json                      per-minute record: title, owner, relation (owner|shared), source (recording|meeting), created, duration, media, summary
minutes/<token>/                meta.json, transcript.txt, transcript.json, media.{m4a,mp4}
segments/<token>/NNNN.wav       16 kHz mono calibration clips
segments.jsonl                  every transcript segment of every minute with media, with usable/reason
calibration.jsonl               the usable segments: asrbench's manifest
minutes.jsonl, inventory.json   written by `stats`
```

## Calibration rows

A row carries asrbench's keys `{id, wav, ref, lang, dur_s, source}` plus `minute`, `speaker`, `start_s`, `relation`, `usable`, `reason`. A segment runs from 0.2 s before its stated start to the next segment's start, so it includes the pause before the next speaker. `reason` is one of:

- `ok`
- `empty`
- `too_short` (under 1 s)
- `too_long` (over 30 s)
- `rate_low` (under 1 Han char or Latin word per second: mostly silence)
- `rate_high` (over 9 units per second: the timing is off)

`lang` is the script tag of mega-asr-loop's Spokenly source (`mega_asr_loop/sources/spokenly_extract.py`): `zh`, `en`, `mixed` (Han and Latin in one segment), or `none`.

Feishu's transcript is a reference, not ground truth. It is smoothed (fillers dropped, some punctuation invented). Crosstalk sits under a single speaker. The segment boundaries come from Feishu's timestamps, not forced alignment. Use it to rank models and track regressions, not as a CER floor.

`stats` sorts each minute by its speakers holding at least 10 % of the words: `solo` (one), `1:1` (two), `group` (three or more). Diarization splits and merges voices, so the 1:1 label is a heuristic. Language mix per minute uses the Latin share of words: `zh` (under 5 %), `zh+en` (5–30 %), `en+zh` (30–80 %), `en` (80 % or more).
