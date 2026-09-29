# megameet

megameet records a meeting on a Mac, registers it on a ccc-pages page (the registry), and processes it on a server: pull, process, align, and an optional ingest into a wiki. This page holds what an operator and a reader of megameet's output need: the directory a record becomes, the drain's states and claims, labels, deletion, and how to run your own.

## Directory contract

`megameet pull <id>` materializes a registry record as a directory at `<meeting.data>/pull/<id>/`; `megameet process <id>` adds the transcript. The ingest (the command `meeting.ingest.command` names) and `megameet check` read this directory.

```
<meeting.data>/pull/<id>/          id = YYYYMMDD-HHMMSS-<source>-<host>
  meta.toml                        id, source (mac|room|phone|feishu|file), host, owner, title,
                                   started, stopped, duration_s, apps, speakers [{name, role}],
                                   tracks [{name, role, file, sha256, bytes}], feishu {token}
  rec.json                         the registry record as pulled
  tracks/<role>.<codec>            one file per track, sha256-verified against the record:
                                   remote + mic (mac), beam + raw (room), media (phone, file, feishu)
  doa.jsonl                        room only: azimuths at 10 Hz
  feishu-transcript.json           feishu only: Feishu's turns {segments: [{speaker, start_s, text}]}
  segments.json                    [{track, speaker, start_s, end_s, raw, text, engine, root}]
  transcript.md                    turns by time: "HH:MM:SS speaker: text"
```

`file` in `meta.toml` is relative to the directory. A Feishu minute whose media export was denied has no tracks and no `segments.json`; its turns are `feishu-transcript.json`.

`megameet check <id|DIR>` exits 0 on a complete directory and prints `complete: <dir> (<source>, N track(s), M segment(s))`. Otherwise it prints one `missing: …` line for each gap: an absent file, a track whose sha256 or size differs from `meta.toml`, a source's required file (`feishu-transcript.json`, `doa.jsonl`), or the process outputs.

### What the drain hands the ingest

The drain writes the ingest's recording record from this directory into `<meeting.data>/ingest/<id>/<stamp>/record.json`:

- `turns`: Feishu's turns for a `feishu` record (Feishu's transcript is canonical), else `segments.json`'s non-empty segments, in time order.
- `speakers`: the record's `speakers [{name, role, person?}]` as `role → name`, so a track-labelled segment (`mic`, `remote`) resolves to a person; an entry with a `person` (a person-page slug) is `role → {name, person}`, and the ingest takes that page as given.
- `media`: every file under `tracks/` except the room box's `raw`, named by role (unnamed when there is one).

It then runs `<meeting.ingest.command> run record.json --out <that folder> --commit`, adding `--replace` when the record is already in the wiki (a `reingest`). The ingest prints a `commit: <wiki> <sha>` line and writes `meeting.json` with the wiki `page`; they become the record's `wiki: {page, commit}`. The drain then writes the record itself as `rec.json` beside the filed audio, the registry's backup.

## The drain

`megameet drain` is the server's loop, run by a systemd timer every 60 s (one tick per start; `--every 60s` loops in the foreground). A tick:

1. Once an hour, marks each record still `uploading` 24 h after its last update `failed` with `error: abandoned`.
2. Lists `q.<id>` and claims each claimable record with one compare-and-swap: `claim {by: <host>:<pid>, at, expires: now + 30 min}`, `action` cleared, `attempts` + 1. A lost swap (409 `version_conflict`) is logged and skipped; another drain has the record.
3. Runs what the record owes, renewing the claim every 5 minutes and re-reading the record before each write:
   - `uploaded`, a `retry`, or `processing` with an expired claim: pull → process → summary → align (`state: processing`, `stage` pull → asr → align), then `aligned`; then the ingest when `meeting.ingest.auto` is on and the record's source is in `meeting.ingest.sources` (default mac, room, phone, file) — `stage: ingest`, then `ingested` with `wiki`, and one line through `meeting.drain.notify` with the page and the run's questions (`notes.md`). A mac, room, phone or file record shorter than `meeting.ingest.min_duration` (default 2 min) is not ingested on its own, and a record with `test: true` (`--test`) is never ingested, not even on `reingest`. A Feishu minute is ingested only when `sources` has `feishu`, Feishu created it (`started`) at or after `meeting.ingest.feishu_since` (the minutes before it are the backfill's), and the backfill's rule holds: 5 min or more, Feishu's transcript, two speaker labels or more in it. Otherwise it rests at `aligned` with the reason in `ingest_skipped`. A record that arrives with `wiki` set (a minute the backfill filed) goes to `ingested` without an ingest, and a Feishu minute the wiki already holds when its ingest comes (a filed source there names the minute's token) gets that `wiki` instead of a second filing.
   - `realign` on an `aligned` or `ingested` record: align again; the state stays.
   - `reingest` (set by the page's Re-ingest, for any source): pull → ingest with `--replace`.
4. Finishes by clearing `claim` and `stage` and deleting `q.<id>`. A record parked at `aligned` with the ingest off leaves the queue too; the page's actions re-queue it.

With `meeting.ingest.people_resolve` on (it needs `auto`), each tick first lists every `rec.<id>` for speakers named by hand: a `speakers` entry with a `role`, a `name` that is neither `Speaker N` nor its role, and no `person`, on a record that is not deleted and not under a live claim. For each such record it runs `<meeting.ingest.command> people resolve record.json --out <meeting.data>/people/<id>/<stamp> --commit` (the record.json carries those speakers only) and reads `people.json`. Each resolved `person` is written onto the entries with that role and that name, by compare-and-swap on a fresh read, only the `person` key. An `ingested` record then gets `action: reingest` and its `q.<id>`, as the page's Re-ingest does, and the same tick re-ingests it. A name the resolve leaves ambiguous (or leaves out) waits in `drain/state.json` until the page renames the speaker; a failed resolve is reported through `meeting.drain.notify` and tried again after an hour.

The summary step runs when `[meeting.summary] command` names a claude CLI, for every source, `feishu` included. It sends the transcript (at most `max_chars` characters: a longer one sends its head and tail), with the closed meeting types and the label list (§ Labels), to `model` (default `claude-haiku-4-5`). It writes `display_title` (a short title that keeps the title rule below), `labels` when the record has none (so edits on the page stay), and, for every source but `feishu`, `summary`: a one-line gist of at most 140 characters, a blank line, then 2–3 sentences, in the meeting's language. It never writes `title`. A Feishu minute's `summary` is Feishu's own, written by `register-feishu` from the archive (`""` when Feishu has none). A record without `summary` has none yet. A failed summary is logged and sent through `meeting.drain.notify`, and the record goes on without one. `megameet titles` does the same for records that lack `display_title` or `labels`.

**The title rule** (`labels.TitleProblem`): a display title says what the meeting covered, never what kind of meeting it was. A closed type or a kind word from `labels.KindWords` (晨会, 复盘会, stand-up …) anywhere in it breaks the rule, even when the rest names a subject: 「烤箱温控复盘会」 fails, and 「烤箱温控校准」 keeps it. It may not end in 会议. A series name or a date with nothing else fails (「日常晨会」, 2020-06-30), and so does a title made only of words for meeting and reporting (「近期任务推进情况」, "Weekly status updates"). Before titling, the model lists the meeting's `subjects`, the one to three things it spent most time on, and builds the title from them. When the record is written the title must also share a run of three characters with a subject (`labels.NamesSubject`): when the only subject is 烤箱温控校准, 「后厨杂项」 keeps every other part of the rule and is still asked again. The prompt states the rule. A title that breaks it gets one more request, built from the model's own summary at a fraction of the first call's cost; if that title breaks it too, the title is taken from the first one or two subjects that keep the rule (in that example, 烤箱温控校准 itself), and only when no subject keeps it is the broken title kept and logged (`breaks the title rule after a retry`). `megameet titles --check` lists the live records that break it and their ids, writing nothing; `megameet titles --redo --ids <ids>` rewrites those records' `display_title` and leaves `title` and `labels` alone.

## Labels

A record's `labels` is a list of strings. Two lists make up the vocabulary:

- **The closed list** is the registry record `labels` (schema `labels@1`): `{closed: [{name, kind}], questions: [...], updated_at, updated_by}`, where `kind` is `type`, `project`, `person` or `topic`. It is the settled set: a change to it lands only as an approved question (below). While the record is absent, the seed meeting types stand in (`labels.SeedTypes`); `megameet labels init` creates it from them.
- **The open list** is every label on a live record that is not closed. It is derived and never stored. The summary step and the maintenance agent coin open labels freely, reusing an existing one when it fits.

A record's **meeting type** is its label that is a closed `type` entry, at most one. The page shows it beside the display title, and `display_title` never contains it. The model picks the type from the closed types only, and never coins one.

`megameet labels` reads and changes both lists (`megameet labels help`). Every change to records is a CAS on `rec.<id>` that touches `labels` alone and re-reads on `version_conflict`. Every change that lands is also written as a new record `labellog.<UTC stamp>.<n>.<rand>` (schema `labellog@1`: `at`, `by`, `op`, `reason`, `question`, `closed_before`/`closed_after`, and `records` with each record's labels before and after). A log record is created once and never rewritten. `megameet labels log` prints them in order.

- **Autonomous:** `rename`, `merge` and `split` among open labels, and `set` (one record's labels, whole; it drops a second type).
- **Through a question:** `promote` and `demote`, and any `rename`, `merge` or `split` that names a closed label. These refuse unless run through a question. `megameet labels ask --text … -- <command>` records the question in `labels.questions`.

Open questions appear on the meetings page for every link except a contributor's, and only the page's owner role can answer one. A tap on Approve or Decline writes the answer onto the question, as `labels question <id> --approve|--decline --answer …` does from a shell, then rings the page's inbox. The page's host hook (`pages/meetings-hook/agent/hook`, ticked on the page's tick host) then runs `megameet labels apply --approved` with the owner credential: every approved question's command runs and the question becomes `applied`. A command that fails leaves its question approved, with the failure in `error`, and the next pass retries it. `labels apply <id>` runs one question by hand.

The label maintenance agent runs once a day from any scheduler. Its procedure is [megameet-labels-agent.md](megameet-labels-agent.md).

`TICK_HOST=<host> pages/meetings-hook/install.sh <slug>` installs the hook into the page's site, clones the site on the tick host (`TICK_HOST`, the ssh name of the host whose ccc-pages tick runs page hooks), and pins it (`handler.hook`). The tick host lists the slug in its `page-endpoints.md`. Re-run the script after every publish of the page; when nothing moved it changes nothing.

A stage that fails writes `state: failed`, `stage` and `error`, deletes `q.<id>`, and sends one line through `meeting.drain.notify`. The ingest is tried twice before it fails.

`megameet status` on the server prints the last tick (`<meeting.data>/drain/state.json`), the queue with each record's state, stage and claim, the failed records with their errors, and the real-time factor of the latest processed records.

## Deleting a recording

A delete is a tombstone. `rec.<id>` stays with `state: deleted`, `deleted_at` and `deleted_by`, and keeps only what names the recording and dedupes it (id, source, host, owner, title, times, duration, `feishu.token`, `wiki`). Its files and `q.<id>` are purged. `register-feishu` skips a minute whose record is a tombstone, the drain never claims one (it drops the queue key, whatever `action` says) and removes its pull and align folders on the next sweep, and `status`, `align` and `score` treat it as absent. The page's Delete writes the tombstone and lists deleted records only under its Deleted filter.

`megameet delete <id>...` does the same from a shell, and also removes this host's recording, pull and pending copies. It prints `rec.<id>: deleted (tombstone, deleted_by megameet delete (<user>@<host>))`, then each purged key and file. It refuses a record a drain holds a live claim on. Run on a tombstone, it purges whatever an earlier delete left.

## Run your own

Your own copy of the whole setup: a ccc-pages service in your Cloudflare account holds a private meetings page, which is the registry and the file store. Your Macs record meetings into it, your phone uploads to it, and a Linux box transcribes what arrives. The steps run in this order; each says what success prints. `alice` stands for your name throughout.

You need: a Cloudflare account (see step 1), a clone of this repository, and a Linux box with [Nix](https://nixos.org/download) (flakes enabled), `git` and `ffmpeg` for step 7. The Mac needs Go and ffmpeg (`brew install go ffmpeg`), or Nix.

### 1. Deploy ccc-pages

Follow ccc-pages' `docs/DEPLOY-YOUR-OWN.md` to the end of § Bootstrap: a clone with `npm ci`, a deploy file, then

```sh
cd ccc-pages
CLOUDFLARE_API_TOKEN=... ./bootstrap.sh
```

Both scripts run under `/bin/bash`, which NixOS lacks: run step 1 from a Mac or another Linux. Success ends with `=== SHIPPED`. Exit 42 also means it shipped: without an R2 upload key pair the smoke's three `POST /u/` grant checks answer 500. megameet never uses `/u/`, only `/f/` (files) and `/d/` (records), so you can skip that key pair. Bootstrap writes `<worker>.owner.env` beside the deploy file (`alice-pages.owner.env` for `CCC_PAGES_WORKER=alice-pages`): your owner credential (`UCC_USERNAME`, `UCC_AUTH_TOKEN`, `CCC_PAGES_URL`). Keep it private: it can delete everything.

Every later owner command runs in a shell with that file sourced:

```sh
. /path/to/alice-pages.owner.env
BIN=/path/to/ccc-pages/skills/ccc-pages/bin
```

### 2. Publish the meetings page

The page is a React app in `pages/meetings` ([its README](../pages/meetings/README.md) covers staging and rollback). From a clone of this repository, build it (Node 24, pnpm) and publish its `dist`:

```sh
cd mega-asr/pages/meetings
pnpm install --frozen-lockfile && pnpm build
cd ../..
"$BIN/page-publish" pages/meetings/dist --private --data on --title Meetings --description Meetings --no-bundle
```

It prints a JSON reply whose `"slug"` is `meetings-<16 hex>`; its last line is `page-publish: release record written — /d/meetings-…/release is now v1` (the lines between, such as the hint to add a `--description`, need nothing from you). That slug names your page from now on. Keep it in the shell for the next steps:

```sh
SLUG=meetings-0123456789abcdef    # the "slug" printed above
```
 `--private` keeps everyone out without a token (an anonymous GET answers 401), `--data on` opens the record store, and `--no-bundle` stops page-publish from uploading this repository's git history as the page's source. To update the page later, rebuild, then publish the same `dist` to the same slug; the records and files stay:

```sh
"$BIN/page-publish" pages/meetings/dist --slug $SLUG --private --no-bundle --description Meetings
```

Each publish is a new version of the page. `"$BIN/page-publish" --slug $SLUG --versions` lists them, and `"$BIN/page-publish" --slug $SLUG --switch <n>` serves version `n` again — the rollback.

After every publish, run `TICK_HOST=<host> pages/meetings-hook/install.sh $SLUG` when the page answers label questions through its host hook ([Labels](#labels)): it re-pins the hook if the page's site moved, and changes nothing otherwise.

Open it yourself with an editor link, which also lets you retry, re-process and delete records from the page:

```sh
"$BIN/page-token" mint "$SLUG" --role editor --label alice-browser
```

The first line is a link `https://…/a/$SLUG#t=…`. Open it once in your browser; the cookie it leaves renews itself.

### 3. One contributor token per device and per phone

A contributor can upload files and create records, and change and read only what it created; it can never delete. Mint one per Mac and per phone, labelled, so a lost device is revoked alone. The Mac's token goes into a file megameet reads (step 5); if the Mac is not the machine with the owner file, copy the file over afterwards, keeping it mode 0600:

```sh
mkdir -p ~/.config/megavoice
(umask 077; "$BIN/page-token" mint "$SLUG" --role contributor --label mac-alice --expires 365d \
  | sed -n 's/.*#t=//p' > ~/.config/megavoice/mac-alice.token)
```

`page-token` prints `id <ID> — shown once; the service keeps only a hash` on stderr; note the id, it is what `page-token revoke <ID>` takes. The file holds the secret part of the link. `--expires` takes `Nd` or an ISO time (`1h` is refused). For a phone, keep the whole link instead of the `sed`:

```sh
"$BIN/page-token" mint "$SLUG" --role contributor --label phone-alice --expires 365d
```

Open that link in the phone's browser. The page opens at phone width with its upload form, and lists only the recordings that link uploaded. iOS has no recorder in the file picker: record in Voice Memos, Share → Save to Files, then pick the file in the form. One file may be up to 100 MiB (about 3 hours of Voice Memos AAC). `page-token list --slug $SLUG` shows every token.

### 4. Install megameet on a Mac

megameet records the meeting app's audio through a Core Audio process tap and your mic through the input device. macOS grants that to a signed app, so megameet runs as `MegaMeet.app` under a LaunchAgent, and the `megameet` command talks to it. You need a code-signing identity: an **Apple Development** certificate (Xcode → Settings → Accounts, a free Apple ID works), or any identity named in `MEGAVOICE_SIGN_IDENTITY`.

The `go build` line makes the `megameet` command and prints nothing on success; the next two make the recorder app. A host that only uploads files (step 5's check) needs the `go build` line alone.

```sh
cd mega-asr
mkdir -p ~/.local/bin
CGO_ENABLED=0 go build -o ~/.local/bin/megameet ./cmd/megameet
scripts/bundle.sh ~/.local/bin/megameet /tmp/MegaMeet.app packaging/megameet.plist
scripts/install-app.sh /tmp/MegaMeet.app
```

`install-app.sh` ends with `install-app: $HOME/Applications/MegaMeet.app ← /tmp/MegaMeet.app (signed: Apple Development: …)` with `$HOME` expanded; a `replacing existing signature` line before it is codesign's and expected. With Nix instead of Go: `nix build .#megavoice` builds both, and `result/libexec/megavoice/install-app.sh result/libexec/megavoice/MegaMeet.app` installs; the CLI is `result/bin/megameet`.

Then the LaunchAgent. Its `PATH` must reach `ffmpeg`, which packs the tracks to FLAC; `/opt/homebrew/bin` is Homebrew's:

```sh
cat > ~/Library/LaunchAgents/app.0xdao.megameet.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>app.0xdao.megameet</string>
  <key>ProgramArguments</key><array>
    <string>$HOME/Applications/MegaMeet.app/Contents/MacOS/megameet</string><string>serve</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/megameet.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/megameet.log</string>
</dict></plist>
EOF
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/app.0xdao.megameet.plist
~/.local/bin/megameet status
```

`status` prints `idle` and the recordings directory (`~/.local/share/megameet/recordings`). The first `megameet start` makes macOS ask **MegaMeet** for **Microphone** and **System Audio Recording Only**; allow both. `start` waits up to 2 minutes for the answers. The grants are tied to the bundle id and the signing identity, so a rebuild signed the same way keeps them.

### 5. Point the Mac at the page

megameet reads the `[meeting]` tables of `~/.config/megavoice/config.toml`. Another file is named with `$MEGAVOICE_CONFIG` or with `--config FILE` before the command (`megameet --config FILE upload …`). Add:

```toml
[meeting]
# data = "~/.local/share/megameet"   # local state: recordings/, pending/ (unfinished uploads), pages/<slug>.jar (the page's cookie)

[meeting.page]
url = "https://alice-pages.<subdomain>.workers.dev"   # your CCC_PAGES_URL
slug = "meetings-0123456789abcdef"              # your $SLUG
token_file = "~/.config/megavoice/mac-alice.token"
```

`data` is where megameet keeps its local state; set it only to keep a second setup apart from a first on the same machine. Paths in the config may start with `~`.

Check it with a file upload, which works from any host and needs no app. Any audio file ffmpeg reads will do; to make one outside the repository: `cd /tmp && ffmpeg -f lavfi -i sine=duration=20 -c:a aac setup-test.m4a`.

```sh
megameet upload --source file --title "setup test" --test setup-test.m4a
```

Test against a production page only with `--test` (on `upload --source file` and on `start`) or on a scratch page: a record marked `test: true` is transcribed and aligned but never filed in the wiki, and `megameet status` lists it with `(test)`.

It prints `rec.<id>: 1 file(s)`, then log lines ending `upload <id>: uploaded`. The record appears on the page as `uploaded`. `megameet status` now lists the page's records as well, as far as this Mac's contributor token can read them (only its own): `records: 1 (uploaded 1)`, the queue, `failed: 0`, and `recent RTF:` (empty until the drain has processed something). Its first two lines (`idle`, `recordings: …`) come from the MegaMeet agent running on this Mac, when there is one, reached through its socket `$XDG_STATE_HOME/megameet/ctl.sock` (default `~/.local/state/megameet/ctl.sock`), which the config does not move; `drain: no tick on this host yet` is normal on a Mac, as the drain runs on the Linux box. The id is the file's start time (its modification time minus its length) and the host, so uploading the same file again prints only the `rec.` line and `upload <id>: uploaded` and adds nothing; `status` still counts one record. An upload that was cut off resumes with plain `megameet upload`. Delete the test record on the page with your editor link (step 2) when you are done with it; a contributor token cannot delete.

To record a call: `megameet apps` during the call lists the audio processes; the bundle id to tap is the one with `Y` in the `OUT` column while the other side speaks (Feishu/Lark: `com.electron.lark.helper`, the default). Then

```sh
megameet start --title "weekly with Bob"     # prints: recording <id> (apps …)
megameet stop                                # prints: stopped <id>: <seconds> s, and its directory
```

After `recording <id>`, `start` watches the mic for about 3 s and prints `mic "<device>": live, <level> dBFS`, or a `WARNING:` when the mic delivers digital silence: every 20 ms block at or below −80 dBFS for 2 s. Turn the mic on, or `stop` and pin another input with `[meeting.capture] mic`. `megameet status` names the mic with its level and repeats the warning for as long as the silence lasts, and the agent's log records when it starts and ends. On the control socket, `status` carries the same as `mic`: `device`, `level_dbfs` (the loudest 20 ms block of the last 100 ms), `silent`, `since`, `warning`.

### From the menu bar

MegaVoice.app puts a waveform in the menu bar that drives the same agent over the same socket: **Start Meeting…** asks for a title and an optional speaker count, **Stop Meeting** ends it. While recording, the bar shows a red dot and the elapsed time, and the menu shows the mic with a level meter; digital silence turns the bar orange with a warning sign and the menu says so. **Microphone** lists the inputs and writes the pick to `[meeting.capture] mic` (`megavoice config set`); the agent reads that key at every start, so a pick applies to the next meeting without a restart. The menu also shows the last recording on this Mac with its state on the page (uploaded → processing → aligned → ingested, or failed) and its wiki page once ingested, and opens the meetings page. Agents keep the CLI: `megameet mics` lists the inputs with the UIDs `mic` takes, and the socket's `last` command answers what the menu shows about the last recording.

At `stop` the agent packs both tracks (`remote`: the call, `mic`: you) to FLAC and uploads them. An upload cut off by sleep or network loss resumes at the agent's next start or `megameet start`, or by hand with `megameet upload`. The local copy stays 30 days after upload (`[meeting.upload] retention_days`).

### 6. Set the drain's owner identity

The drain reads and writes every record, so it needs the owner credential. On the Linux box, put the owner file where megameet reads it:

```sh
install -D -m 600 alice-pages.owner.env ~/.local/share/ucc/user-env.sh
```

(`$UCC_HOME/user-env.sh` if you set `UCC_HOME`; megameet reads `UCC_USERNAME` and `UCC_AUTH_TOKEN` from it.)

### 7. Run the drain on a Linux box

The drain claims each uploaded record, transcribes it with Fun-ASR-Nano on the CPU, and writes the transcript back to the page (§ The drain above). Build megameet and the ASR engine from this repository:

```sh
cd mega-asr
nix build .#megameet -o ~/.local/share/megameet/pkg
nix build .#funasr-root -o ~/.local/share/mega-asr/funasr-root
```

`~/.local/share/megameet/pkg/bin/megameet` is the command; the engine root holds `bin/llama-funasr-cli` and `models/`. Write `~/.config/megavoice/config.toml` on this box:

```toml
[asr.funasr]
root = "~/.local/share/mega-asr/funasr-root"

[meeting.page]
url = "https://alice-pages.<subdomain>.workers.dev"
slug = "meetings-0123456789abcdef"
identity = "ucc"
```

Run one tick by hand:

```sh
~/.local/share/megameet/pkg/bin/megameet drain
```

With the setup test from step 5 still queued, it logs `claimed for full`, the ASR's chunks, `<id>: full aligned`, and ends `drain: tick HH:MM:SS: 1 queued, 1 handled in N s`; the record shows `aligned` on the page with its transcript. `megameet status` on this box prints the last tick, the queue, failures and each record's real-time factor, which depends on the CPU and on `asr.funasr.llm_threads` (the CLI's 4 threads when unset).

Then run it every minute with a systemd user timer:

```sh
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/megameet-drain.service <<'EOF'
[Unit]
Description=megameet drain: one tick over the meetings page
[Service]
Type=oneshot
ExecStart=%h/.local/share/megameet/pkg/bin/megameet drain
EOF
cat > ~/.config/systemd/user/megameet-drain.timer <<'EOF'
[Unit]
Description=megameet drain every 60 s
[Timer]
OnBootSec=1min
OnUnitActiveSec=60s
[Install]
WantedBy=timers.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now megameet-drain.timer
loginctl enable-linger "$USER"      # keep the timer running while you are logged out
systemctl --user list-timers megameet-drain.timer
```

`list-timers` shows one line with the next run. `journalctl --user -u megameet-drain` has each tick's log. Without systemd, `megameet drain --every 60s` loops in the foreground (run it under tmux).

The ingest into a wiki (`[meeting.ingest] auto`) is off by default and stays off here: it files meetings into a wiki and needs an ingest command for that wiki. With it off, a record rests at `aligned`, and its transcript (`transcript.md`, `segments.json`) is on the page.

The title and summary (`[meeting.summary] command`) are off by default too. Setting `command` to a logged-in `claude` sends each transcript to Anthropic. Haiku 4.5 costs $1 per million input tokens and $5 per million output tokens (thinking and the prompt cache are off). The input is bounded by `max_chars` (default 40 000 characters; Chinese text runs at about one token per character or fewer), so one call's input costs at most about $0.04, and its output, a title, labels and a short summary, adds well under a cent.

### Privacy

megameet has **no `[meeting.privacy]` table**. Consent, naming and exclusion are how you use it, and what each link can read is ccc-pages' rule for the page. What it does:

- **Consent.** Nothing records until someone runs `megameet start`, and nothing tells the other side. Say that you are recording when you start.
- **Naming.** A Mac recording's two tracks are labelled by role: `mic` is you, `remote` is everyone on the call. A phone or file upload is one speaker. megameet stores no names; `--speakers N` is only a count.
- **Exclusion.** Delete a recording on the page (editor link → Delete). That removes its record and every file of it from R2, and there is no undo. The drain transcribes within a minute of the upload, so delete early, or stop the drain timer first. The Mac keeps its own copy under `~/.local/share/megameet/recordings/<id>/` for `retention_days`; delete that too.
- **Who can read what.** The page is private: without a link nothing on it can be read. The owner file (step 1) and an editor link read everything and can delete everything; `"$BIN/page-publish" --slug $SLUG --delete-page` does exactly that. A viewer link, if you mint one, reads every recording and transcript. A contributor link (a Mac's token file, a phone's link) reads only the records and files its own token created: its uploads and their records, which the drain keeps updating. It cannot read the transcripts and alignments the drain makes from them, or any other device's or person's recording: those answer as missing. So a contributor link on the page lists only its own uploads, and `megameet status` on a Mac counts only that Mac's records. Give each device and each person their own contributor link, and revoke a lost one with `page-token revoke <ID>` (it takes effect within an hour). A leaked contributor link can add recordings, but it cannot read or delete anyone else's.
- **Where it lives.** Audio and transcripts stay in your Cloudflare account and on your own Mac and Linux box. Nothing leaves them while the ingest and the summary are off; the summary sends the transcript to Anthropic.
