package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
)

// The meeting menu: MegaVoice's menu bar item drives megameet serve over
// its control socket, the same commands the megameet CLI sends. This file
// is the view — what the bar and the menu show for a state; meetmenu_darwin.go
// polls megameet and renders it.

// meetingsPage is the page where every meeting record is listed:
// meeting.page.url's /a/<meeting.page.slug>/; "" when the slug is unset.
func meetingsPage(p app.MeetingPageConfig) string {
	if p.Slug == "" {
		return ""
	}
	return strings.TrimSuffix(p.URL, "/") + "/a/" + p.Slug + "/"
}

// meetStatus is the part of megameet's `status` answer the menu reads.
type meetStatus struct {
	Recording bool `json:"recording"`
	Current   *struct {
		ID      string
		Title   string
		Started time.Time
	} `json:"current"`
	Seconds float64  `json:"seconds"`
	Mic     *meetMic `json:"mic"`
}

type meetMic struct {
	Device    string    `json:"device"`
	LevelDBFS float64   `json:"level_dbfs"`
	Silent    bool      `json:"silent"`
	Since     time.Time `json:"since"`
	Warning   string    `json:"warning"`
}

// meetLast is megameet's `last` answer.
type meetLast struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Started   time.Time `json:"started"`
	DurationS float64   `json:"duration_s"`
	Error     string    `json:"error"`
	Upload    string    `json:"upload"`
	State     string    `json:"state"`
	Stage     string    `json:"stage"`
	RecError  string    `json:"rec_error"`
	Wiki      *struct {
		Page string `json:"page"`
	} `json:"wiki"`
	WikiOpen  string `json:"wiki_open"`
	PageError string `json:"page_error"`
	Deleted   bool   `json:"deleted"`
}

// meetState is everything the menu shows.
type meetState struct {
	Down    string // megameet serve does not answer: why
	Status  meetStatus
	Last    *meetLast
	Inputs  []audio.Input
	MicPick string // meeting.capture.mic: a UID or "default"
	Page    string // the meetings page (meetingsPage); "" offers none
	Busy    string // a start or stop in flight
	Note    string // the last action's failure
	Voice   voiceState
}

// actKind is what a clickable row does.
type actKind int

const (
	actStart actKind = iota + 1
	actStop
	actMic // arg: the UID, or "default"
	actOpen
	actSet       // key = arg, a TOML value, through megavoice config set
	actRun       // run argv and show its output under the title arg
	actExec      // run argv, no output (open, restart)
	actLast      // transcribe the newest take again
	actCapture   // sets, in order, through megavoice config set: an Input pick
	actAddRemote // ask for a mic server's host, then pair: over ssh, else by PIN
	actForget    // arg: the remote mic to forget
	actPair      // pair a found box: arg its host:port, key its ssh Host ("" by PIN)
	actTake      // arg stop or cancel, key the take: the recording dictation take, from the menu
	actExit      // arg restart or quit, ending a recording take first as offered
)

type act struct {
	kind actKind
	arg  string
	key  string
	argv []string    // argv[0] "megavoice" is this app's own binary
	sets [][2]string // actCapture: key, TOML value
}

type row struct {
	title                       string
	key                         string // ⌘ key equivalent
	color                       string // "", "red", "orange", "label"
	enabled, checked, mono, sep bool
	indent                      int
	act                         *act
	sub                         []row
}

type barLook struct{ symbol, tint, title string }

func clock(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// bar is the menu bar look: a waveform when idle, beside it the count of
// undelivered dictation takes when there are any; a red light and the
// elapsed time while a meeting records, orange with a warning sign while the
// mic is digital silence.
func (st meetState) bar() barLook {
	s := st.Status
	idle := barLook{symbol: "waveform"}
	if n := st.Voice.Undelivered; n > 0 {
		idle.title = strconv.Itoa(n)
	}
	switch {
	case st.Down != "":
		return idle
	case !s.Recording:
		return idle
	case s.Mic != nil && s.Mic.Silent:
		return barLook{"exclamationmark.triangle.fill", "orange", clock(secs(s.Seconds))}
	}
	return barLook{"record.circle.fill", "red", clock(secs(s.Seconds))}
}

// span is a duration in words: "5 s", "12 min 36 s", "1 h 2 min".
func span(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s >= 3600:
		return fmt.Sprintf("%d h %d min", s/3600, s/60%60)
	case s >= 60:
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	}
	return fmt.Sprintf("%d s", s)
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

const meterCells = 20

// meter draws a level from -60 dBFS (no filled segment) to 0 (all filled).
func meter(db float64) string {
	n := int(math.Round((db + 60) / 60 * meterCells))
	n = min(max(n, 0), meterCells)
	txt := "no signal"
	if db > -119 {
		txt = fmt.Sprintf("%.0f dBFS", db)
	}
	return strings.Repeat("▰", n) + strings.Repeat("▱", meterCells-n) + "  " + txt
}

// rows are the whole menu: the meeting's, then dictation's.
func (st meetState) rows() []row {
	return append(append(st.meetRows(), row{sep: true}), st.Voice.rows()...)
}

func (st meetState) meetRows() []row {
	var rs []row
	info := func(t string, indent int) { rs = append(rs, row{title: t, indent: indent}) }
	sep := func() { rs = append(rs, row{sep: true}) }
	s := st.Status
	switch {
	case st.Down != "":
		info("MegaMeet is not running", 0)
		info(st.Down, 1)
	case s.Recording:
		title := "untitled"
		if s.Current != nil && s.Current.Title != "" {
			title = s.Current.Title
		}
		rs = append(rs, row{title: fmt.Sprintf("● Recording %s — %s", clock(secs(s.Seconds)), title), color: "red"})
		rs = append(rs, row{title: "Stop Meeting", enabled: st.Busy == "", act: &act{kind: actStop}})
	default:
		rs = append(rs, row{title: "Start Meeting…", enabled: st.Busy == "", act: &act{kind: actStart}})
	}
	if st.Busy != "" {
		info(st.Busy, 1)
	}
	if st.Note != "" {
		rs = append(rs, row{title: "⚠︎ " + st.Note, indent: 1, color: "orange"})
	}
	if st.Down == "" {
		sep()
		rs = append(rs, st.micRows()...)
	}
	sep()
	rs = append(rs, st.lastRows()...)
	if st.Page != "" {
		rs = append(rs, row{title: "Open Meetings Page", enabled: true, act: &act{kind: actOpen, arg: st.Page}})
	}
	return rs
}

// micRows: the mic in use and its level while recording, else the one the
// next start takes; and the picker.
func (st meetState) micRows() []row {
	var rs []row
	if m := st.Status.Mic; st.Status.Recording && m != nil {
		rs = append(rs, row{title: "Mic: " + m.Device})
		rs = append(rs, row{title: meter(m.LevelDBFS), mono: true, indent: 1, color: "label"})
		if m.Silent {
			rs = append(rs, row{title: fmt.Sprintf("⚠︎ No audio from this mic for %s", span(time.Since(m.Since))), indent: 1, color: "orange"})
			rs = append(rs, row{title: "Another mic takes effect at the next start", indent: 1, color: "orange"})
		}
	} else {
		rs = append(rs, row{title: "Mic: " + st.micName()})
	}
	sub := []row{}
	def := "System default"
	for _, in := range st.Inputs {
		if in.Default {
			def += " (" + in.Name + ")"
		}
	}
	sub = append(sub, row{title: def, enabled: true, checked: st.MicPick == "default", act: &act{kind: actMic, arg: "default"}})
	sub = append(sub, row{sep: true})
	found := st.MicPick == "default"
	for _, in := range st.Inputs {
		on := in.UID == st.MicPick
		found = found || on
		sub = append(sub, row{title: in.Name, enabled: true, checked: on, act: &act{kind: actMic, arg: in.UID}})
	}
	if !found {
		sub = append(sub, row{title: "(not connected) " + st.MicPick, checked: true})
	}
	if st.Status.Recording {
		sub = append(sub, row{sep: true}, row{title: "A new pick applies at the next start"})
	}
	rs = append(rs, row{title: "Microphone", enabled: true, sub: sub})
	return rs
}

// micName is the input the next start records.
func (st meetState) micName() string {
	for _, in := range st.Inputs {
		if in.UID == st.MicPick || (st.MicPick == "default" && in.Default) {
			return in.Name
		}
	}
	if st.MicPick == "default" {
		return "system default"
	}
	return st.MicPick + " (not connected)"
}

var stateText = map[string]string{
	"uploading":  "uploading",
	"uploaded":   "uploaded — waiting for the server",
	"processing": "processing",
	"aligned":    "transcribed — waiting for the wiki",
	"ingested":   "in the wiki",
	"failed":     "failed",
}

func (st meetState) lastRows() []row {
	l := st.Last
	if l == nil {
		return []row{{title: "No meeting recorded on this Mac yet"}}
	}
	name := l.Title
	if name == "" {
		name = l.ID
	}
	rs := []row{{title: fmt.Sprintf("Last: %s — %s, %s", name, l.Started.Local().Format("Jan 2 15:04"), clock(secs(l.DurationS)))}}
	add := func(t string) { rs = append(rs, row{title: t, indent: 1}) }
	if l.Error != "" {
		add("recording ended with: " + trim(l.Error, 70))
	}
	switch {
	case l.State != "":
		t := stateText[l.State]
		if t == "" {
			t = l.State
		}
		if l.State == "processing" && l.Stage != "" {
			t += " (" + l.Stage + ")"
		}
		if l.State == "failed" && l.RecError != "" {
			t += ": " + trim(l.RecError, 70)
		}
		add(t)
	case l.Upload != "":
		add("upload: " + trim(l.Upload, 70))
	case l.Deleted:
		add("deleted from the meetings page")
	case l.PageError != "":
		add("page: " + trim(l.PageError, 70))
	default:
		add("not on the meetings page")
	}
	if w := l.Wiki; w != nil && w.Page != "" {
		label := strings.TrimSuffix(w.Page[strings.LastIndex(w.Page, "/")+1:], ".md")
		r := row{title: "Wiki: " + label, indent: 1}
		if l.WikiOpen != "" {
			r.enabled, r.act = true, &act{kind: actOpen, arg: l.WikiOpen}
		}
		rs = append(rs, r)
	}
	return rs
}

func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// voiceState is dictation's settings and checks: the values serve runs
// with, the config file's (what a restart runs with), and what can be
// picked.
type voiceState struct {
	Running, File voiceSettings
	FileErr       string   // the config file does not load: why
	Models        []model  // fine-tune exports asr.funasr.llm can name
	Logs          []string // log files that exist
	Hotwords      string
	Corrections   string
	Store         string            // store.data: the takes
	Check         string            // the check in flight
	Note          string            // the last setting's or action's failure
	Compare       compareSettings   // the file's: read at every take
	Takes         string            // the Takes page's URL; "" while this process holds no listener
	TakesNote     string            // why its row is greyed
	Undelivered   int               // takes undelivered and not seen: the bar's count
	Capture       app.CaptureConfig // the file's: read at every take
	Inputs        []inputView       // this Mac's inputs
	Remotes       []remoteView      // the paired remote mics
	Found         []foundView       // the boxes Add Remote Mic… offers
	Scanning      bool              // a search for them is running
	FoundNote     string            // why the network was not searched
	Take          voiceTake         // the dictation take recording now
}

// voiceTake is the dictation take recording now, if one does.
type voiceTake struct {
	ID        string // its id, which the menu's stop and cancel name
	Recording bool
	Elapsed   time.Duration
}

type voiceSettings struct {
	TapKey   string
	WholeMax time.Duration
	LLM      string // "" is the root's base model
	Engine   string // asr.engine
}

type compareSettings struct {
	On      bool
	Engines []string
}

// engines are asr.engine's values as the menu names them.
var engines = []struct{ key, label string }{{"funasr", "Fun-ASR (local)"}, {"doubao", "Doubao 2.0 (cloud)"}}

func engineLabel(k string) string {
	for _, e := range engines {
		if e.key == k {
			return e.label
		}
	}
	return k
}

// tomlList is a TOML array of strings.
func tomlList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

type model struct{ Name, Path string }

// tapKeys are mac.LookupTapKey's names, with their menu label and their
// name in the resend hotkey's hint on a flash.
var tapKeys = []struct{ key, label, chord string }{{"right_shift", "Right Shift", "右Shift"}, {"right_option", "Right Option", "右Option"}}

var wholeMaxes = []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

const afterRestart = "Applies after Restart MegaVoice"

func keyLabel(k string) string {
	for _, t := range tapKeys {
		if t.key == k {
			return t.label
		}
	}
	return k
}

// chordKey is the tap key k's name in the hotkey's hint: 右Option for the
// default Right Option.
func chordKey(k string) string {
	for _, t := range tapKeys {
		if t.key == k {
			return t.chord
		}
	}
	return ""
}

func wholeLabel(d time.Duration) string {
	if d == 0 {
		return "none, every take streams"
	}
	return "up to " + strings.Replace(span(d), " 0 s", "", 1)
}

func (v voiceState) modelLabel(llm string) string {
	if llm == "" {
		return "Base"
	}
	for _, m := range v.Models {
		if m.Path == llm {
			return m.Name
		}
	}
	return filepath.Base(filepath.Dir(llm)) + "/" + filepath.Base(llm)
}

// setting is one restart-scoped setting's submenu: its title names the
// file's value, and "(after restart)" while serve still runs another.
func setting(name, file, running string, sub []row) row {
	title := name + ": " + file
	if file != running {
		title += " (after restart)"
	}
	sub = append(sub, row{sep: true}, row{title: afterRestart})
	return row{title: title, enabled: true, sub: sub}
}

func (v voiceState) rows() []row {
	rs := []row{{title: "Dictation"}}
	if v.Take.Recording {
		rs = append(rs,
			row{title: "● 录音中 " + clock(v.Take.Elapsed), color: "red"},
			row{title: "停止并粘贴", enabled: true, act: &act{kind: actTake, arg: "stop", key: v.Take.ID}},
			row{title: "取消录音", enabled: true, act: &act{kind: actTake, arg: "cancel", key: v.Take.ID}},
		)
	}
	rs = append(rs, v.takesRow())
	if v.TakesNote != "" {
		rs = append(rs, row{title: "⚠︎ " + trim(v.TakesNote, 90), indent: 1, color: "orange"})
	}
	f, r := v.File, v.Running
	if v.FileErr != "" {
		rs = append(rs, row{title: "⚠︎ " + trim(v.FileErr, 90), indent: 1, color: "orange"})
		f = r
	}
	rs = append(rs, v.inputRows())
	pick := func(title string, on bool, key, val string) row {
		return row{title: title, enabled: v.FileErr == "", checked: on, act: &act{kind: actSet, key: key, arg: val}}
	}

	var keys []row
	for _, t := range tapKeys {
		keys = append(keys, pick(t.label, t.key == f.TapKey, "tap.key", strconv.Quote(t.key)))
	}
	rs = append(rs, setting("Key", keyLabel(f.TapKey), keyLabel(r.TapKey), keys))

	var whole []row
	found := false
	for _, d := range wholeMaxes {
		found = found || d == f.WholeMax
		whole = append(whole, pick(capital(wholeLabel(d)), d == f.WholeMax, "take.whole_max", strconv.Quote(app.Duration(d).String())))
	}
	if !found {
		whole = append(whole, row{title: capital(wholeLabel(f.WholeMax)), checked: true})
	}
	whole = append(whole, row{sep: true}, row{title: "A take this long or shorter is decoded whole at stop;"}, row{title: "a longer one streams in chunks while you speak"})
	rs = append(rs, setting("Decode whole", wholeLabel(f.WholeMax), wholeLabel(r.WholeMax), whole))

	var models []row
	found = f.LLM == ""
	for _, m := range v.Models {
		found = found || m.Path == f.LLM
		models = append(models, pick(m.Name, m.Path == f.LLM, "asr.funasr.llm", strconv.Quote(m.Path)))
	}
	if !found {
		models = append(models, pick(v.modelLabel(f.LLM), true, "asr.funasr.llm", strconv.Quote(f.LLM)))
	}
	if len(models) > 0 {
		models = append([]row{{sep: true}}, models...)
	}
	models = append([]row{pick("Base", f.LLM == "", "asr.funasr.llm", `""`)}, models...)
	models = append(models, row{sep: true}, row{title: "Checks › Transcribe and Script Check use it at once"})
	rs = append(rs, setting("Model", v.modelLabel(f.LLM), v.modelLabel(r.LLM), models))

	var engs []row
	for _, e := range engines {
		engs = append(engs, pick(e.label, e.key == f.Engine, "asr.engine", strconv.Quote(e.key)))
	}
	engs = append(engs, row{sep: true}, row{title: "Doubao streams each take as you speak;"}, row{title: "Fun-ASR stands in when it fails (offline)"})
	rs = append(rs, setting("Engine", engineLabel(f.Engine), engineLabel(r.Engine), engs))
	rs = append(rs, v.compareRows(r.Engine)...)

	rs = append(rs,
		row{title: "Edit Hotwords…", enabled: true, act: &act{kind: actExec, argv: []string{"open", "-t", v.Hotwords}}},
		row{title: "Edit Corrections…", enabled: true, act: &act{kind: actExec, argv: []string{"open", "-t", v.Corrections}}},
		row{title: "Edits apply at the next take", indent: 1},
	)
	restart := "Restart MegaVoice"
	if f != r {
		restart += " to Apply the Changes"
	}
	rs = append(rs, row{title: restart, enabled: true, act: &act{kind: actExit, arg: "restart"}})
	rs = append(rs, row{sep: true}, v.checks())
	if v.Check != "" {
		rs = append(rs, row{title: v.Check + "…", indent: 1})
	}
	if v.Note != "" {
		rs = append(rs, row{title: "⚠︎ " + v.Note, indent: 1, color: "orange"})
	}
	rs = append(rs, row{sep: true}, row{title: "Quit MegaVoice", key: "q", enabled: true, act: &act{kind: actExit, arg: "quit"}})
	return rs
}

// takesRow opens the Takes page and names the takes undelivered and not
// seen: 打开录音历史 · 2 条未送达.
func (v voiceState) takesRow() row {
	title := "打开录音历史"
	if v.Undelivered > 0 {
		title += fmt.Sprintf(" · %d 条未送达", v.Undelivered)
	}
	return row{title: title, enabled: v.Takes != "", act: &act{kind: actOpen, arg: v.Takes}}
}

// drainMax bounds how long a restart or quit chosen from the menu waits for
// what is in flight. A take's text normally arrives within seconds of its
// stop (Doubao's final answer within 10 s plus the queued audio at 8×; a
// whole decode of a one-minute take in a few seconds), so 30 s covers it with
// room; the ASR's own deadlines (60 s plus twice the clip) are what a hang
// looks like, and the person at the menu does not wait for those. Past it
// megavoice exits: the take's WAV and record are on disk, and the next start
// recovers a take without text.
const drainMax = 30 * time.Second

// offer is what Restart or Quit asks while a take records: each choice's
// button and how it ends the take ("" keeps it recording, and nothing exits).
type offer struct {
	title, info string
	choices     []offerChoice
}

type offerChoice struct{ label, end string }

func exitOffer(cmd string, elapsed time.Duration) offer {
	verb := "重启"
	if cmd == "quit" {
		verb = "退出"
	}
	return offer{
		title: "正在录音 " + clock(elapsed),
		info:  verb + "前先结束这条录音。结束后最多再等 " + strconv.Itoa(int(drainMax.Seconds())) + " 秒识别和送达，然后" + verb + "；没送达的录音留在录音历史里。",
		choices: []offerChoice{
			{"停止并粘贴，然后" + verb, "paste"},
			{"取消录音，然后" + verb, "cancel"},
			{"继续录音", ""},
		},
	}
}

// takeArgs are the control socket's args for a take row's stop or cancel:
// from the menu, for the take the row was drawn for.
func takeArgs(a act) map[string]string { return map[string]string{"via": "menu", "take": a.key} }

// exitArgs are the control socket's args for restart or quit from the menu,
// with the offer's choice and the take it was shown for when a take records;
// nil when the choice keeps the take recording. A menu exit never meets
// the refusal: with nothing recording it still ends in an exit once what is
// in flight is done.
func exitArgs(tk voiceTake, o offer, choice int) map[string]string {
	if !tk.Recording {
		return map[string]string{"end": "paste"}
	}
	if choice < 0 || choice >= len(o.choices) || o.choices[choice].end == "" {
		return nil
	}
	return map[string]string{"end": o.choices[choice].end, "take": tk.ID}
}

// compareRows: compare mode and its engines apply at the next take; the
// primary is the running engine, never run twice.
func (v voiceState) compareRows(primary string) []row {
	c, ok := v.Compare, v.FileErr == ""
	rs := []row{{title: "Compare Mode", enabled: ok, checked: c.On, act: &act{kind: actSet, key: "compare.on", arg: strconv.FormatBool(!c.On)}}}
	var sub []row
	for _, e := range engines {
		if e.key == primary {
			sub = append(sub, row{title: e.label + " — primary", checked: true})
			continue
		}
		on := slices.Contains(c.Engines, e.key)
		var next []string
		for _, x := range engines {
			if x.key == e.key && !on || x.key != e.key && slices.Contains(c.Engines, x.key) {
				next = append(next, x.key)
			}
		}
		sub = append(sub, row{title: e.label, enabled: ok, checked: on, act: &act{kind: actSet, key: "compare.engines", arg: tomlList(next)}})
	}
	sub = append(sub, row{sep: true}, row{title: "Each take also goes to the checked engines;"}, row{title: "the primary delivers as always"})
	return append(rs, row{title: "Compare Engines", enabled: true, sub: sub})
}

// checks runs the commands an agent would, and shows what they print.
func (v voiceState) checks() row {
	idle := v.Check == ""
	run := func(title string, argv ...string) row {
		return row{title: title, enabled: idle, act: &act{kind: actRun, arg: title, argv: argv}}
	}
	sub := []row{
		{title: "Transcribe Last Take Again", enabled: idle, act: &act{kind: actLast, arg: "Transcribe Last Take Again"}},
		run("Script Check", "megavoice", "script"),
		{sep: true},
		run("MegaVoice Status", "megavoice", "status"),
		run("MegaMeet Status", "megameet", "status"),
	}
	if len(v.Logs) > 0 {
		sub = append(sub, row{sep: true})
	}
	for _, l := range v.Logs {
		sub = append(sub, row{title: "Open " + filepath.Base(l), enabled: true, act: &act{kind: actExec, argv: []string{"open", "-a", "Console", l}}})
	}
	return row{title: "Checks", enabled: true, sub: sub}
}

func capital(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// lastTake is the newest finished take in dir: a WAV with its text beside
// it (a take still recording has none yet).
func lastTake(dir string) (string, error) {
	wavs, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		return "", err
	}
	slices.Sort(wavs)
	for i := len(wavs) - 1; i >= 0; i-- {
		if _, err := os.Stat(strings.TrimSuffix(wavs[i], ".wav") + ".txt"); err == nil {
			return wavs[i], nil
		}
	}
	return "", fmt.Errorf("no take in %s", dir)
}
