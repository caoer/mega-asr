# mega-asr

Local ASR toolbox for the Mac (Apple Silicon). FunASR models on the
llama.cpp/GGUF runtime — CPU only, no Python at inference time.

## Contents

- `cmd/megavoice` — tap-to-talk voice input for the Mac, and the mic server
  for a Linux box (below).
- `cmd/megameet` — the meeting recorder and its server: [docs/megameet.md](docs/megameet.md).
- `cmd/asrbench` — scores ASR output against references (`asrbench score`)
  and cuts a WAV the way megavoice does (`asrbench split`).
- `pages/meetings` — the meetings page megameet registers recordings on.
- `runtime/` — the engine. `funasr-cli-serve.patch` applies to FunASR
  [v1.3.30](https://github.com/modelscope/FunASR/releases/tag/v1.3.30)'s
  llama.cpp runtime; `build-funasr-cli.sh` (macOS, Linux) and
  `funasr-root.nix` (Nix, Linux) build `llama-funasr-cli` with it. The patch
  adds `--hotwords 'a, b'` (the Fun-ASR-Nano 热词列表 prompt),
  `--hotwords-cjk` (hotwords for segments whose plain transcript has
  Chinese), `--serve` (models loaded once, one request per stdin line),
  `--gpu N` (Qwen3 layers on Metal), `--enc-gpu` (the SAN-M encoder on Metal;
  the CPU when Metal lacks an op) and `--threads N` (CPU encoder threads,
  default 8); without them its output is identical to the release binary.
- `bin/download-funasr-model.sh` — upstream's helper that pulls the GGUF
  weights from Hugging Face.
- `docs/README-upstream.md` — upstream runtime documentation.

## The engine root

megavoice and megameet run one directory's `bin/llama-funasr-cli` on the
GGUF weights in its `models/`: Fun-ASR-Nano (`funasr-encoder-f16.gguf` +
`qwen3-0.6b-q8_0.gguf`) and `fsmn-vad.gguf`, from
`FunAudioLLM/Fun-ASR-Nano-GGUF` and `FunAudioLLM/fsmn-vad-GGUF` on Hugging
Face. That directory is the engine root, the setting `asr.funasr.root`
(default `~/.local/share/mega-asr/funasr-root`). Build it there:

```bash
git clone https://github.com/caoer/mega-asr.git
cd mega-asr
root=~/.local/share/mega-asr/funasr-root
```

Linux with Nix (flakes enabled) — the runtime and the weights, pinned by
revision and sha256:

```bash
nix build .#funasr-root -o "$root"
```

macOS (Apple Silicon) — needs git, cmake and Xcode's clang for the build,
and the Hugging Face CLI (`pip install -U huggingface_hub`) for the weights:

```bash
runtime/build-funasr-cli.sh "$(mktemp -d)" "$root/bin"   # prints: built …/bin/llama-funasr-cli
bin/download-funasr-model.sh nano "$root/models"         # the encoder, Qwen3 q8_0 and FSMN-VAD
shasum -a 256 "$root"/models/*.gguf                       # compare with the sha256 pins in runtime/funasr-root.nix
```

Linux without Nix takes the same two scripts; the build needs git, cmake and
gcc and targets the host's CPU. An engine root elsewhere is named in the
config: `megavoice config set asr.funasr.root DIR`.

## Usage

```bash
# 16 kHz mono wav in, plain text out; --vad handles long audio
cd ~/.local/share/mega-asr/funasr-root
ffmpeg -i input.m4a -ar 16000 -ac 1 -acodec pcm_s16le input16k.wav
bin/llama-funasr-cli \
  --enc models/funasr-encoder-f16.gguf \
  -m models/qwen3-0.6b-q8_0.gguf \
  --vad models/fsmn-vad.gguf \
  -a input16k.wav
```

## Output

The CLI runs on the CPU and writes plain text. Speaker labels come from the
PyTorch Fun-ASR-Nano-2512 pipeline (FSMN-VAD + cam++ + ct-punc) on a GPU box.

## megavoice — tap-to-talk voice input (macOS)

`cmd/megavoice`: tap a modifier alone to start recording; the transcript goes
where you were typing when the recording started (herdr's focused pane, or
the app window and field), wherever focus is afterwards. Tap the modifier
again to paste it; press Enter instead to paste and send (the Enter itself
never reaches the app — it is pressed in the target after the paste). Enter
during "transcribing…" turns a pending paste into a send; Esc cancels, and
the take and its text are kept: the tap key held with Esc pastes it back
(below). Every take ends in a state written on disk, and the Takes page
lists every take.
Capture is this Mac's input — the system default, or one picked in the menu's
Input submenu (`capture.source = "local"`, `capture.mic`) — or an ALSA device
on another host over ssh, such as channel 2 of a ReSpeaker XVF3800 on a
Linux box (`capture.source = "ssh"`); the pick applies at the next take. ASR is the
engine root's `bin/llama-funasr-cli` (Fun-ASR-Nano + FSMN-VAD). Pure Go, no cgo (purego
drives AppKit/CoreGraphics).

- **Whole takes, streaming:** a take is written to its WAV block by block
  as it arrives. Up to `take.whole_max` (60 s), nothing is decoded while
  you speak: at the stop the take, minus its silent head and tail, goes to
  the model as one window, so no cut lands inside a sentence. A longer take streams: it is
  cut into chunks — at a pause of 1.9 s once the chunk holds 3 s, or at its
  quietest block by 30 s — and one resident `llama-funasr-cli --serve`
  process (models loaded once, Qwen3 on Metal) transcribes them in the
  background, so a stop waits only for the last chunk. Chunk texts, and the
  VAD segment texts inside the CLI, are joined in order (no space next to
  CJK, one space between Latin words) before corrections and filler cleanup
  see the whole text. If the resident process fails on a chunk, the CLI runs
  once for it; if it will not start, the CLI serves alone for a minute and
  then a fresh process is tried.
- **Engines** (menu › Engine, `asr.engine`, after a restart): `funasr`, the
  above, on this Mac; or `doubao`, Doubao ASR 2.0 on the Volcengine Agent
  Plan — each take streams to it as it records (200 ms packets over one
  WebSocket, two-pass), so its text lands soon after the stop. Speech
  goes only to the plan endpoints (`wss://openspeech.bytedance.com/api/v3/plan/sauc/…`;
  any other `asr.doubao.url` is refused — elsewhere is billed apart from the
  plan). The key stays in its own file (`asr.doubao.key_file`), read at each
  take. If a take's stream fails (offline, server error, no answer 10 s after
  the stop) funasr transcribes its WAV and that text is delivered.
- **Compare mode** (menu › Compare Mode and Compare Engines, `compare.*`,
  read at every take — no restart): each take also goes to the checked
  engines beside the primary (`asr.engine`), streaming or from the WAV once
  it ends. The primary delivers as always and never waits for them; a
  failing or hung one only shows its error. Every engine's text (after
  corrections and filler cleanup, and raw) and its stop-to-text latency land
  in `<take>.compare.json` beside the take; doubao's answer there also
  carries `words` (`[{text, start_ms, end_ms}]`, times from the take's start).
- **Labels** (on the Takes page, below): a take's detail stacks every
  version of its text — the take's own, each compare engine's, each
  re-transcription's, the raw text — with the words the engines disagree on
  lit. When a version is exactly right, its 这版对 labels the take in one
  click, saved at once with that version's text as the transcript (⇧-click
  marks another as well; clicking the pressed one again unmarks it, and
  unmarking the last one clears the label). Otherwise compose the right
  text — a base version, a reading picked at each place the versions
  differ, free edits — and/or write a note, and 确认为正确文字 (⌘↩). On a take
  with doubao's word timing a click on a word plays the take from it. Each
  save appends a row to `~/.local/share/megavoice/labels.jsonl` — an
  asrbench manifest row `{id, wav, ref, lang, dur_s, source: "megavoice-label"}`
  plus `delivered`, `primary`, `delivered_by`, `engines {name: {text, raw,
  latency_ms, error}}`, `note`, `correct` (the engines whose text is the
  `ref`, ignoring spacing, punctuation and case, the clicked ones first;
  `delivered` for a take without a compare record), `label_source` (`click`,
  `typed`, or `cleared`, a row with no `ref` that unlabels the take),
  `labeled_at`. Older takes can be labelled
  too. The file is append-only: a take's last row is its label, and the rows
  with a `ref` (last per id) are an eval manifest for `asrbench score` and
  labels for mega-asr-loop (below). The take's own `.txt` is never changed. A
  take whose text was decoded from the backup input takes a note alone.
- **Takes overlap:** a tap while earlier takes transcribe starts a new take.
  Each take keeps its own target; texts are delivered in recording order.
  Enter goes to the recording take, else to the newest one still on its way;
  Esc cancels only the recording take (its audio and text are kept, nothing
  is pasted). A take stops by itself after 30 min.
- **Overlay:** bar colour is the block's level above the noise floor (blue
  ≥ 18 dB, amber 10–18, grey below — poor pickup); red bars and "clipping"
  when a sample hits full scale. While a take transcribes it shows
  "transcribing 0:07 (~0:03 left)"; the estimate is this machine's recent
  seconds of ASR per second of audio, shown once 3 chunks were measured.
- **Durability:**

  | after | audio lost at most | text |
  |---|---|---|
  | stop, Enter, auto-stop | nothing | delivered |
  | ASR failure on a chunk | nothing | the rest delivered, "transcription incomplete"; no `.txt`; `megavoice retranscribe` is the retry |
  | SIGTERM or SIGINT (`launchctl kickstart -k`, logout) | nothing | within 3 s every take in flight has its WAV header patched and `hold: interrupted` on its record; megavoice exits 75, and launchd starts it again |
  | app crash, `kill -9`, panic | the last 20 ms block plus what the ssh pipe held | at the next start the WAV header is repaired and the take transcribed and saved, not delivered |
  | ssh drop, remote host reboot, cable | what the remote host had not yet sent | what arrived is delivered, "stream cut" |
  | power loss on the Mac | the above plus what the file system had not flushed (a take is fsynced at its stop) | as a crash |
  | disk full | the block that failed | the take stops, "cannot save the recording" |

  At each start megavoice reads the records of the last 7 days and marks
  every take the last exit cut, once: `recover` and `hold: interrupted` on
  its record (未送达 · 被打断), its text saved, never sent on its own; the
  overlay says `上次被重启打断 m:ss · 未送达 · 见录音历史` (`N 条` for more
  than one take).

- **Final states:** every take ends in one state, written to its record
  (`<ts>.events.jsonl`, contract I1 below) before the overlay reports it:
  已发送 (sent), 已粘贴 (pasted), 已取消 (cancelled), 无语音 (no speech), or
  未送达 (undelivered) — 送达失败 (the delivery failed; the text is on the
  clipboard), 识别失败 (ASR failed), 被打断 (an exit cut it), 不完整 (it went
  out with failed chunks). An outcome flashes for 1.5 s, a failure for 6 s.
  Only undelivered takes are counted: the number beside the menu bar symbol
  and the menu row `打开录音历史 · 2 条未送达`, until the take is resent,
  opened on the Takes page, or dismissed there. A take that becomes
  undelivered plays the system alert sound once; nothing else makes a sound.
  A take an exit cut while its paste went out (`delivery_cut`) most likely
  landed and is never counted. Deliveries — a take's own, the hotkey's, the
  page's, a command's — take one lock from the focus change to the Enter, so
  two never interleave.
- **The resend hotkey:** hold the tap key and press Esc (Right Option + Esc
  by default). With megavoice idle it pastes, into what has focus and
  without Enter, the newest undelivered take of the last 10 minutes, else
  the newest cancelled take of 1 s or more with text, and flashes
  `已粘贴 2:17「…」→ 当前光标`. A dismissed take, or one cut while its paste
  went out, is never the one. A pasted take is delivered, so the next press
  reaches the take before it. While a take records or is still on its way
  the chord only flashes why (`还在识别`; a cancelled take with no words:
  `没有识别到文字`). The chord's Esc never reaches the app, and a plain Esc
  is told apart by the tap key's state as the HID system reports it at the
  Esc. A cancel flashes `已取消 2:17 · 右Option+Esc 重贴`, naming the
  configured tap key.
- **The menu while a take records:** `● 录音中 m:ss`, 停止并粘贴 and 取消录音.
  Restart or Quit chosen then asks first: 停止并粘贴，然后重启, 取消录音，然后重启
  (Return is the first button), or 继续录音 (Esc); after a choice that ends
  the take, megavoice waits up to 30 s for its text and delivery.
- **Takes page** (menu › 打开录音历史, or `megavoice takes url`,
  `http://127.0.0.1:7865/takes/#k=<key>`; the listener's root sends a browser
  there with its `#k=`): served by megavoice at `/takes/` on the loopback
  listener (`compare.addr`),
  plain HTML, JS and CSS embedded in the binary; it loads nothing from
  another host. Every take, newest first and grouped by day: time, length,
  state, target and the text's first line; a take without speech is greyed,
  never hidden. A search over every text of a take (substrings, so Chinese
  needs no word breaks), filters by state, target, device and day, and the
  strip `2 条未送达 · 只看这些` with 全部忽略 (撤销 takes it back). A take's
  detail: the delivered text, the raw text, each compare engine's and each
  re-transcription's answer, each with 复制 and 重发; a player whose scrub
  bar is the waveform; 重新识别 with an engine picker, a cloud engine marked
  `云端 · 音频会发到火山引擎` and refused unless `asr.engine` or
  `compare.engines` names it; the take's events as a timeline. 重发 offers the
  take's own target while it exists, recent targets, herdr's panes, running
  apps (never one whose front window shows the page, the browser in use
  among them) and the clipboard, names the target before it sends, and presses
  Enter only when asked. A take recorded before the record existed shows as
  早期录音.
- **The listener's guard** (the Takes page): a request
  is answered only when its Host is a loopback address with the listener's
  port and its `Sec-Fetch-Site` is `same-origin` or `none`; every text, audio
  file and action also needs the key as the one `X-Megavoice-Key` header. The
  key is 128 random bits in `~/.local/state/megavoice/takes.key` (0600); it
  changes at every start of `megavoice serve` and on `megavoice takes url
  --rotate`. The menu opens a page with the key in the address's fragment
  (`#k=`), which the browser never sends and the page keeps in the tab's
  sessionStorage. An action is a POST of JSON (anything else gets 415);
  every response carries a Content-Security-Policy naming only the page's
  own origin and `Referrer-Policy: no-referrer`; a refusal reads
  `从菜单栏重新打开录音历史`. The guard is for web pages and other browser
  origins on a single-user Mac, not for other local accounts.

- **Install:** build the app bundle and install it at a stable path under a
  stable signature (`scripts/install-app.sh` needs an Apple Development
  identity — Xcode → Settings → Accounts, a free Apple ID works — or one
  named in `MEGAVOICE_SIGN_IDENTITY`):

  ```bash
  CGO_ENABLED=0 go build -o /tmp/megavoice ./cmd/megavoice
  scripts/bundle.sh /tmp/megavoice /tmp/MegaVoice.app
  scripts/install-app.sh /tmp/MegaVoice.app   # → ~/Applications/MegaVoice.app
  ```

  With Nix instead: `nix build .#megavoice`, then
  `result/libexec/megavoice/install-app.sh result/libexec/megavoice/MegaVoice.app`.
  The LaunchAgent `app.0xdao.megavoice` (the bundle id) runs
  `~/Applications/MegaVoice.app/Contents/MacOS/megavoice serve`; its `PATH`
  must reach `herdr` for delivery into herdr panes. A later
  `install-app.sh` asks it for `megavoice restart --when-idle`: megavoice
  restarts once nothing has been in flight and no key was pressed for 3 s,
  and from that moment a tap flashes `正在重启`:

  ```bash
  cat > ~/Library/LaunchAgents/app.0xdao.megavoice.plist <<EOF
  <?xml version="1.0" encoding="UTF-8"?>
  <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
  <plist version="1.0"><dict>
    <key>Label</key><string>app.0xdao.megavoice</string>
    <key>ProgramArguments</key><array>
      <string>$HOME/Applications/MegaVoice.app/Contents/MacOS/megavoice</string><string>serve</string>
    </array>
    <key>EnvironmentVariables</key><dict>
      <key>PATH</key><string>$HOME/.local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
    </dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ProcessType</key><string>Interactive</string>
    <key>StandardOutPath</key><string>$HOME/Library/Logs/megavoice.log</string>
    <key>StandardErrorPath</key><string>$HOME/Library/Logs/megavoice.log</string>
  </dict></plist>
  EOF
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/app.0xdao.megavoice.plist
  ~/Applications/MegaVoice.app/Contents/MacOS/megavoice status   # grants, tap and state
  ```
- **Permission:** Accessibility for MegaVoice, once. It survives rebuilds
  because the signing identity and bundle id stay the same.
- **Delivery:** the target fixes the path. In front of a herdr window, the
  focused pane gets the text in one `pane.send_input` request on herdr's
  socket, which herdr wraps in bracketed-paste markers if the pane's program
  turned that mode on, so the program reads a paste, not typed keys. Any
  other app gets a paste into the window captured at the start, brought back
  to the front first if needed, and the clipboard keeps the text. To send,
  Enter follows the paste: `herdr pane send-keys <pane> enter` for a pane, a
  Return posted to the app for a window, only while that window has focus.
- **Vocabulary:** `~/.config/megavoice/hotwords.txt` (one term per line; into
  the ASR prompt for Chinese speech only) and `corrections.tsv`
  (`mishearing<TAB>intended`, applied after ASR; a lower-case mishearing
  matches any case, Latin edges match whole words, `zh:` limits a rule to
  Chinese context — the file's header has the rules). Both are seeded on first
  start, read at every recording, and yours to edit.
- **Filler cleanup:** after corrections, `internal/post/filler.go` drops
  嗯/呃/um/uh, stutter repeats and VAD-break full stops; it only deletes (or
  turns 。 into ，) and delivers the raw text if that ever fails. Gold set,
  scorer and the reference `rules.py`: `internal/post/testdata/filler/`.
- **Kept:** every take, the delivered text and the uncorrected ASR text,
  `~/.local/share/megavoice/utterances/<ts>.{wav,txt,raw.txt}`, the input
  device that captured it as `<ts>.input.json` (`source` local/ssh/remote;
  local: `name`, `uid`, `transport`; ssh: `host`, `pcm`; remote: the box's
  `name` and `host`; `channel`), `<ts>.compare.json` in compare mode, the
  take's record `<ts>.events.jsonl` (contract I1 below), and
  `backup/<ts>.wav`, the backup input's track (`capture.backup`), kept only
  when the main input was dead somewhere in the take. A take under 0.3 s is
  a mistaken tap: its audio is removed, its record stays, and its name is
  never used again. Chunk WAVs live under
  `~/.local/state/megavoice/chunks/` until their take's text is saved.
- **Commands:** `megavoice status | toggle | cancel | record [--seconds N] OUT.wav | transcribe [--plain] [--engine funasr|doubao] WAV`
  (`status` prints the running agent's `build`, and `restart_pending` while
  a `restart --when-idle` waits; `cancel` keeps the take as cancelled;
  `transcribe` runs the engine once on the whole file; doubao takes it
  unpaced, faster than real time).
  `megavoice restart [--when-idle] | quit` ends the agent between takes;
  `--when-idle` returns at once and the agent exits once nothing has been in
  flight and no key was pressed for 3 s. A refused command exits 1, a
  refused restart or quit 75, and with no agent running they exit 69.
  `megavoice takes [-n N] [--undelivered] [--json]` lists the newest takes
  (name, length, state, target, text); `megavoice events ID` prints a
  take's record; `megavoice resend [ID] [--to front|pane:<id>|app:<pid>:<bundle id>|clipboard] [--send] [--text delivered|raw|<engine>|"retranscription N"]`
  delivers a kept take's text again (default: the newest undelivered take,
  pasted where the focus is, no Enter); `megavoice retranscribe ID --engine
  funasr|doubao` decodes a take's audio again and appends the answer to its
  record, changing no stored text; `megavoice takes url [--rotate]` prints
  the Takes page's address with its key.
  `megavoice script [--plain] [DIR]` is the script check: the scripted takes
  `DIR/script.tsv` names (`DIR` defaults to `store.data`) transcribed and
  scored, errors per clip.
  `megavoice replay [-data DIR] [-out DIR] [-chunks DIR] WAV...` streams WAVs through the
  controller at real time without the app and prints each text with its
  stop-to-text time; with `--set compare.on=true` it writes the compare
  records too; `-chunks DIR` writes each take's chunks to `DIR/<name>/`:
  `chunk-NNN.wav` and `chunks.tsv` (sample offsets, both edges' kinds —
  `start`, `pause`, `max`, `tail`, `whole` — and each chunk's text).
  `asrbench split [-whole D]` cuts a WAV the same way into the same columns
  (`cuts.tsv`). `megavoice version` and `asrbench version` print
  `audio.ChunkerVersion`, which names the chunker's cuts; `megavoice version`
  prints the build id and the commit too. `megavoice panel [-data DIR] [-labels FILE] [-addr HOST:PORT]` serves the Takes
  page with its labels over any take directory (a replay's), without resend and
  re-transcription, and prints its address with the key. Log: `~/Library/Logs/megavoice.log`.
- **Settings:** see [Settings](#settings) below.

### Settings

megavoice reads one TOML file, `~/.config/megavoice/config.toml`
(`$XDG_CONFIG_HOME/megavoice/`, beside `hotwords.txt` and `corrections.tsv`).
Every setting has a default in code (`internal/app/config.go`); the file
overrides it key by key, and `--set section.key=value` overrides the file for
one run. An absent file means the defaults. `--config FILE` or
`MEGAVOICE_CONFIG` names another file, which must then exist. The same file
carries megameet's `[meeting.*]` tables (listed in `megavoice config init`'s
template); megavoice validates them, its menu bar reads and writes
`meeting.capture.mic` (see docs/megameet.md § From the menu bar), and its
`config show` prints only its own tables.

```bash
megavoice config init            # write the commented default config (kept if it exists)
megavoice config check [FILE]    # validate; an unknown key or a bad value is an error naming the key
megavoice config show            # the settings in effect, each marked default | file | flag
megavoice config set KEY VALUE   # write one key, keeping the file's other lines and comments; refused if the result is invalid
megavoice --set asr.funasr.mode=exec transcribe take.wav   # one-run override
```

| key | default | |
|---|---|---|
| `tap.key` | `"right_option"` | the modifier whose lone tap starts and stops a take (`right_option`, `right_shift`) |
| `tap.window` | `"400ms"` | longest press that is a tap |
| `tap.quiet` | `"500ms"` | no start this soon after another key press |
| `capture.source` | `"local"` | `local`: an input of this Mac; `ssh`: an ALSA device on another host, read by `arecord` over ssh. Each source's keys are checked only while it is the source |
| `capture.mic` | `"default"` | local: an input's UID (`megavoice mics`); `default` follows the system's input |
| `capture.mic_name` | `""` | local: the pinned input's name, shown while it is not connected ("Podium Condenser not connected — using …"); the menu writes it with `capture.mic`; `""` shows the UID |
| `capture.mic_channel` | `0` | local: `0` mixes every channel, `n` records channel `n` |
| `capture.backup` | `"auto"` | local on a Mac: a second input recorded beside the main one; a stretch of 2 s or more in which the main input is all zero samples while this one carries a voice is decoded from this one. `auto`: a wired input other than the main one, a display's or camera's microphone before the built-in (not with the lid closed); an input's UID pins one; `""` or `off` records none. Never a Bluetooth or wireless input; the record's `backup_off` says why none was recorded |
| `capture.host` | `""` | ssh: destination with the device, e.g. `micbox` |
| `capture.device` | `""` | ssh: ALSA PCM on that host, e.g. `hw:Array,0`, or a dsnoop PCM that shares the device |
| `capture.channel` | `2` | ssh: 1–6; 2 is the XVF3800's ASR beam |
| `take.max_duration` | `"30m"` | a take stops on its own after this |
| `take.chunk_pause` | `"1900ms"` | a pause this long ends a chunk |
| `take.chunk_max` | `"30s"` | a chunk is cut at its quietest block by this length |
| `take.whole_max` | `"1m"` | a take this long or shorter is decoded whole at stop; a longer one streams in chunks; `"0s"` streams every take |
| `asr.engine` | `"funasr"` | `funasr` (local) or `doubao` (cloud, funasr standing in when a take's stream fails); each engine has its own `[asr.<engine>]` table |
| `asr.funasr.root` | the build's `asrRoot`; else `"~/.local/share/mega-asr/funasr-root"` (`$XDG_DATA_HOME/mega-asr/funasr-root`) | the engine root: `bin/llama-funasr-cli` and `models/` ([The engine root](#the-engine-root)) |
| `asr.funasr.llm` | `""` | a Qwen3 GGUF used in place of the root's `models/qwen3-0.6b-q8_0.gguf` — your fine-tune's export (mega-asr-loop's `recipes/funasr-nano/`, below); the encoder, VAD and CLI still come from the root. Must be an existing file; `""` (or removing the key) is the base model. megameet's drain uses it too and records it on each record's engine; `megameet score --root` ignores it (a root is a whole engine) |
| `asr.funasr.mode` | `"resident"` | `resident`: one `--serve` process; `exec`: one CLI run per chunk |
| `asr.funasr.gpu_layers` | `99` | Qwen3 layers on Metal; 0 decodes on the CPU |
| `asr.funasr.encoder` | `"gpu"` | the SAN-M encoder on Metal, or `cpu` |
| `asr.funasr.vad` | `"gpu"` | FSMN-VAD on Metal, or `cpu` |
| `asr.funasr.threads` | `8` | encoder threads when it runs on the CPU |
| `asr.funasr.llm_threads` | `0` | Qwen3 threads on the CPU (`--llm-threads`); `0` passes no flag, and the CLI uses its own default of 4 |
| `asr.doubao.url` | `"wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async"` | a plan ASR endpoint; anything outside `wss://…/api/v3/plan/sauc/` is refused |
| `asr.doubao.resource_id` | `"volc.seedasr.sauc.duration"` | ASR 2.0 on the plan |
| `asr.doubao.key_file` | `"~/.config/megavoice/doubao.key"` (`$XDG_CONFIG_HOME/megavoice/doubao.key`) | the file holding the plan key; must hold one when doubao is the engine or a compare engine |
| `asr.doubao.key_var` | `""` | the `KEY=value` line in `key_file` that holds it; `""` when the file is the key alone |
| `asr.doubao.two_pass` | `true` | `enable_nonstream`: once a sentence ends, the server decodes it again with its non-streaming model and that text replaces the streamed one |
| `asr.doubao.ddc` | `false` | `enable_ddc`: the server drops fillers and repeats |
| `asr.doubao.hotwords` | `true` | `hotwords.txt` sent with each take (`request.corpus.context`) |
| `post.join_words` | `[]` | words that end a clause, not a sentence: a full stop the VAD put right after one, before more Chinese text, becomes a comma. They add to the built-in list (就是 然后 因为 但是 而且 或者 所以); megameet's transcript chain reads the same key |
| `deliver.herdr_apps` | `["com.github.wez.wezterm"]` | bundle ids of the terminals herdr runs in |
| `store.data` | `"~/.local/share/megavoice/utterances"` | every take's WAV and texts |
| `store.labels` | `"~/.local/share/megavoice/labels.jsonl"` | labels saved from the Takes page |
| `compare.on` | `false` | compare mode; read at every take |
| `compare.engines` | `["funasr", "doubao"]` | the engines run beside `asr.engine` (which never runs twice) |
| `compare.addr` | `"127.0.0.1:7865"` | the Takes page's listener; a loopback IP only, no host name |

Design choices:

- **TOML via BurntSushi/toml:** its decode metadata reports which keys a
  file set (the per-value source in `config show`) and which keys matched
  nothing (the strict unknown-key error).
- **No settings in environment variables:** the file is the one place a
  setting lives, so an agent's edit to it cannot be silently beaten by a
  stale variable in a launchd plist or a shell. The old `MEGAVOICE_*`
  variables are ignored with a warning naming the key that replaces each.
- **Durations are strings with a unit:** a bare `400` is refused rather than
  read as nanoseconds.
- **Sections by stage** (`tap`, `capture`, `take`, `asr`, `post`, `deliver`,
  `store`, `compare`); engine settings under `[asr.<engine>]`, so another engine, a
  models directory or adapter paths are new keys, not a new layout.
- **The package owns the engine path; the file owns choices:** `package.nix`
  accepts `asrRoot` and compiles it in as the default `asr.funasr.root`.
  Whoever packages megavoice sets the path there once; the file is for what
  a user picks, such as the tap key or the capture source.

### Remote mic (Linux)

A mic on a Linux box, such as a ReSpeaker XVF3800, becomes an input
of the Mac by pairing, no ssh key needed. On the box, megavoice's Linux build
runs `megavoice mic serve`: it records `[capture]` on that host (`arecord`,
one per stream) and serves it over TLS on `mic_server.listen` (`:7866`) to
the Macs paired with it. `megavoice mic pair` on the box prints an 8-digit
PIN, single-use, valid 2 minutes; on the Mac, MegaVoice › Input › Add remote
mic… takes the box's address and that PIN, and the box then shows up in the
Input submenu. The PIN is the whole check: a PAKE proves both ends know it
and pins the box's certificate, so there is no fingerprint to compare.

A box the Mac can ssh to pairs with no PIN: the Mac runs `megavoice mic
token NAME` there over ssh, which pairs the Mac as `NAME` and prints one JSON
line — `{"box", "client", "token", "fingerprint", "port"}` — that the Mac
pins; the audio still comes over the server's TLS stream. Running it again
re-pairs `NAME`: the old token stops working and its live streams end.

The server advertises itself on the LAN by mDNS/DNS-SD — service
`_megavoice-mic._tcp` in `local.`, instance its name, its listen port, TXT
`v=1` — on the non-loopback interfaces it listens on, so Add Remote Mic…
lists it; `dns-sd -B _megavoice-mic._tcp` on a Mac shows the same. The box's
firewall must pass UDP 5353 on the LAN interface.

```bash
megavoice mic serve          # the service runs this
megavoice mic pair           # a PIN for the next Mac, and when it expires
megavoice mic token NAME     # pair the Mac NAME, print its token line (ssh pairing)
megavoice mic status         # address, name, capture, paired and live clients
megavoice mic clients        # each Mac: paired, last seen, live streams
megavoice mic revoke NAME    # unpair a Mac; its live streams end
```

On Linux `capture.source = "local"` is an ALSA PCM on this host:
`capture.mic` names it (`default` is ALSA's), and `capture.mic_channel`
picks the channel. `0` records the PCM in mono, ALSA mixing it, which works
for any device; `n` opens the PCM as the XVF3800's six channels and records
channel `n` (2 is its ASR beam), so any other device keeps `0` — a PCM that
is not six channels fails naming the device and this rule. For the array
through a dsnoop PCM that shares it between readers, named here
`array_shared`:

```toml
[capture]
mic = "array_shared"
mic_channel = 2
```

| key | default | |
|---|---|---|
| `mic_server.listen` | `":7866"` | host:port the paired Macs reach; `:7866` is every interface |
| `mic_server.name` | `""` | the name a pairing Mac sees; `""` is the hostname |
| `mic_server.state` | `"~/.local/state/megavoice/mic"` | TLS key (0600) and paired clients, in a 0700 directory (`$XDG_STATE_HOME/megavoice/mic`) |
| `mic_server.socket` | `""` | the control socket; `""` is `ctl.sock` in `state` |
| `mic_server.admin_group` | `"megavoice-mic"` | who may use the control socket, besides root and the server's own user |

The Linux build finds its file as the Mac's does (`--config`,
`$MEGAVOICE_CONFIG`, `~/.config/megavoice/config.toml`), then falls back to
`/etc/megavoice/config.toml`, the system service's. `pair`, `token`,
`status`, `clients` and `revoke` reach the server on `mic_server.socket`
(else `ctl.sock` in `mic_server.state`), so `sudo megavoice mic pair`, and
root over ssh, find a system service with no flags. The server admits a
caller by the credentials the kernel reports for the socket (`SO_PEERCRED`,
`SO_PEERGROUPS`): root, its own user, or a member of `mic_server.admin_group`
by primary or supplementary group; anyone else is refused before the command
runs. A system service keeps its state directory 0700 and puts the socket in
a directory the group can reach (for example `/run/megavoice-mic/ctl.sock`,
socket 0660, group `megavoice-mic`).

**Install.** NixOS: the flake's `megavoice` package for Linux, run as a
system service — for example a user `megavoice-mic` in `audio`,
`/etc/megavoice/config.toml`, state in `/var/lib/megavoice-mic`, the socket
in `/run/megavoice-mic`, port 7866 open on the LAN interface only. Any
other Linux host, with `arecord` installed (`alsa-utils`):

```bash
CGO_ENABLED=0 go build ./cmd/megavoice      # or: nix run .#megavoice -- mic serve
install -Dm755 megavoice ~/.local/bin/megavoice
megavoice config init                        # then set [capture] and [mic_server]
install -Dm644 packaging/megavoice-mic.service ~/.config/systemd/user/megavoice-mic.service
systemctl --user daemon-reload && systemctl --user enable --now megavoice-mic
loginctl enable-linger "$USER"               # keep serving while logged out
```

The user needs to open the capture device (group `audio` on most
distributions), and the port must be reachable from the Mac's network.

## Improving recognition on your voice: mega-asr-loop

The loop that measures and improves recognition on your own recordings lives in its own repo,
[mega-asr-loop](https://github.com/caoer/mega-asr-loop): inventory and audio quality,
references, error classes and their fixes, selection, a prediction sealed before any number, corrections,
hotwords and settings candidates, the LoRA fine-tune of Fun-ASR-Nano (`recipes/funasr-nano/`), and verify on
held-out takes. Its local runtimes (`mega_asr_loop/engines/runtime.py`) run this repo's engine. This repo
keeps the Go runtime, `cmd/asrbench` and `internal/score`, the engine build and the packaging.

The loop runs without megavoice and relies on five contracts with it. Each is a fixture test there
(`docs/megavoice.md` names them); a change to one here breaks that test, so change it on both sides:

1. **The store** (I1): `<ts>.wav`, `.txt`, `.raw.txt`, `.input.json`, `.compare.json` and `.events.jsonl`
   under `store.data`, `labels.jsonl` beside it, and the serve log's `session: recording <ts>[ from
   <device>]`, `session: <ts>: target locked: <target>, <n> ms after the tap` and `serve: asr <engine>` lines
   (a build from before the record logs `session: recording <ts>[ from <device>]; target locked: <app>`).
   `<ts>.events.jsonl` is the take's record: append-only, one JSON object per line, `{"v":1,"at":…,"ev":…}`
   and the ev's fields. `v` on a line is the event format's version, not the take row's schema `v`; `at` is
   RFC 3339 with milliseconds and offset. The evs: `start` (`trigger`, `tap_at`, `target {kind, pane,
   workspace, app, pid, bundle_id, title}`, `engine`, `model`, `build`, `prev {id, gap_s}`), `mic_live`,
   `no_signal`, `backup_on` (`after_s`), `backup_off` (`after_s`, `why`), `stop` (`kind`
   tap/enter/cancel/auto/stream_end/write_error/shutdown, `why`, `via` menu/ctl, `dur_s`), `send_on`, `text`
   (`engine`, `raw_chars`, `chars`, `failed_chunks`, `latency_ms`, `stream_err`, `sha256`, `audio`), `hold`
   (`why` cancelled/deliver_failed/asr_failed/interrupted/delivery_cut/empty), `deliver` (`n`, `via`
   auto/hotkey/page/cli, `caller`, `target`, `ok`, `err`, `chars`, `submit`, `source`, `text`), `received`
   (`n`, `received` whole/short/unknown, `why`), `recover`, `retranscribe` (`n`, `engine`, `model`, `raw`,
   `text`, `latency_ms`, `err`, `audio`), `seen`, `dismiss` and `undismiss` (the later of the two decides).
   A line that does not parse, such as one a crash cut, is skipped. A take under 0.3 s keeps its record and
   loses its WAV.
2. **Replay** (I2): `megavoice [--config F] [--set k=v]... replay [-data DIR] [-out DIR] [-chunks DIR] WAV...`, each text
   printed and in `-out/<name>.txt`, and the log lines `session: recording <ts>` and `session: <ts>: target
   locked: replay <name>, <n> ms after the tap` mapping a replay take to its WAV; `--set compare.on=true`
   writes compare records; `megavoice config show` prints the effective settings.
3. **Settings and vocabulary files** (I3): the keys a candidate may set (`asr.funasr.llm`,
   `asr.doubao.two_pass`, `asr.doubao.ddc`, `asr.doubao.hotwords`, `take.whole_max`), and `hotwords.txt`,
   `corrections.tsv` (its matching rules) and `vocabulary.tsv` under `$XDG_CONFIG_HOME/megavoice/`.
4. **The FunASR root** (I4): `bin/llama-funasr-cli` and `models/*.gguf`, and `--serve`'s protocol: `[ready]`
   on stderr, then one `wav<TAB>hotwords[<TAB>whole]` line in, one text line out, `[done]` on stderr.
5. **A tuned LLM** (I5): a `qwen3-0.6b-q8_0.gguf` goes in through `asr.funasr.llm`; the encoder stays the
   root's.
