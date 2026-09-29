package main

import (
	"encoding/json"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

func TestVoiceMenu(t *testing.T) {
	build := "/m/lab-build.gguf"
	settled := voiceSettings{TapKey: "right_option", WholeMax: time.Minute, Engine: "funasr"} // the shipped defaults
	base := voiceState{Running: settled, File: settled, Hotwords: "/c/hotwords.txt", Corrections: "/c/corrections.tsv",
		Compare: compareSettings{Engines: []string{"funasr", "doubao"}}, Takes: "http://127.0.0.1:7865/takes/",
		Models:  []model{{"lab-build", build}},
		Logs:    []string{"/L/megavoice.log", "/L/megameet.log"},
		Capture: app.CaptureConfig{Source: "local", Mic: "default"},
		Inputs:  []inputView{{"builtin-mic", "Built-in Microphone", 1, true}, {"stage-interface", "Stage Interface", 3, false}}}
	pending := base
	pending.File = voiceSettings{TapKey: "right_shift", WholeMax: 90 * time.Second, LLM: build, Engine: "doubao"}
	pending.Compare = compareSettings{On: true}
	broken := base
	broken.FileErr = "config.toml: tap.key: unknown tap key \"caps\""
	busy := base
	busy.Check = "Script Check"
	busy.Models = nil
	pinned := base
	pinned.Capture = app.CaptureConfig{Source: "local", Mic: "stage-interface", MicName: "Stage Interface", MicChannel: 3, Host: "micbox", Device: "hw:Array,0", Channel: 1}
	absent := base
	absent.Capture = app.CaptureConfig{Source: "local", Mic: "podium-condenser", MicName: "Podium Condenser"}
	ssh := base
	ssh.Capture = app.CaptureConfig{Source: "ssh", Mic: "default", Host: "micbox", Device: "hw:Array,0", Channel: 1}
	online := base
	online.Capture = app.CaptureConfig{Source: "remote", Mic: "default", Remote: "micbox"}
	online.Remotes = []remoteView{{Name: "den", Addr: "192.0.2.22:7866"}, {Name: "micbox", Addr: "micbox:7866"}}
	offline := online
	offline.Remotes = []remoteView{{Name: "den", Addr: "192.0.2.22:7866"}, {Name: "micbox", Addr: "micbox:7866", Offline: true}}
	unpaired := online
	unpaired.Remotes = nil
	found := online
	found.Found = []foundView{{Name: "den", Addr: "192.0.2.22:7866"}, {Name: "micbox", Addr: "192.0.2.11:7866", SSH: "micbox"}, {Name: "host-b", Advertised: "bench-recorder", Addr: "192.0.2.222:7866", SSH: "host-b"}}
	searching := base
	searching.Scanning = true
	recording := base
	recording.Take = voiceTake{ID: "20200101-120000", Recording: true, Elapsed: 83 * time.Second}
	localNet := base
	localNet.FoundNote = "Local Network access is off for this app (System Settings › Privacy & Security › Local Network)"
	portHeld := base // another process holds compare.addr: no address, and why
	portHeld.Takes, portHeld.TakesNote = pageURL("", &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)})

	for _, c := range []struct {
		name string
		v    voiceState
	}{{"settled", base}, {"pending", pending}, {"broken", broken}, {"busy", busy}, {"input-pinned", pinned}, {"input-absent", absent}, {"input-ssh", ssh},
		{"input-remote-online", online}, {"input-remote-offline", offline}, {"input-remote-unpaired", unpaired},
		{"input-add-found", found}, {"input-add-searching", searching}, {"input-add-no-network", localNet}, {"recording", recording}, {"listener-held", portHeld}} {
		t.Run(c.name, func(t *testing.T) {
			got := titles(c.v.rows())
			if want, err := os.ReadFile(filepath.Join("testdata", "voicemenu-"+c.name+".txt")); os.Getenv("UPDATE") != "" {
				os.MkdirAll("testdata", 0o755)
				os.WriteFile(filepath.Join("testdata", "voicemenu-"+c.name+".txt"), []byte(got), 0o644)
			} else if err != nil || got != string(want) {
				t.Errorf("rows:\n%s\nwant:\n%s", got, want)
			}
		})
	}

	// what the rows do: the key the pickers write, the commands the checks run
	acts := map[string]act{}
	var walk func(string, []row)
	walk = func(parent string, rs []row) {
		for _, r := range rs {
			if r.act != nil {
				acts[parent+r.title] = *r.act // by its path
				acts[r.title] = *r.act        // and by its title, the last to have it
			}
			walk(parent+r.title+" › ", r.sub)
		}
	}
	walk("held: ", portHeld.rows()) // first: base's rows keep the bare titles
	walk("", base.rows())
	walk("ssh: ", ssh.rows())
	walk("remote: ", online.rows())
	walk("found: ", found.rows())
	walk("rec: ", recording.rows())
	in := "Input: System default (Built-in Microphone) › "
	rin := "remote: Input: micbox (remote mic) › "
	fin := "found: Input: micbox (remote mic) › Add Remote Mic… › "
	for title, want := range map[string]act{
		in + "Add Remote Mic… › Other…":               {kind: actAddRemote},
		rin + "Add Remote Mic… › Other…":              {kind: actAddRemote},
		fin + "micbox (paired)":                       {kind: actPair, arg: "192.0.2.11:7866", key: "micbox"},
		fin + "den (paired, PIN)":                     {kind: actPair, arg: "192.0.2.22:7866"},
		fin + "host-b (advertised as bench-recorder)": {kind: actPair, arg: "192.0.2.222:7866", key: "host-b"},
		rin + "Forget Remote Mic › micbox":            {kind: actForget, arg: "micbox"},
		rin + "Forget Remote Mic › den":               {kind: actForget, arg: "den"},
		rin + "den (remote mic)":                      {kind: actCapture, sets: [][2]string{{"capture.remote", `"den"`}, {"capture.source", `"remote"`}}},
		rin + "micbox (remote mic)":                   {kind: actCapture, sets: [][2]string{{"capture.remote", `"micbox"`}, {"capture.source", `"remote"`}}},
		rin + "Built-in Microphone":                   {kind: actCapture, sets: capturePick("builtin-mic", "Built-in Microphone", 0)},
		rin + "System default (Built-in Microphone)":  {kind: actCapture, sets: capturePick("default", "", 0)},
	} {
		if got, ok := acts[title]; !ok || got.kind != want.kind || got.arg != want.arg || got.key != want.key || !slices.Equal(got.sets, want.sets) {
			t.Errorf("%s: %+v, want %+v", title, got, want)
		}
	}
	if got := takeArgs(acts["rec: 取消录音"]); !maps.Equal(got, map[string]string{"via": "menu", "take": "20200101-120000"}) {
		t.Errorf("the take row sends %v", got)
	}
	if _, ok := acts[in+"Forget Remote Mic"]; ok {
		t.Error("Forget Remote Mic shows with no remote paired")
	}
	for title, want := range map[string][][2]string{
		"ssh: Input: Array over ssh (micbox) › Stage Interface › Channel 3": {{"capture.mic", `"stage-interface"`}, {"capture.mic_name", `"Stage Interface"`}, {"capture.mic_channel", "3"}, {"capture.source", `"local"`}},
		in + "Stage Interface › Channel 1":                                  {{"capture.mic", `"stage-interface"`}, {"capture.mic_name", `"Stage Interface"`}, {"capture.mic_channel", "1"}, {"capture.source", `"local"`}},
		"ssh: Input: Array over ssh (micbox) › Array over ssh (micbox)":     {{"capture.source", `"ssh"`}},
		in + "Built-in Microphone":                                          {{"capture.mic", `"builtin-mic"`}, {"capture.mic_name", `"Built-in Microphone"`}, {"capture.mic_channel", "0"}, {"capture.source", `"local"`}},
		in + "System default (Built-in Microphone)":                         {{"capture.mic", `"default"`}, {"capture.mic_name", `""`}, {"capture.mic_channel", "0"}, {"capture.source", `"local"`}},
		in + "Stage Interface › All channels, mixed":                        {{"capture.mic", `"stage-interface"`}, {"capture.mic_name", `"Stage Interface"`}, {"capture.mic_channel", "0"}, {"capture.source", `"local"`}},
	} {
		if got, ok := acts[title]; !ok || got.kind != actCapture || !slices.Equal(got.sets, want) {
			t.Errorf("%s: %+v, want the writes %q", title, got, want)
		}
	}
	for _, title := range []string{"held: 打开录音历史"} {
		if a := acts[title]; a.arg != "" {
			t.Errorf("%s opens %q with no listener held", title, a.arg)
		}
	}
	for title, a := range acts {
		if a.kind == actOpen && a.arg != "" && a.arg != "http://127.0.0.1:7865/takes/" {
			t.Errorf("%s opens %q: the Takes page is the one page the menu opens", title, a.arg)
		}
	}
	if _, ok := acts[in+"Array over ssh (micbox)"]; ok {
		t.Error("the ssh row shows without ssh settings in the file")
	}
	for title, want := range map[string]act{
		"Right Shift":                {kind: actSet, key: "tap.key", arg: `"right_shift"`},
		"None, every take streams":   {kind: actSet, key: "take.whole_max", arg: `"0s"`},
		"Up to 2 min":                {kind: actSet, key: "take.whole_max", arg: `"2m"`},
		"Base":                       {kind: actSet, key: "asr.funasr.llm", arg: `""`},
		"lab-build":                  {kind: actSet, key: "asr.funasr.llm", arg: `"` + build + `"`},
		"Edit Hotwords…":             {kind: actExec, argv: []string{"open", "-t", "/c/hotwords.txt"}},
		"Restart MegaVoice":          {kind: actExit, arg: "restart"},
		"Quit MegaVoice":             {kind: actExit, arg: "quit"},
		"rec: 停止并粘贴":                 {kind: actTake, arg: "stop", key: "20200101-120000"},
		"rec: 取消录音":                  {kind: actTake, arg: "cancel", key: "20200101-120000"},
		"Script Check":               {kind: actRun, arg: "Script Check", argv: []string{"megavoice", "script"}},
		"MegaMeet Status":            {kind: actRun, arg: "MegaMeet Status", argv: []string{"megameet", "status"}},
		"Transcribe Last Take Again": {kind: actLast, arg: "Transcribe Last Take Again"},
		"Doubao 2.0 (cloud)":         {kind: actSet, key: "compare.engines", arg: `["funasr"]`},
		"Compare Mode":               {kind: actSet, key: "compare.on", arg: "true"},
		"Open megavoice.log":         {kind: actExec, argv: []string{"open", "-a", "Console", "/L/megavoice.log"}},
	} {
		got := acts[title]
		if got.kind != want.kind || got.key != want.key || got.arg != want.arg || !slices.Equal(got.argv, want.argv) {
			t.Errorf("%s: %+v, want %+v", title, got, want)
		}
	}
}

// The Takes row opens the page and names the undelivered takes; the bar
// shows their count beside the idle waveform.
func TestTakesRow(t *testing.T) {
	url := "http://127.0.0.1:7865/takes/"
	for _, c := range []struct {
		n     int
		title string
		bar   barLook
	}{
		{0, "打开录音历史", barLook{symbol: "waveform"}},
		{2, "打开录音历史 · 2 条未送达", barLook{symbol: "waveform", title: "2"}},
	} {
		v := voiceState{Takes: url, Undelivered: c.n}
		var got *row
		for _, r := range v.rows() {
			if strings.HasPrefix(r.title, "打开录音历史") {
				got = &r
				break
			}
		}
		if got == nil || got.title != c.title || !got.enabled || got.act == nil || got.act.kind != actOpen || got.act.arg != url {
			t.Errorf("%d undelivered: row %+v, want %q opening %s", c.n, got, c.title, url)
		}
		if b := (meetState{Voice: v}).bar(); b != c.bar {
			t.Errorf("%d undelivered: bar %+v, want %+v", c.n, b, c.bar)
		}
		if b := (meetState{Voice: v, Down: "no answer"}).bar(); b != c.bar {
			t.Errorf("%d undelivered, megameet down: bar %+v, want %+v", c.n, b, c.bar)
		}
	}
	rec := meetState{Voice: voiceState{Undelivered: 2}, Status: meetStatus{Recording: true, Seconds: 61}}
	if b := rec.bar(); b.title != "1:01" {
		t.Errorf("a meeting recording: bar %+v, want its clock", b)
	}
}

func TestLastTake(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"20200314-004500.wav", "20200314-004500.txt", "20200314-014500.wav", "20200314-014500.txt", "20200314-024500.wav"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	got, err := lastTake(dir)
	if want := filepath.Join(dir, "20200314-014500.wav"); err != nil || got != want {
		t.Errorf("got %q, %v; want %q (the newest with a text; 12:00 is still recording)", got, err, want)
	}
	if _, err := lastTake(t.TempDir()); err == nil {
		t.Error("empty dir: no error")
	}
}

// An Input pick that makes the source local asks for the Microphone grant
// then, not at that source's first take (which would record the prompt's
// silence); a pick of the array over ssh does not.
func TestPicksLocal(t *testing.T) {
	for sets, want := range map[string]bool{
		`[["capture.mic","\"boom-mic\""]]`:                                        false,
		`[["capture.source","\"ssh\""]]`:                                          false,
		`[["capture.mic","\"podium-condenser\""],["capture.source","\"local\""]]`: true,
	} {
		var s [][2]string
		if err := json.Unmarshal([]byte(sets), &s); err != nil {
			t.Fatal(err)
		}
		if got := picksLocal(s); got != want {
			t.Errorf("picksLocal(%s) = %v, want %v", sets, got, want)
		}
	}
}

// Restart and Quit from the menu always end in an exit: with a take
// recording, the offer's first two choices end it first and the third keeps
// it recording and exits nothing; with none, the exit goes ahead.
func TestExitOffer(t *testing.T) {
	o := exitOffer("restart", 83*time.Second)
	if o.title != "正在录音 1:23" || o.info != "重启前先结束这条录音。结束后最多再等 30 秒识别和送达，然后重启；没送达的录音留在录音历史里。" {
		t.Errorf("offer %q / %q", o.title, o.info)
	}
	var labels []string
	for _, c := range o.choices {
		labels = append(labels, c.label)
	}
	if want := []string{"停止并粘贴，然后重启", "取消录音，然后重启", "继续录音"}; !slices.Equal(labels, want) {
		t.Errorf("choices %q, want %q", labels, want)
	}
	if q := exitOffer("quit", time.Second); q.choices[0].label != "停止并粘贴，然后退出" || q.choices[1].label != "取消录音，然后退出" {
		t.Errorf("quit's choices %+v", q.choices)
	}
	rec := voiceTake{ID: "20200101-120000", Recording: true}
	for _, c := range []struct {
		name   string
		tk     voiceTake
		choice int
		want   map[string]string // nil sends nothing
	}{
		{"accepted: stop and paste", rec, 0, map[string]string{"end": "paste", "take": "20200101-120000"}},
		{"accepted: cancel", rec, 1, map[string]string{"end": "cancel", "take": "20200101-120000"}},
		{"refused: keep recording", rec, 2, nil},
		{"alert closed some other way", rec, -1, nil},
		{"a choice past the buttons", rec, 3, nil},
		{"nothing recording", voiceTake{}, -1, map[string]string{"end": "paste"}},
	} {
		if args := exitArgs(c.tk, o, c.choice); !maps.Equal(args, c.want) || (args == nil) != (c.want == nil) {
			t.Errorf("%s: args %v, want %v", c.name, args, c.want)
		}
	}
}

// fakeEnder is a controller for endAndExit: stuck blocks StopFrom and
// CancelFrom forever, as a held mutex would.
type fakeEnder struct {
	mu    sync.Mutex
	calls []string
	stuck chan struct{}
	left  []string
	ok    bool          // what StopFrom and CancelFrom report, when not stuck
	drain time.Duration // the bound Drain was given
}

func (f *fakeEnder) note(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeEnder) StopFrom(via, id string) bool {
	f.note("stop " + via + " " + id)
	if f.stuck != nil {
		<-f.stuck
	}
	return f.ok
}
func (f *fakeEnder) CancelFrom(via, id string) bool {
	f.note("cancel " + via + " " + id)
	if f.stuck != nil {
		<-f.stuck
	}
	return f.ok
}
func (f *fakeEnder) Drain(d time.Duration) []string {
	f.note("drain")
	f.mu.Lock()
	f.drain = d
	f.mu.Unlock()
	return f.left
}

// Restart or quit from the menu exits in every state: once what is in flight
// is done, and at its bound plus the grace when the controller never
// answers. It exits once.
func TestEndAndExit(t *testing.T) {
	type exited struct {
		left     []string
		answered bool
		after    time.Duration
	}
	run := func(f *fakeEnder, end string, max time.Duration) (chan exited, time.Time) {
		ch := make(chan exited, 4)
		t0 := time.Now()
		endAndExit(f, end, "", max, func(left []string, answered bool) { ch <- exited{left, answered, time.Since(t0)} })
		return ch, t0
	}
	f := &fakeEnder{ok: true, left: []string{"20200101-120000 transcribing"}}
	ch, _ := run(f, "cancel", 100*time.Millisecond)
	select {
	case e := <-ch:
		if !e.answered || !slices.Equal(e.left, f.left) {
			t.Fatalf("exit %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit")
	}
	if got := strings.Join(f.calls, ","); got != "cancel menu ,drain" {
		t.Fatalf("calls %s", got)
	}
	select { // the armed timer fires at 1.1 s: it must not exit again
	case e := <-ch:
		t.Fatalf("a second exit %+v", e)
	case <-time.After(100*time.Millisecond + exitGrace + 300*time.Millisecond):
	}

	stuck := &fakeEnder{stuck: make(chan struct{})}
	ch, _ = run(stuck, "paste", 100*time.Millisecond)
	select {
	case e := <-ch:
		if e.answered || e.after < 100*time.Millisecond+exitGrace || e.after > 100*time.Millisecond+exitGrace+500*time.Millisecond {
			t.Fatalf("exit %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exit while the controller never answers")
	}
	close(stuck.stuck) // the controller answers late: it must not exit again
	select {
	case e := <-ch:
		t.Fatalf("a second exit %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}

// The control socket's stop and cancel: each calls its own, via ctl unless
// the args say menu, with the take the menu showed; ending nothing fails.
func TestTakeRequest(t *testing.T) {
	for _, c := range []struct {
		cmd, args string
		ok        bool
		call, err string
	}{
		{"stop", "", true, "stop ctl ", ""},
		{"cancel", "", true, "cancel ctl ", ""},
		{"stop", `{"via":"menu","take":"20200101-120000"}`, true, "stop menu 20200101-120000", ""},
		{"cancel", `{"via":"menu","take":"20200101-120000"}`, true, "cancel menu 20200101-120000", ""},
		{"stop", `{"via":"menu","take":"20200101-120000"}`, false, "stop menu 20200101-120000", "take 20200101-120000 is not recording"},
		{"cancel", "", false, "cancel ctl ", "no take is recording"},
		{"stop", `{"via":"page"}`, true, "", `via "page": menu or ctl`},
	} {
		f := &fakeEnder{ok: c.ok}
		var raw json.RawMessage
		if c.args != "" {
			raw = json.RawMessage(c.args)
		}
		err := takeRequest(f, c.cmd, raw)
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if got := strings.Join(f.calls, ","); got != c.call || gotErr != c.err {
			t.Errorf("%s %s: calls %q err %q, want %q %q", c.cmd, c.args, got, gotErr, c.call, c.err)
		}
	}
	for _, c := range []struct{ args, end, take, err string }{
		{"", "", "", ""}, {`{"end":"paste"}`, "paste", "", ""}, {`{"end":"cancel","take":"20200101-120000"}`, "cancel", "20200101-120000", ""}, {`{"end":"now"}`, "", "", `end "now": paste or cancel`},
	} {
		end, take, err := exitEnd(json.RawMessage(c.args))
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if end != c.end || take != c.take || gotErr != c.err {
			t.Errorf("exitEnd %s: %q %q %q", c.args, end, take, gotErr)
		}
	}
}

// Restart and quit from the menu end the recording take from the menu, give
// what is in flight drainMax, and exit 75 and 0.
func TestMenuExit(t *testing.T) {
	for _, c := range []struct {
		cmd, end, call string
		code           int
	}{{"restart", "paste", "stop menu ", 75}, {"quit", "cancel", "cancel menu 20200101-120000", 0}} {
		f := &fakeEnder{ok: true}
		code := make(chan int, 2)
		menuExit(f, c.cmd, c.end, "20200101-120000", func(n int) { code <- n })
		select {
		case n := <-code:
			if n != c.code {
				t.Errorf("%s: exit %d, want %d", c.cmd, n, c.code)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: no exit", c.cmd)
		}
		f.mu.Lock()
		calls, d := strings.Join(f.calls, ","), f.drain
		f.mu.Unlock()
		if calls != c.call+",drain" || d < drainMax-time.Second || d > drainMax {
			t.Errorf("%s: calls %q, drain bound %v; want %q then drain at %v", c.cmd, calls, d, c.call, drainMax)
		}
	}
}

// The offer's cancel ends the take it was shown for; when that take no
// longer records, what records is stopped to paste, never cancelled.
func TestExitCancelsTheOfferedTake(t *testing.T) {
	for _, c := range []struct {
		name  string
		ok    bool // CancelFrom finds the offered take recording
		calls string
	}{
		{"the offered take records", true, "cancel menu 20200101-120000,drain"},
		{"another take records", false, "cancel menu 20200101-120000,stop menu ,drain"},
	} {
		f := &fakeEnder{ok: c.ok}
		done := make(chan struct{})
		endAndExit(f, "cancel", "20200101-120000", 100*time.Millisecond, func([]string, bool) { close(done) })
		<-done
		f.mu.Lock()
		got := strings.Join(f.calls, ",")
		f.mu.Unlock()
		if got != c.calls {
			t.Errorf("%s: calls %q, want %q", c.name, got, c.calls)
		}
	}
}

// fakeCtl is a controller for takeCommands.
type fakeCtl struct {
	fakeEnder
	busy, rec bool
}

func (f *fakeCtl) ToggleFrom(trigger string) { f.note("toggle " + trigger) }
func (f *fakeCtl) Busy() bool                { return f.busy }
func (f *fakeCtl) Close(string) bool         { return !f.busy }
func (f *fakeCtl) Reopen()                   {}
func (f *fakeCtl) State() session.State      { return session.Idle }
func (f *fakeCtl) Recording() (string, time.Duration, bool) {
	if f.rec {
		return "20200101-120000", time.Second, true
	}
	return "", 0, false
}

// The control socket's take commands: each reaches its own controller call;
// a menu exit refuses every start while it waits, exits once, and a second
// menu exit arms nothing; a plain restart refuses while a take is in flight.
func TestTakeCommands(t *testing.T) {
	req := func(cmd, args string) ctl.Request {
		r := ctl.Request{Cmd: cmd}
		if args != "" {
			r.Args = json.RawMessage(args)
		}
		return r
	}
	var flashes []string
	codes := make(chan int, 4)
	f := &fakeCtl{fakeEnder: fakeEnder{ok: true}}
	k := &takeCommands{ctrl: f, flash: func(m string) { flashes = append(flashes, m) }, exit: func(n int) { codes <- n }}
	for _, c := range []struct{ cmd, args, call string }{
		{"toggle", "", "toggle ctl"},
		{"stop", `{"via":"menu","take":"20200101-120000"}`, "stop menu 20200101-120000"},
		{"cancel", "", "cancel ctl "},
	} {
		f.calls = nil
		resp, ok := k.handle(req(c.cmd, c.args))
		if !ok || !resp.OK || strings.Join(f.calls, ",") != c.call {
			t.Errorf("%s: ok %v %+v calls %q, want %q", c.cmd, ok, resp, f.calls, c.call)
		}
	}
	if _, ok := k.handle(req("status", "")); ok {
		t.Error("status taken by the take commands")
	}

	f.busy = true
	if resp, _ := k.handle(req("quit", "")); resp.OK {
		t.Error("a plain quit went ahead with a take in flight")
	}
	f.calls = nil
	if resp, _ := k.handle(req("restart", `{"end":"paste"}`)); !resp.OK {
		t.Fatalf("menu restart: %+v", resp)
	}
	if resp, _ := k.handle(req("toggle", "")); resp.OK || !strings.Contains(resp.Error, "exiting (restart)") {
		t.Errorf("toggle while the exit waits: %+v", resp)
	}
	if !k.refuseStart() || !slices.Equal(flashes, []string{"正在重启", "正在重启"}) {
		t.Errorf("a tap while the exit waits: flashes %q", flashes)
	}
	if resp, _ := k.handle(req("quit", `{"end":"cancel"}`)); !resp.OK {
		t.Errorf("a second menu exit: %+v", resp)
	}
	f.rec = true
	if k.refuseStart() {
		t.Error("a tap that stops the recording take was refused")
	}
	select {
	case n := <-codes:
		if n != 75 {
			t.Errorf("exit %d, want 75", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit")
	}
	select {
	case n := <-codes:
		t.Errorf("a second exit %d", n)
	case <-time.After(300 * time.Millisecond):
	}
	f.mu.Lock()
	if got := strings.Join(f.calls, ","); got != "stop menu ,drain" {
		t.Errorf("calls %q: the second menu exit reached the controller", got)
	}
	f.mu.Unlock()

	f2 := &fakeCtl{}
	k2 := &takeCommands{ctrl: f2, flash: func(string) {}, exit: func(n int) { codes <- n }}
	if resp, _ := k2.handle(req("restart", "")); !resp.OK {
		t.Fatalf("plain restart idle: %+v", resp)
	}
	select {
	case n := <-codes:
		if n != 75 {
			t.Errorf("plain restart exit %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("plain restart did not exit")
	}
}

// fakeTaker is a controller for takeTap.
type fakeTaker struct {
	calls []string
	used  bool
	why   string
}

func (f *fakeTaker) Cancel() { f.calls = append(f.calls, "cancel") }
func (f *fakeTaker) Hotkey() { f.calls = append(f.calls, "hotkey") }
func (f *fakeTaker) EnterWhy() (bool, string) {
	f.calls = append(f.calls, "enter")
	return f.used, f.why
}
func (f *fakeTaker) ToggleAt(trigger string, _ time.Time) {
	f.calls = append(f.calls, "toggle "+trigger)
}

// A key tap's Result: Esc cancels; the chord is the hotkey, also while a
// menu exit waits; a Return no take uses goes back to the app; a tap
// toggles, but starts nothing while a menu exit waits.
func TestTakeTap(t *testing.T) {
	idle := &takeCommands{ctrl: &fakeCtl{}, flash: func(string) {}}
	exiting := &takeCommands{ctrl: &fakeCtl{}, flash: func(string) {}}
	cmd := "restart"
	exiting.exiting.Store(&cmd)
	for _, c := range []struct {
		name    string
		r       mac.Result
		k       *takeCommands
		used    bool
		calls   string
		pressed bool
	}{
		{"esc", mac.Result{Cancel: true, Swallow: true}, idle, false, "cancel", false},
		{"chord", mac.Result{Chord: true, Swallow: true}, idle, false, "hotkey", false},
		{"chord while a menu exit waits", mac.Result{Chord: true, Swallow: true}, exiting, false, "hotkey", false},
		{"return used", mac.Result{Enter: true, Swallow: true}, idle, true, "enter", false},
		{"return used by no take", mac.Result{Enter: true, Swallow: true}, idle, false, "enter", true},
		{"tap", mac.Result{Tap: true}, idle, false, "toggle tap", false},
		{"tap while a menu exit waits", mac.Result{Tap: true}, exiting, false, "", false},
	} {
		f := &fakeTaker{used: c.used, why: "after a cancel"}
		pressed := false
		takeTap(c.r, time.Now(), f, c.k, func() { pressed = true })
		if got := strings.Join(f.calls, ","); got != c.calls || pressed != c.pressed {
			t.Errorf("%s: calls %q pressed %v, want %q %v", c.name, got, pressed, c.calls, c.pressed)
		}
	}
}
