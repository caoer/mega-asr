---
version: alpha
name: "megavoice 录音历史"
description: "A local list of dictated takes, read and labelled in a browser window of any width with mouse and keys. The first screen answers: did any take not arrive, and where is its text? A take's detail answers: which engine heard it right?"
colors:
  glaze: "#edf1ec"
  slip: "#f8faf7"
  sunk: "#e2e9e3"
  ink: "#15201a"
  ink-2: "#3b4841"
  ash: "#56645c"
  rule: "#d3dcd5"
  rule-strong: "#b5c3b9"
  edge: "#7b8980"
  cobalt: "#2542b5"
  cobalt-deep: "#1b3291"
  cobalt-wash: "#dde4f7"
  cinnabar: "#b3361f"
  cinnabar-wash: "#f5dfd9"
  ochre: "#805508"
  ochre-wash: "#f3e6c4"
  jade: "#2b7654"
  steel: "#3a6a8a"
  hl: "#f1dc8a"
  primary: "{colors.ink}"
typography:
  title: { fontFamily: "Archivo Variable", fontSize: 20px, fontWeight: 600, lineHeight: 1.3 }
  heading: { fontFamily: "Archivo Variable", fontSize: 14px, fontWeight: 600, lineHeight: 1.4 }
  body: { fontFamily: "Archivo Variable", fontSize: 14px, fontWeight: 400, lineHeight: 1.5, fontFeature: '"tnum" 1' }
  reading: { fontFamily: "Archivo Variable", fontSize: 16px, fontWeight: 400, lineHeight: 1.75 }
  label: { fontFamily: "Archivo Variable", fontSize: 13px, fontWeight: 500, lineHeight: 1.4 }
  meta: { fontFamily: "Archivo Variable", fontSize: 12px, fontWeight: 400, lineHeight: 1.4, fontFeature: '"tnum" 1' }
  mono: { fontFamily: "Spline Sans Mono", fontSize: 13px, fontWeight: 400, lineHeight: 1.4 }
rounded: { sm: 4px, md: 7px, lg: 10px, full: 999px }
spacing: { 1: 4px, 2: 8px, 3: 12px, 4: 16px, 5: 20px, 6: 24px, 8: 32px, 12: 48px }
components:
  page: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink}", typography: "{typography.body}" }
  row: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink}", typography: "{typography.body}", height: 40px }
  row-selected: { backgroundColor: "{colors.cobalt-wash}", textColor: "{colors.ink}" }
  row-time: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink-2}", typography: "{typography.mono}" }
  day-head: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink}", typography: "{typography.heading}" }
  panel: { backgroundColor: "{colors.slip}", textColor: "{colors.ink}", rounded: "{rounded.lg}", padding: 12px }
  button-primary: { backgroundColor: "{colors.ink}", textColor: "{colors.glaze}", typography: "{typography.label}", rounded: "{rounded.md}", height: 32px, padding: 12px }
  button-secondary: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink}", typography: "{typography.label}", rounded: "{rounded.md}", height: 32px, padding: 12px }
  control-edge: { backgroundColor: "{colors.edge}", width: 1px }
  input: { backgroundColor: "{colors.slip}", textColor: "{colors.ink}", typography: "{typography.body}", rounded: "{rounded.md}", height: 36px }
  pill: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink-2}", typography: "{typography.label}", rounded: "{rounded.full}", height: 28px }
  pill-on: { backgroundColor: "{colors.ink}", textColor: "{colors.glaze}", rounded: "{rounded.full}" }
  state-undelivered: { backgroundColor: "{colors.ochre-wash}", textColor: "{colors.ochre}", typography: "{typography.label}", rounded: "{rounded.sm}" }
  notice-warn: { backgroundColor: "{colors.ochre-wash}", textColor: "{colors.ink}", rounded: "{rounded.md}", padding: 12px }
  notice-err: { backgroundColor: "{colors.cinnabar-wash}", textColor: "{colors.ink}", rounded: "{rounded.md}", padding: 12px }
  search-match: { backgroundColor: "{colors.hl}", textColor: "{colors.ink}", rounded: "{rounded.sm}" }
  row-hover: { backgroundColor: "{colors.sunk}", textColor: "{colors.ink}" }
  row-meta: { backgroundColor: "{colors.glaze}", textColor: "{colors.ash}", typography: "{typography.meta}" }
  divider: { backgroundColor: "{colors.rule}", height: 1px }
  panel-edge: { backgroundColor: "{colors.rule-strong}", width: 1px }
  focus-ring: { backgroundColor: "{colors.cobalt}", width: 2px }
  waveform-played: { backgroundColor: "{colors.cobalt}", textColor: "{colors.slip}" }
  link-on-wash: { backgroundColor: "{colors.cobalt-wash}", textColor: "{colors.cobalt-deep}" }
  failure-word: { backgroundColor: "{colors.glaze}", textColor: "{colors.cinnabar}", typography: "{typography.label}" }
  state-sent: { backgroundColor: "{colors.glaze}", textColor: "{colors.jade}", typography: "{typography.label}" }
  state-pasted: { backgroundColor: "{colors.glaze}", textColor: "{colors.steel}", typography: "{typography.label}" }
  diff-mark: { backgroundColor: "{colors.hl}", textColor: "{colors.ink}", rounded: "{rounded.sm}" }
  diff-underline: { backgroundColor: "{colors.ochre}", height: 2px }
  version-tag: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink-2}", typography: "{typography.meta}", rounded: "{rounded.sm}" }
  mark-on: { backgroundColor: "{colors.ink}", textColor: "{colors.glaze}", typography: "{typography.label}", rounded: "{rounded.full}", height: 28px }
  label-mark: { backgroundColor: "{colors.glaze}", textColor: "{colors.ink-2}", width: 10px }
  unsaved: { backgroundColor: "{colors.glaze}", textColor: "{colors.ochre}", typography: "{typography.label}" }
  word-playing: { backgroundColor: "{colors.cobalt-wash}", textColor: "{colors.ink}", rounded: "{rounded.sm}" }
  wave-band: { backgroundColor: "{colors.cobalt}", rounded: "{rounded.full}", height: 3px }
---

# megavoice Takes page

The page megavoice serves at `/takes/` on its loopback listener. The mock `pages/takes-mock/` (open `index.html` from disk; `?state=` picks a fixture) realises the list, the player and the resend picker; the version stack and the labels exist only in this directory's page.

## Overview

Mode: Operate. The menu bar row `打开录音历史 · 2 条未送达` opens it; the jobs are to find a take and get its text where it belongs (copy, resend, play, re-transcribe), and to label it: say which engine heard it right, or write what was said. The page is also the labelling tool: the compare panel's functions live in a take's detail, and `labels.jsonl` gets the same rows it got from the panel. The list is long and grows every day, so rows are dense and every action has a key.

```text
Mode:      Operate — megavoice's local Takes page (127.0.0.1, opened from the menu bar)
Who/verb:  find a take and put its text where it belongs; say which engine heard it right, or write
           what was said; mouse and keys, a window of any width
Focal:     list: the 未送达 strip and marks; detail: the take's texts stacked, the words the
           versions disagree on lit, 复制 / 重发… on the take's own text
Type:      no serif; Archivo for all words and numbers; Spline Sans Mono for times, ids, paths, engine names
Layout:    above 1080 px a 44 % list beside a docked detail; below, the list, then a pushed detail
Motion:    none; this is a keyboard tool
Signature: the scrub bar is the take's own waveform; the brand mark is that waveform, half played in
           cobalt; the words the engines disagree on are lit on every version, the take's own text
           included, so where to listen shows at a glance
Generic:   card per take, modal resend dialog, centred mic icon on empty; a Label tab or mode, a modal
           label form, green check buttons, a red and green diff table → one-line rows under sticky
           day heads, the resend picker inline under the text, a left-aligned empty line; the
           versions stacked where the take's text is, one lit mark, 这版对 as a pressed pill, the
           typed correction one more text under the stack
```

The look is the meetings page's (`pages/meetings/src/styles.css`, `pages/meetings/src/player/player.css`): porcelain neutrals on a green-grey hue, cobalt for play and selection, ochre for what still needs action, Archivo in its wider widths for headings. The page is plain HTML, JS and CSS embedded in the Go binary: no framework, no build step, nothing loaded from another host.

## Colors

Every colour token keeps the meetings page's name. Light is the `:root` block of `pages/meetings/src/styles.css`; dark is its `@media (prefers-color-scheme: dark)` block and the `:root[data-theme="dark"]` override, both carried over the same way. A token is cited by its name, which is the same in both stylesheets. A value that differs from the meetings page says why in its row.

| Token | Light | Dark | Role here |
|---|---|---|---|
| `--glaze` | #edf1ec | #0e1512 | page ground |
| `--slip` | #f8faf7 | #141d19 | raised: player, picker, inputs, keys panel |
| `--sunk` | #e2e9e3 | #1b2621 | hover, skeleton bars, raw event block |
| `--ink` | #15201a | #dde7e0 | text; primary button and pressed pill fill: a state filter, a pressed `这版对` |
| `--ink-2` | #3b4841 | #b3c1b8 | secondary text: row time, target, meta values, a version's tags; the list's labelled check |
| `--ash` | #56645c | #8b9a91 | tertiary text, cancelled and empty marks. Light is 5.45:1 on `--glaze` and 4.90 on the selected row's `--cobalt-wash`: darker than the meetings page's, for the 4.5:1 floor |
| `--rule` | #d3dcd5 | #25322b | row and section hairlines |
| `--rule-strong` | #b5c3b9 | #35453c | panel borders, the match and version tags' outline (structure only) |
| `--edge` | #7b8980 | #687a6f | the boundary of every control: buttons, pills, inputs, selects, player controls. `--rule-strong` as a control border is 1.60:1 on `--glaze`; the floor asks 3:1. Light 3.21 on `--glaze`, 3.49 on `--slip`; dark 4.06 and 3.77 |
| `--cobalt` | #2542b5 | #93a6ff | focus ring, played waveform, playhead, the line under the waveform for the span playing, links, the brand mark's played half, the pressed mute button's border |
| `--cobalt-deep` | #1b3291 | #b6c3ff | link text on a wash (`只看这些`), the pressed mute button's word, `已静音` |
| `--cobalt-wash` | #dde4f7 | #1d2749 | the selected row, the chosen picker option, the pressed mute button, `::selection`; the word playing |
| `--cinnabar` | #b3361f | #f0907a | failure words only: `送达失败`, a failed resend or re-transcription, the offline mark; the border of the failed re-transcription alert |
| `--cinnabar-wash` | #f5dfd9 | #3b201a | the failed re-transcription alert, the `megavoice 未运行` notice |
| `--ochre` | #805508 | #e2b660 | 未送达, 录音中, 识别中, the cloud mark, `未存`, the 2 px underline of a disagreement mark. Light is 5.71:1 on `--glaze` and 5.26 on `--ochre-wash`: darker than the meetings page's, for the floor |
| `--ochre-wash` | #f3e6c4 | #3a2f15 | the 未送达 tag and strip |
| `--jade` | #2b7654 | #7fcfa4 | the 已发送 mark, a delivered line in the timeline |
| `--steel` | #3a6a8a | #8fbad6 | the 已粘贴 mark |
| `--hl` | #f1dc8a | #5c4c12 | text to look at: a search match (meetings `mark` rule); in the detail, a word the versions disagree on, with an `--ochre` underline so the mark does not rest on the wash alone |
| `--shadow` | as meetings | none | the keys panel only; none in dark, where a raised surface is lighter than the ground and casts no shadow |

`--cinnabar` never fills a row. `--c0`…`--c7` (speaker lanes) and `--night` (the day ruler) are not used: one speaker, no ruler.

## Typography

- Stacks, from `pages/meetings/src/styles.css` `--f-ui` and `--f-mono`: Archivo Variable, then PingFang SC and the other CJK faces, for all words; Spline Sans Mono for machine values. Chinese falls through to PingFang.
- The fonts are three embedded files, latin subset, OFL: `fonts/archivo-wdth.woff2` (weights 100–900, widths 62–125 %), `fonts/spline-sans-mono-400.woff2`, `fonts/spline-sans-mono-500.woff2`. Their `@font-face` rules sit at the top of `styles.css`, as `@fontsource-variable/archivo/wdth.css` and `@fontsource/spline-sans-mono` declare them for the meetings page.
- Sizes, fixed rem: 12 (`--fs-12`, meta and counts), 13 (`--fs-13`, labels, mono), 14 (`--fs-14`, body), 16 (`--fs-16`, a take's text and headings of empty states), 20 (`--fs-20`, the count line and the detail's heading). The meetings page's 10.5, 11, 11.5, 12.5, 13.5 and 15.5 px round to these; nothing is under 12 px.
- Weights 400, 500, 600. The meetings page's 560, 620, 650 and 680 map to 500 and 600; the wide cut (`font-stretch` 108–112 %, meetings `.brand` and `.count-line`) carries the headings' character instead of weight.
- Mono is for timestamps (the row's `HH:MM:SS`, the player's position, the timeline's `+m:ss`), take ids, paths, raw event lines and engine names. A length (`2:36`) and a count (`200`, `2 未送达`) are numbers read side by side: Archivo with tabular figures. This moves the meetings page's `.pill .num` and its `.row-time .dur` out of mono.
- Sentence case; no letter-spacing and no uppercase: the meetings `.day-head h2` uppercase tracking has nothing to do on Chinese and is dropped.

## Layout

- Above 1080 px: the list in a column of `minmax(440px, 44%)`, the detail docked beside it, sticky under the top bar and scrolling on its own. `j`/`k` move the selection and the detail follows; with nothing chosen the newest visible take is selected.
- At 1080 px and below: the list alone; a take opens as its own view with `录音列表` as the back link (the meetings page's `@media (max-width: 1080px)` and `(max-width: 760px)` breakpoints).
- At 760 px and below: the top bar takes two lines (brand, freshness and tools; the search under them), the state pills wrap onto a second line, and a row takes two lines: time, length, state and target, then the text.
- A narrow window is still a desktop window with mouse and keys. Coarse-pointer rules still raise controls to 44 px and inputs to 16 px.
- Spacing is in multiples of 4 px: 24 px page padding (16 px narrow), 32 px between detail sections, 12 px inside a group.

## Elevation & Depth

One strategy: surface steps and hairlines. `--slip` raises the player, pickers and inputs above `--glaze`; `--rule` separates rows. Only the keys panel floats, with `--shadow` in light and none in dark. Focus is `outline: 2px solid var(--cobalt)`, offset 2 px (the meetings `:focus-visible` rule); on a row the ring is drawn inside it.

## Shapes

`--r-sm` 4 px for state tags, match tags, row hover and the tooltip; `--r-md` 7 px for buttons, inputs and selects (the meetings `.btn`); `--r-lg` 10 px for the player and the pickers (the meetings `.up` in `player.css`); `--r-full` for pills, the play button and the row play button. Icons are hand-drawn 10–16 px strokes in `app.js`: search, play, pause, back, theme, the state glyphs; no emoji, no icon font.

## Components

### States

Colour always comes with a shape and a word (the meetings page's rule, `ui.tsx` `StateGlyph`).

| State | Word | Mark | Colour |
|---|---|---|---|
| sent (paste and Enter) | 已发送 | filled circle | `--jade` |
| pasted | 已粘贴 | half-filled circle | `--steel` |
| cancelled | 已取消 | hollow circle | `--ash`, the word too |
| undelivered | 未送达, then its reason 送达失败 / 识别失败 / 被打断 / 不完整 in `--cinnabar` | filled triangle, on an `--ochre-wash` tag | `--ochre` |
| dismissed (an undelivered take the user dismissed) | 已忽略, then its reason in `--ash` | hollow triangle | `--ash`, the word too |
| recording, transcribing | 录音中, 识别中 | dashed circle | `--ochre` |
| no speech | 无语音; the row's text `（无语音）` in italics | dash | `--ash`, the row too |
| before the record existed | 早期录音 | dotted circle | `--ash` |

### List

- The strip `▲ 2 条未送达  只看这些` heads the list while any take is 未送达; pressed, it reads `显示全部`. The count and the link word are two spans with a gap between them, no `·`: a count joined to a control word by `·` reads as one string. Its border is `--ochre`, the 3:1 boundary of a control. It counts every 未送达 take, as the 未送达 pill does and as its filter shows. The menu bar's count is the narrower one: 未送达 takes not yet seen.
- Dismissing is marking read: a dismissed take is 已忽略 and leaves every 未送达 count (the strip, the pill, the day head, the menu bar); its text, audio, resend and copy stay. `全部忽略`, a secondary button beside the strip, dismisses every 未送达 take at once; the strip then gives way to a `--slip` notice `△ 已忽略 7 条  录音和文字都还在，不再算作未送达  撤销  收起`, where `撤销` takes back exactly those takes. A take that fails later is 未送达 again.
- The count line: `200 条录音`; with a query `找到 5 条  共 200`; with a filter `37 条录音  共 200`, the total a smaller `--ash` span beside the count (meetings `.count-line`).
- State filters are pills with counts (meetings `.pill`): 全部, 未送达, 已发送, 已粘贴, 已取消, 无语音, and 已忽略 while any take is dismissed. Target, device and day are native selects whose first option names them: `全部目标`, `全部设备`, `全部日期`.
- Day heads, sticky under the top bar: `今天 · 周二 8/18` and `37 条 · 2 未送达`, the count of 未送达 in `--ochre` (meetings `.day-head`).
- A row, 40 px, one line: `HH:MM:SS` (mono, `--ink-2`), length `m:ss`, the state, the target's short name (`basalt › w2:p5`, the full name in its title), the text's first line with an ellipsis. A delivered take over 60 s clamps its text to two lines. The row is a link (the time is the anchor, stretched over the row); the selected row is `--cobalt-wash`; hover is a flat `--sunk`. A play button, 24 px, replaces the length on hover, on focus and on the selected row.
- A take recording now sits first in its day as `录音中 0:23` with `结束后显示文字`.
- A labelled take ends its row in a 10 px check in `--ink-2`, `已标注` its title and label; an unlabelled row keeps the 12 px column empty, so texts align.
- With a query the row's text is a snippet around the first match, the match in `--hl`: up to 7 characters before it, clipped from their left, and the text after it, cut with an ellipsis; the match itself gives way last, and a match wider than the column ends in an ellipsis inside it. A match found outside the delivered text carries a tag: `原始`, the engine's name in mono, or `重新识别 #n`.
- Older takes load on scroll, 200 at a time; the list ends in `已显示 200 条 · 更早的录音随滚动载入`.

### Detail, in this order

1. Heading: date, weekday and `HH:MM:SS` at 20 px, the state, the reason in the failure-word style (13 px, 500). Under it the meta line, its parts 16 px apart: `时长 2:36`, `原目标 basalt › w2:p5`, `设备 Podium Condenser`, `引擎 funasr`, and `声音 备用输入（主输入没有声音，播放的是备用输入）` when the text came from the backup input: the player then plays that track.
2. The texts, stacked where the take's text always was (see Versions): the take's own text first, then each other version, one row each. A take without text shows `（无语音）`, `录音中，结束后在这里显示文字。` or `正在识别，完成后在这里显示文字。` in its place.
3. Under the stack, `重新识别…` (`重新识别备用输入…` on a take whose text came from the backup input: the track it decodes) opens an inline engine picker: each engine with `本机`, `本机 · 当前引擎`, or `↗ 云端 · 音频会发到火山引擎` in `--ochre`; a cloud engine the config does not name shows greyed with `云端 · 配置没有启用它（asr.engine，或 compare.on 和 compare.engines）`, and a refusal of one reads `没有识别：<engine> 是云端引擎，` with the same words; the button reads `开始识别`, or `发到云端识别` for a cloud engine. Its answer joins the stack as a new row.
4. `标注` (see Labels): the take's ground truth, composed here when no version is exactly right.
5. The player: the meetings `.up` card and transport (`player.css`, from `.up` on): the 40 px play button, `0:48 / 2:36`, the rate button (`1×`, tabular figures), the mute button `静音`, which reads `已静音` while pressed (`--cobalt-wash` with a `--cobalt` border), the key hint `空格 播放 · ← → 跳` in the meta style; all three in Archivo. Under the transport the scrub bar is the take's waveform, 56 px high: played bars `--cobalt`, unplayed `--edge`, a 2 px cobalt playhead, a time tooltip on hover, click and drag to seek, `role="slider"` with arrow keys. A take of 1 s still fills the width with at least 40 bars. A waveform that does not load leaves a flat bar that still seeks and plays, and `读不到波形：<error>。仍然可以播放。` in `--cinnabar` under the player. A take still recording shows `录音中 0:23 · 结束后可以播放` in place of the player.
6. `经过`: one line per event, `+m:ss` (`+h:mm:ss` past an hour, the day and time past a day) then words — `开始 → basalt › w2:p5`, `按 Esc 取消`, `识别完成 · funasr · 159 字 · 0.8 秒`, `已发送 → basalt › w2:p5 · 完整`, `送达失败 → basalt › w2:p5：面板已关闭`. Failures in `--cinnabar`, deliveries in `--jade`. `原始事件` discloses the record's JSON lines.
7. `文件`, collapsed: the WAV, `.txt` and `.events.jsonl` paths in mono.

### Versions

The stack holds every text of the take, in this order: the take's own text (the engine it was adopted from), each other engine of the compare record by name, each re-transcription (`重新识别 #1` then the engine in mono), and `原始` last. Rows are separated by `--rule` hairlines; each is a head line, the text, and its actions.

- The head line: the engine's name in mono (`原始` and `重新识别 #n` in Archivo), then its tags as 12 px words in a `--rule-strong` outline, `--ink-2`: `采用` on the take's own text, `主引擎` on the engine `asr.engine` named when the take was recorded; `↗ 云端` in `--ochre` on a cloud engine; `用时 0.62 秒` in `--ash`, from the take's stop to the engine's answer. At the line's end, `这版对` on every version a label can name.
- The text: 16 px at 68 ch, as the take's text always was; `原始` at 14 px in `--ink-2`, the least read. A word a version heard differently from the take's own text is lit with `--hl` and a 2 px `--ochre` underline; on the take's own text, the words any other engine heard differently. Words are compared as the labels are: each Han character and each run of letters or digits, case, spaces and punctuation ignored, so `Postgres`/`post gress` lights and `GIN`/`gin` does not. A run of lit words with the spaces between them is one mark.
- The actions: `复制` and `重发…`, the resend picker opening under the row that asked (see Resend). On the take's own text of a 未送达 take also `忽略`, a secondary button; on a dismissed one it reads `取消忽略`. The primary is on the take's own text only (`重发…` for a 未送达 take, `复制` otherwise), and only while no picker and no label form is open.
- An engine that failed shows the alert (meetings `.alert`): `识别失败 · doubao`, the error in mono, then `主引擎没有给出文字，这一条用了 funasr 的。` on a failed primary, `这个引擎这次没有给出文字，不能标它对。` on another engine, `存下的文字都没有改动。换一个引擎再试。` on a re-transcription. It has no `这版对` and no actions.
- The take's text without a compare record (compare mode was off) is still a version with `这版对`: the label names it `delivered`.

### Resend

The resend picker, inline under the buttons that opened it (not a dialog: nothing is at risk and the text stays in view), read again at every open. Only the take's own target is preselected; with none, the line reads `选一个目标，这里会写明发到哪里、怎么发。` An app that shows this page is never offered: the frontmost app, which is the browser in use, and an app whose front window is this page, as a window of it behind can be; a take spoken into one shows `原目标正显示着这个页面，发过去会贴进本页`. The send button reads `发送中…` and takes no click while its request is out. Groups in this order: 原目标, or `原目标已关闭 · <full name>`; 最近发过, where a target that cannot be reached now shows greyed and not selectable, with `已关闭`; 面板 by workspace, `workspace › process · folder · pane id`, or `herdr 没有运行` (a recent pane then shows greyed with the same words); 应用, `<应用> — <front window title>`; 剪贴板, `只放进剪贴板`. Then one line, sticky at the picker's bottom so it stays in view beside a target chosen far down the list, `发到 <full name>，粘贴 · 不回车`, the text's first 40 characters in `「」`, the button `粘贴` (`粘贴并按 Enter` when the box is ticked, `放进剪贴板` for the clipboard), the box `粘贴后按 Enter` unticked at every open, and `收起`. An outcome shows in words beside the buttons: `✓ 已复制 86 字`, `✓ 已粘贴到 <应用> · 2:36`, `✓ 已忽略 · 不再算作未送达`, or the error in `--cinnabar`.

### Labels

A label is the take's ground truth: the last row of the take in `labels.jsonl`, written through `POST /takes/api/label` in the shape the compare panel wrote, so `mega-asr-loop`, the calibration dataset and `tune corrections` read the same rows. The take's answer carries its label (`label`, the row, or null), and a list row says `labeled`. There are two ways to give it. `这版对` says a version is exactly right. When none is, the user composes the ground truth: a base version, a reading chosen at each place the versions differ, free edits on top, then `确认为正确文字`, which posts the text as a typed ref; the server names every engine that says the same words.

- `这版对` is a pill (the state filters' shape, 28 px, 44 px under a coarse pointer); pressed it fills with `--ink` and gains a check. A click marks that version alone and saves at once; ⇧ or ⌘ with the click marks it as well, or takes it out; a plain click on a pressed one clears the label. Keys `1`–`9` are the versions a label can name, in stack order; Shift is the ⇧-click. While a label is being saved every `这版对` takes no click.
- The `标注` section is a head line (`标注`, and the key hint `1–2 这版对 · e 改文字 · ⌘↩ 确认` in the meta style, as the player's, hidden under 760 px), a state line, and what the table says:

| State | State line | Below it |
|---|---|---|
| unlabelled | `未标注` and `哪一版全对，点它的「这版对」；都不全对，就在下面拼出正确的文字，再确认` | the pending ground truth, open |
| unlabelled, folded | as above | `拼出正确文字…` |
| marked | `✓ 已标注`, `doubao 这版对` (`funasr、doubao 都对`), the time in `--ash` | the note, if any; `改正确文字…`, `清除标注` |
| composed | `✓ 已标注`, `确认过的正确文字` (`，和 doubao 一样` when the server matched an engine), the time | `正确文字` and the text at 16 px, its words lit against the take's own text; `备注` and the note; `改正确文字…`, `清除标注` |
| composing again | as above | the pending ground truth, from the label's text, named `当前` |
| saved, cleared | the new state | `✓ 已存为正确文字` or `✓ 已存标注` beside the buttons; `✓ 已清除标注` with `撤销`, which posts the label the clear took away as it was made (`✓ 已恢复标注`) |
| save failed | unchanged | `没存上：<why>。标注没有改动，再试一次。` in `--cinnabar`, the why in the page's words (see Errors in words), in the form while it is open, else beside the buttons |
| text from the backup input | `未标注` and `这一条的文字来自备用输入，录音文件里没有这段声音，文字和它对不上：只能写备注` | `备注` and `存备注`; no `这版对` on its versions: the label file takes a note alone for such a take |
| no WAV (under 0.3 s), recording, transcribing | no section: the label file needs the take's WAV | |

#### The pending ground truth

In the pickers' raised `--slip` box, top to bottom:

1. `从这版开始` and a pill per version with text (`当前` first on a labelled take), the base pressed. A pill starts the draft again from that version; a draft it replaces that had been edited stays one `撤销换底` away.
2. `待确认的正确文字`: the text box, 16 px at 68 ch, growing with its text. The places where the versions differ are lit inside it, as on the versions: the words only, the spaces around them plain; a place written by hand keeps a dashed `--ochre` underline and no wash.
3. `不同的地方 · 2 处` (`各版本说的字都一样` when there are none), then a row per place: its number, ten characters before it in `--ash`, a pill per reading — the versions that share it named in mono (`funasr、原始`), then the words, or `（没有字）` where a version has none there — the reading in the text pressed, `手改`, ten characters after it, and `▶` (`听第 1 处`) on a timed take.
4. `备注` and a one-line field, `哪里听错了，比如 yolk 听成了 yoke…`.
5. `确认为正确文字`, `收起`, and the status: `未存` in `--ochre` once touched, `正在存…` on the button while the request is out. The button is secondary on an untouched draft and the view's primary once the draft is touched; the take's `复制` or `重发…` gives the primary up in the same moment.

How the places are found: the base's words that every other version also has, in order, are anchors. Between two anchors, a stretch where every version has the same words (case, spaces and punctuation aside) stays the base's; a stretch where they differ is a place, with one reading per distinct wording. A base with three versions against it gives fewer, longer places than one with a single other version.

How choices and free edits live together, in one text:

- Picking a reading puts its words at the place.
- Typing inside one place makes it hand-written: `手改` is pressed, and picking a reading replaces the hand-written words there.
- `手改` selects the place in the text, so what is typed next replaces it: the way to write a place no version gets right.
- Typing in a stretch outside every place is just text.
- An edit across a place's edge merges everything it touches into one hand-written stretch, and the places in it leave the list.
- Typing is taken in place, without redrawing the box, so an input method keeps its composition.

The draft stays while the user moves to another take and back; `收起` folds it; saving drops it. A page closed or reloaded with a touched draft asks first, with the browser's own prompt. Esc in a field leaves it. ⌘↩ in the box or Enter in the note confirms, once an input method has committed; with both empty `确认为正确文字` sends nothing and says `写下正确的文字或者备注，再确认。` A click on `这版对` while a draft is open saves the draft's note with the marks.

#### Listening to a word or a place

Only on a take with word timing: an engine's answer that carries `words`, `[{text, start_ms, end_ms}]`, times from the take's start (doubao's; see the note at the end of this section). Every version's words take their times from the timed answer, word by word along their longest common subsequence; a word the timed engine did not hear takes the gap between its timed neighbours. Then:

- A click on a word of any version plays the take from 0.15 s before it and plays on. The word stays lit in `--cobalt-wash`, and its span shows under the waveform as a 3 px `--cobalt` line (a wash behind the bars would drop the unplayed bars under 3:1). A drag that selects text plays nothing. Pointer only; the keyboard has the player and `▶`.
- `▶` on a place of the pending ground truth plays that place alone: from the end of the word before it to the start of the word after it, 0.15 s before and 0.3 s after, its line under the waveform.
- A take without timing shows none of this: its words are plain text and its places have no `▶`.

Where the times come from: doubao is asked with `show_utterances: true` (`internal/asr/doubao.go`, `request()`), and its reply's `result.utterances[].words[]` (`{text, start_time, end_time}` in ms from the start of the audio; a Han character is a word; a space between words comes as its own word with `-1` times and is left out) is kept by the stream and stored as `words` on doubao's `EngineResult` in the take's `.compare.json` (`internal/compare/compare.go`), which the Takes API passes through on the answer (`internal/takes/index.go`). A take recorded before that, a take where doubao failed, and a take whose primary was doubao (its text comes from the controller, which keeps no times) have none. The label file never carries them.

### Errors in words

The page never shows the server's English. A delivery's error (a deliver line's `err`) and an action's refusal read as the tables say, after `送达失败 → <目标>：` in `经过` and in a resend's outcome, `没有发出去：` for a refused resend, `没有识别：` for a refused re-transcription, `没存上：` for a label, and `已粘贴 → <目标> · 没有按 Enter：` when the paste landed and the Enter did not. The English stays in `原始事件` and the log.

A refused request answers `{code, error}`, and the page words it by the code; a code it does not know shows the server's `error` as written, in mono, so nothing is hidden:

| Code | The page's words |
|---|---|
| `no_take` | `这一条录音已经不在存储里了` |
| `recording`, `transcribing`, `resending`, `retranscribing` | `还在录音`, `还在识别`, `这条正在重发`, `这条正在重新识别` |
| `delivery_busy` | `另一条正在送达，等它完成再发` |
| `target_gone`, `no_herdr` | `目标已关闭`, `herdr 没有运行` |
| `front_app`, `page_window` | `不能发到最前面的应用：会贴进本页`, `这个应用正显示着本页：发过去会贴进本页` |
| `no_answer` | `这个引擎没有给出这一条的文字，不能标它对` |
| `backup_text` | `这一条的文字来自备用输入，录音文件里没有这段声音：只能存备注` |
| `disk_full`, `no_permission` | `磁盘满了`, `没有写入的权限` |

The two refusals answered 422 read apart by code. `no_text`, a resend of a version with no text, reads `这一版没有文字可发：`, then why, from the version the page sent: `对比记录里没有 doubao 的结果`, `doubao 这次识别失败了`, `没有重新识别 #2`, `重新识别 #2 失败了`, `原始文字是空的` or `送达的文字是空的`. `not_named`, a re-transcription with a cloud engine the config does not name, reads `doubao 是云端引擎，配置没有启用它（asr.engine，或 compare.on 和 compare.engines）`.

A delivery's error is herdr's or the system's words as the record keeps them, and reads by its words:

| The record's words | The page's words |
|---|---|
| `pane_not_found`, `herdr lists no such pane` | `面板已关闭` |
| `herdr socket: …`, herdr's CLI not found or refused | `herdr 没有运行` |
| `no app was frontmost` | `当时没有前台应用` |
| `<App> (pid N) has quit`, `no running application with pid N` | `<App> 已经退出`, `应用已经退出` |
| `<App> did not take focus within …` | `<App> 没有切到前台` |
| `<App> lost focus before Enter` | `粘贴后 <App> 失去了焦点` |
| `another delivery is under way` | `另一条正在送达，等它完成再发` |
| `no space left on device`, `permission denied` | `磁盘满了`, `没有写入的权限` |

### Page states

| State | What shows |
|---|---|
| empty store | `还没有录音` and `按右 Option 开始录音，再按一次结束。每一条都会出现在这里，取消的也在。`, left-aligned in the list column; no filters, no detail |
| first load | `正在载入…` and 8 skeleton rows in the row's grid, in the list's column (two lines under 760 px); shown after 300 ms, kept at least 400 ms |
| no match | `没有匹配「q」` and `清除筛选` |
| megavoice not running | a `--cinnabar-wash` notice `megavoice 未运行  下面是 16:21 的列表，它回来后会自动刷新`, the two parts as separate spans; the freshness line reads `16:21 之后没有更新` in `--ochre`; the last list stays |
| refused (403) | the page alone: `从菜单栏重新打开录音历史` and why |
| live | the freshness line reads `刚刚更新`; a new take appears at the top without moving the selection |

### Keys

Listed under `?` in a small panel: `/` search; `j`/`k` rows; Enter opens; `c` copies the take's text (⌘C with no selection does the same); `r` resend; `t` re-transcribe (nothing while one runs); `i` dismiss a 未送达 take, or take its dismiss back; `1`–`9` mark the nth version a label can name, as a click on its `这版对`, with Shift as a ⇧-click; `e` opens the pending ground truth with the caret at the end of its text; ⌘↩ in it confirms; Space play; `←`/`→` 1 s under 60 s and 5 s otherwise, with Shift 10 s and 60 s; `,` and `.` speed; `u` only 未送达; Esc clears the search, closes a picker, or leaves the narrow detail. Keys act only outside a text field.

### Motion

None: the page is a keyboard tool. Buttons press to `scale(0.97)`; `prefers-reduced-motion` drops even that.

## Choices this record replaces

Each item names a first choice and what replaced it:

1. **The 3 px `--ochre` bar at a 未送达 row's left edge** becomes an `--ochre-wash` tag around the mark and word. A coloured left border over 1 px is on /ccc-design's refuse list; the tag carries the same colour with its shape and word, and survives the selected row's wash.
2. **Filters as pills for target, device and day** become native selects. Targets and days are open sets that grow without bound, and as pills they wrap into several rows in a 44 % column and at 390 px. State stays pills: six fixed values, the filter changed most often.
3. **`--ash` and `--ochre` in light** darken from the meetings page's #65736b and #94650f, which read 4.36:1 and 4.46:1 on `--glaze`, under the floor; **`--rule-strong` as a control border** (1.60:1) gives way to the `--edge` token, which carries control boundaries.
4. **Mono for lengths and counts** (meetings `.pill .num`, `.row-time .dur`) becomes Archivo with tabular figures: they are compared numbers, and mono on small data labels is a refused tell. Times, ids, paths and engine names stay mono.
5. **The meetings type scale and weights** (10.5–15.5 px, 560–680) round to 12/13/14/16/20 and 400/500/600, the floor's scale.
6. **The meetings row hover gradient** (`.row:hover`) becomes a flat `--sunk`: gradient washes are refused.
7. **The strip's number** counts every 未送达 take, so it agrees with the 未送达 pill and with the filter it turns on; only the menu bar counts the unseen ones.
8. **`其他版本` tabs** under the player become the stack under the heading. Comparing is reading side by side; a tab shows one version and hides the words the others disagree on.
9. **The link `在对比页标注`** becomes the `标注` section: the compare panel's labelling is here.
10. **The compare panel's own look** gives way to this record. Every function it had has a place above; what its look gives up:
    - a card per take in one long feed → the list is the feed, one take at a time in the detail;
    - display names `Fun-ASR`, `Doubao 2.0` → the engine names in mono, as the rest of this page and the config write them;
    - a green `✓ correct` fill → `这版对` pressed in `--ink`: green is 已发送 here;
    - two diff colours, yellow for an insertion and pink for a deletion → one mark, `--hl` with an `--ochre` underline, since the row says which side;
    - `Use Doubao 2.0` beside the save button → `从这版开始` pills, and a reading per version at every place the versions differ;
    - a text box and a note field open on every take → the pending ground truth, open on an unlabelled take and folded on a labelled one;
    - `1.30 s after stop` → `用时 1.30 秒`; the header's legend sentence → the `?` panel and the key hint;
    - the waveform decoded in the page with `AudioContext` → the player above, from the server's peaks;
    - system font, `#0a66d8` blue and Apple greys → this record's tokens; English → Chinese.

11. **`改写正确文字…` behind `这版对`** becomes the pending ground truth, open on an unlabelled take: a label is a ground truth, and when no version is exactly right, composing it from the versions is the main way to give one.

## Do's and Don'ts

- Do set every record field as text: a window title or an engine's answer is text others control.
- Do show every take, the empty ones greyed, never hidden.
- Do keep one primary button per view: `重发…` on a 未送达 take, `复制` otherwise, the picker's send button while a picker is open, `确认为正确文字` once the pending ground truth is touched.
- Do write labels only through `POST /takes/api/label`, in its shape: the label rows have readers outside this page.
- Don't put a decision in a toast; outcomes show beside the button that caused them and in the timeline.
- Don't load anything from another host, and don't add a font, colour, size or radius this file does not list.
