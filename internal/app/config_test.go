package app

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/asr"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/post"
)

// isolate points every config and data location into a temp dir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("MEGAVOICE_CONFIG", "")
	return filepath.Join(dir, "megavoice")
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNoFileIsDefaults(t *testing.T) {
	isolate(t)
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "" || !reflect.DeepEqual(l.Config, Default()) {
		t.Fatalf("got %+v from %q, want the defaults", l.Config, l.Path)
	}
	if got := l.Meeting.Ingest.MinDuration; got != Duration(2*time.Minute) {
		t.Fatalf("meeting.ingest.min_duration %s, want 2m", got)
	}
	if _, err := Load(LoadOpts{Path: filepath.Join(t.TempDir(), "missing.toml")}); err == nil {
		t.Fatal("a named file that is absent loaded")
	}
}

// The commented default config, uncommented, is exactly the defaults and
// names every key, so it cannot drift from the struct.
func TestDefaultTemplateIsTheDefaults(t *testing.T) {
	dir := isolate(t)
	body := regexp.MustCompile(`(?m)^# ([a-z_]+ = )`).ReplaceAllString(string(Template()), "$1")
	path := writeConfig(t, dir, body)
	t.Setenv("XDG_CONFIG_HOME", "") // the template names paths as they are with XDG unset
	l, err := Load(LoadOpts{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Config, Default()) {
		t.Fatalf("template\n%+v\ndefaults\n%+v", l.Config, Default())
	}
	for _, key := range leafKeys(reflect.TypeOf(Config{}), "") {
		if l.SourceOf(key) != "file" {
			t.Errorf("%s is not in the template", key)
		}
	}
}

func leafKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		key := prefix + f.Tag.Get("toml")
		if f.Type.Kind() == reflect.Struct {
			out = append(out, leafKeys(f.Type, key+".")...)
		} else {
			out = append(out, key)
		}
	}
	return out
}

func TestPrecedence(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, `
[tap]
key = "right_shift"
[capture]
device = "plughw:2,0"
channel = 3
`)
	l, err := Load(LoadOpts{Sets: []string{"capture.channel=4", "take.chunk_pause=1500ms", "deliver.herdr_apps=[\"a\", \"b\"]"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.Tap.Key = "right_shift"
	want.Capture.Device = "plughw:2,0"
	want.Capture.Channel = 4
	want.Take.ChunkPause = MS(1500)
	want.Deliver.HerdrApps = []string{"a", "b"}
	if !reflect.DeepEqual(l.Config, want) {
		t.Fatalf("got %+v\nwant %+v", l.Config, want)
	}
	for key, src := range map[string]string{
		"tap.key": "file", "capture.device": "file", "capture.channel": "flag",
		"take.chunk_pause": "flag", "tap.window": "default", "asr.funasr.root": "default",
	} {
		if got := l.SourceOf(key); got != src {
			t.Errorf("%s: source %s, want %s", key, got, src)
		}
	}
}

func TestMegavoiceConfigNamesTheFile(t *testing.T) {
	isolate(t)
	path := writeConfig(t, t.TempDir(), "[capture]\nhost = \"elsewhere\"\n")
	t.Setenv("MEGAVOICE_CONFIG", path)
	l, err := Load(LoadOpts{})
	if err != nil || l.Path != path || l.Capture.Host != "elsewhere" {
		t.Fatalf("got %q from %q, %v", l.Capture.Host, l.Path, err)
	}
	other := writeConfig(t, t.TempDir(), "[capture]\nhost = \"flagged\"\n")
	if l, _ = Load(LoadOpts{Path: other}); l.Capture.Host != "flagged" {
		t.Fatalf("--config lost to MEGAVOICE_CONFIG: %q", l.Capture.Host)
	}
}

func TestRejects(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"[tap]\nbogus = 1\n", "unknown key tap.bogus"},
		{"[nope]\nx = 1\n", "unknown key nope"},
		{"[tap]\nwindow = 400\n", `duration "400"`},
		{"[capture]\nsource = \"ssh\"\nhost = \"h\"\ndevice = \"d\"\nchannel = 7\n", "capture.channel: 7 is outside 1..6"},
		{"[capture]\nsource = \"ssh\"\ndevice = \"d\"\n", "capture.host: empty"},
		{"[capture]\nsource = \"ssh\"\nhost = \"h\"\n", "capture.device: empty"},
		{"[capture]\nsource = \"usb\"\n", "capture.source: \"usb\" is not one of local, ssh, remote"},
		{"[capture]\nsource = \"remote\"\n", "capture.remote: empty; pair one with megavoice mic add"},
		{"[capture]\nmic_channel = -1\n", "capture.mic_channel: must not be negative"},
		{"[capture]\nmic = \"\"\n", "capture.mic: empty"},
		{"[mic_server]\nlisten = \"7866\"\n", "mic_server.listen: \"7866\" is not host:port"},
		{"[mic_server]\nlisten = \":0\"\n", "mic_server.listen: \":0\": port \"0\" is not 1..65535"},
		{"[mic_server]\nname = \"tea kettle\"\n", "mic_server.name: \"tea kettle\" has a space or /"},
		{"[mic_server]\nstate = \"mic\"\n", "mic_server.state: \"mic\" is not an absolute path"},
		{"[mic_server]\nsocket = \"ctl.sock\"\n", "mic_server.socket: \"ctl.sock\" is not an absolute path"},
		{"[mic_server]\nadmin_group = \"\"\n", "mic_server.admin_group: \"\" is not a group name"},
		{"[take]\nchunk_max = \"0s\"\n", "take.chunk_max"},
		{"[take]\nwhole_max = \"-1s\"\n", "take.whole_max"},
		{"[asr]\nengine = \"whisper\"\n", "asr.engine"},
		{"[asr.funasr]\nmode = \"daemon\"\n", "asr.funasr.mode"},
		{"[asr.funasr]\nencoder = \"npu\"\nvad = \"npu\"\n", "asr.funasr.vad"},
		{"[asr.funasr]\nthreads = 0\n", "asr.funasr.threads"},
		{"[asr.funasr]\nllm_threads = -1\n", "asr.funasr.llm_threads"},
		{"[asr.funasr]\nllm = \"/nonexistent/model.gguf\"\n", "asr.funasr.llm: stat /nonexistent/model.gguf"},
		{"[asr.funasr]\nllm = \"/\"\n", "asr.funasr.llm: / is not a file"},
		{"[deliver]\nherdr_apps = [\"\"]\n", "deliver.herdr_apps"},
		{"[capture]\nchannel = \"2\"\n", "capture.channel"},
		{"[meeting.capture]\nmic = \"\"\n", "meeting.capture.mic"},
		{"[meeting.process]\ndiarizer = \"pyannote\"\n", "meeting.process.diarizer"},
		{"[meeting.page]\nurl = \"pages.example\"\n", "meeting.page.url"},
		{"[meeting.page]\nslug = \"Meetings_1\"\n", "meeting.page.slug"},
		{"[meeting.page]\nslug = \"meetings-1\"\n", "meeting.page.url: empty while slug is set"},
		{"[meeting.page]\nidentity = \"root\"\n", "meeting.page.identity"},
		{"[meeting.page]\ntoken_file = \"/t\"\nidentity = \"ucc\"\n", "token_file and identity both set"},
		{"[meeting.upload]\npart_mib = 4\n", "meeting.upload.part_mib: 4 is outside 5..95"},
		{"[meeting.upload]\nretention_days = -1\n", "meeting.upload.retention_days"},
		{"[meeting.ingest]\nauto = \"yes\"\n", "meeting.ingest.auto"},
		{"[meeting.ingest]\nsources = [\"feishu\"]\n", "meeting.ingest.feishu_since: unset while sources has feishu"},
		{"[meeting.ingest]\nfeishu_since = \"2020-03-14 09:05\"\n", "meeting.ingest.feishu_since: parsing time"},
		{"[meeting.ingest]\nmin_duration = \"-1m\"\n", "meeting.ingest.min_duration: must not be negative"},
		{"[meeting.ingest]\nmin_duration = 120\n", "meeting.ingest.min_duration"},
		{"[asr.doubao]\nurl = \"https://ark.cn-beijing.volces.com/api/v3/audio\"\n", "asr.doubao.url: \"https://ark.cn-beijing.volces.com/api/v3/audio\" is not a plan ASR endpoint"},
		{"[asr.doubao]\nurl = \"wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async\"\n", "asr.doubao.url"},
		{"[asr.doubao]\nresource_id = \"\"\n", "asr.doubao.resource_id"},
		{"[asr]\nengine = \"doubao\"\n[asr.doubao]\nkey_file = \"/nonexistent/.env\"\n", "asr.doubao.key_file: open /nonexistent/.env"},
		{"[compare]\non = true\n[asr.doubao]\nkey_file = \"/nonexistent/.env\"\n", "asr.doubao.key_file"},
		{"[compare]\nengines = [\"whisper\"]\n", "compare.engines: \"whisper\" is not one of funasr, doubao"},
		{"[compare]\nengines = [\"doubao\", \"doubao\"]\n", "compare.engines: doubao twice"},
		{"[compare]\naddr = \"0.0.0.0:7865\"\n", "compare.addr: \"0.0.0.0:7865\" is not a loopback"},
		{"[compare]\naddr = \"localhost:7865\"\n", "compare.addr: \"localhost:7865\" is not a loopback IP"},
		{"[store]\nlabels = \"\"\n", "store.labels"},
	} {
		dir := isolate(t)
		writeConfig(t, dir, tc.body)
		if _, err := Load(LoadOpts{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want an error with %q", tc.body, err, tc.want)
		}
	}
	isolate(t)
	if _, err := Load(LoadOpts{Sets: []string{"capture.nope=1"}}); err == nil || !strings.Contains(err.Error(), "capture.nope") {
		t.Errorf("--set of an unknown key: %v", err)
	}
	if _, err := Load(LoadOpts{Sets: []string{"capture.channel"}}); err == nil {
		t.Error("--set without = loaded")
	}
}

// The backup key takes either keyword exactly as written, nothing at all, or
// a device UID. Anything made only of lower-case letters that is not a
// keyword, and a keyword typed in capitals, reads as a typo and fails.
func TestCaptureBackup(t *testing.T) {
	cases := []struct {
		value string
		loads bool
	}{
		{"boom-mic", true}, // a hyphen makes it a UID, not a word
		{"OFF", false},
		{"auto", true},
		{"aoto", false},
		{"", true},
		{"off", true},
	}
	for _, c := range cases {
		writeConfig(t, isolate(t), "[capture]\nbackup = "+strconv.Quote(c.value)+"\n")
		_, err := Load(LoadOpts{})
		if c.loads && err != nil {
			t.Errorf("backup = %q refused: %v", c.value, err)
		}
		if !c.loads && (err == nil || !strings.Contains(err.Error(), "capture.backup")) {
			t.Errorf("backup = %q: %v, want a capture.backup error", c.value, err)
		}
	}
}

// With no file, dictation records from this Mac's default input: nothing
// points at a host that may not exist. The ssh keys are checked only
// when the source is ssh, so a file that keeps them loads under local.
func TestCaptureDefaultsLocal(t *testing.T) {
	isolate(t)
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if c := l.Capture; c.Source != "local" || c.Mic != "default" || c.MicChannel != 0 || c.Host != "" || c.Device != "" {
		t.Fatalf("capture %+v", c)
	}
	var out strings.Builder
	l.WriteEffective(&out, "megavoice")
	for _, line := range strings.Split(out.String(), "\n") {
		if (strings.HasPrefix(line, "host = ") || strings.HasPrefix(line, "device = ")) && !strings.Contains(line, `= ""`) {
			t.Fatalf("defaults name a host: %s", line)
		}
	}
	dir := isolate(t)
	writeConfig(t, dir, "[capture]\ndevice = \"plughw:2,0\"\nchannel = 9\n")
	if _, err := Load(LoadOpts{}); err != nil {
		t.Fatalf("ssh keys under local: %v", err)
	}
}

// Switching a file to ssh: host first, then the source; the source alone is
// refused by the key it lacks. Switching back keeps the ssh keys.
func TestSetCaptureSourceSSH(t *testing.T) {
	dir := isolate(t)
	path := writeConfig(t, dir, "[capture]\ndevice = \"plughw:2,0\"\n")
	if err := Set(path, "capture.source", Value("ssh"), nil); err == nil || !strings.Contains(err.Error(), "capture.host: empty") {
		t.Fatalf("source ssh without a host: %v", err)
	}
	for _, kv := range [][2]string{{"capture.host", "pantry-pi"}, {"capture.source", "ssh"}} {
		if err := Set(path, kv[0], Value(kv[1]), nil); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if c := l.Capture; c.Source != "ssh" || c.Host != "pantry-pi" || c.Device != "plughw:2,0" || c.Channel != 2 {
		t.Fatalf("capture %+v", c)
	}
	if err := Set(path, "capture.source", Value("local"), nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := Load(LoadOpts{}); l.Capture.Source != "local" || l.Capture.Host != "pantry-pi" {
		t.Fatalf("back to local: %+v", l.Capture)
	}
}

// The effective config is itself a config file that loads back to the same
// settings.
func TestEffectiveRoundTrips(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, "[tap]\nkey = \"right_shift\"\n[take]\nmax_duration = \"90s\"\n[meeting.ingest]\nauto = true\ncommand = [\"ingest\"]\n[meeting.page]\nurl = \"https://pages.example\"\nslug = \"meetings-0123456789abcdef\"\n")
	l, err := Load(LoadOpts{Sets: []string{"asr.funasr.gpu_layers=0"}})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	l.WriteEffective(&out, "megavoice")
	if !strings.Contains(out.String(), `key = "right_shift"`) || !regexp.MustCompile(`gpu_layers = 0 +# flag`).MatchString(out.String()) ||
		!regexp.MustCompile(`auto = true +# file`).MatchString(out.String()) {
		t.Fatalf("effective config:\n%s", out.String())
	}
	path := writeConfig(t, t.TempDir(), out.String())
	back, err := Load(LoadOpts{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Config, l.Config) {
		t.Fatalf("reloaded %+v\nwant %+v", back.Config, l.Config)
	}
}

func TestLegacyEnvWarns(t *testing.T) {
	t.Setenv("MEGAVOICE_TAP_KEY", "right_shift")
	w := legacyEnv()
	if len(w) != 1 || !strings.Contains(w[0], "MEGAVOICE_TAP_KEY") || !strings.Contains(w[0], "tap.key") {
		t.Fatalf("warnings %q", w)
	}
}

// The parts are built from a Config value.
func TestPartsFromConfig(t *testing.T) {
	isolate(t)
	c := Default()
	c.Take.ChunkPause, c.Take.ChunkMax = MS(700), MS(9000)
	c.ASR.FunASR = FunASRConfig{Root: "/r", Mode: "resident", GPULayers: 0, Encoder: "cpu", VAD: "gpu", Threads: 3, LLMThreads: 6, LLM: "/ft.gguf"}

	if ch := c.Chunker().(*audio.Chunker); ch.Pause != 700*time.Millisecond || ch.Max != 9*time.Second {
		t.Errorf("chunker %+v", ch)
	}
	fb, ok := c.Transcriber().(*Fallback)
	if !ok {
		t.Fatalf("resident mode gave %T", c.Transcriber())
	}
	if r := fb.new(); r.Root != "/r" || r.GPU != 0 || r.EncGPU || !r.VADGPU || r.Threads != 3 || r.LLMThreads != 6 || r.LLM != "/ft.gguf" {
		t.Errorf("resident %+v", r)
	}
	c.ASR.FunASR.Mode, c.ASR.FunASR.GPULayers = "exec", 99
	// the CLI runs with the resident process's engine flags: an LLM left on
	// the CPU gives other text than dictation's Metal
	if e, ok := c.Transcriber().(asr.FunASR); !ok || e.Root != "/r" || e.LLM != "/ft.gguf" || e.EncGPU || !e.VADGPU ||
		e.GPU != 99 || e.Threads != 3 || e.LLMThreads != 6 {
		t.Errorf("exec mode gave %#v", c.Transcriber())
	}
}

// megavoice reads the shared file with its meeting tables and prints only
// its own tables; a caller's Check joins the validation.
func TestVoiceTablesAndCheck(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, "[meeting.page]\nurl = \"https://pages.example\"\nslug = \"meetings-0123456789abcdef\"\nidentity = \"ucc\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Meeting.Page.Identity != "ucc" || l.Meeting.Upload.PartMiB != 16 {
		t.Fatalf("meeting %+v", l.Meeting)
	}
	var out strings.Builder
	l.WriteEffective(&out, "megavoice", VoiceTables...)
	if strings.Contains(out.String(), "meeting") || !strings.Contains(out.String(), "\n[store]\n") {
		t.Fatalf("megavoice's tables:\n%s", out.String())
	}
	_, err = Load(LoadOpts{Check: func(Config) []string { return []string{"tap.key: no such key"} }})
	if err == nil || !strings.Contains(err.Error(), "config.toml: tap.key: no such key") {
		t.Fatalf("check: %v", err)
	}
}

// The plan key is read from key_file, a KEY=value line or the whole file;
// its errors never carry the file's text.
func TestDoubaoKey(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("LOG_LEVEL=info\n\nexport SPEECH_TOKEN=\"t-9f2\"\n# end\n"), 0o600)
	bare := filepath.Join(dir, "key")
	os.WriteFile(bare, []byte("k-bare\n"), 0o600)
	for _, c := range []struct {
		d       DoubaoConfig
		key     string
		errPart string
	}{
		{DoubaoConfig{KeyFile: env, KeyVar: "SPEECH_TOKEN"}, "t-9f2", ""},
		{DoubaoConfig{KeyFile: bare}, "k-bare", ""},
		{DoubaoConfig{KeyFile: env, KeyVar: "MISSING"}, "", "has no MISSING line"},
		{DoubaoConfig{KeyFile: env}, "", ""}, // no key_var: the whole file, which is not a key but is not empty
		{DoubaoConfig{KeyFile: filepath.Join(dir, "none")}, "", "no such file"},
	} {
		k, err := c.d.Key()
		switch {
		case c.errPart != "" && (err == nil || !strings.Contains(err.Error(), c.errPart) || strings.Contains(err.Error(), "t-9f2")):
			t.Errorf("%+v: %v, want an error with %q and no key", c.d, err, c.errPart)
		case c.errPart == "" && c.key != "" && (err != nil || k != c.key):
			t.Errorf("%+v: %q, %v; want %q", c.d, k, err, c.key)
		}
	}
}

// compare.engines replaces the default list; doubao as the engine loads
// with its key file present.
func TestCompareAndDoubaoLoad(t *testing.T) {
	dir := isolate(t)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("k\n"), 0o600)
	writeConfig(t, dir, "[asr]\nengine = \"doubao\"\n[asr.doubao]\nkey_file = \""+key+"\"\nkey_var = \"\"\n[compare]\non = true\nengines = [\"funasr\"]\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Compare.Engines, []string{"funasr"}) || !l.Uses("doubao") || !l.Uses("funasr") {
		t.Fatalf("compare %+v", l.Compare)
	}
	if d := l.Streamer().(*asr.Doubao); d.URL != Default().ASR.Doubao.URL || !d.TwoPass || d.Hotwords == nil {
		t.Fatalf("streamer %+v", d)
	}
	if k, err := d0(l).Key(); err != nil || k != "k" {
		t.Fatalf("key %q, %v", k, err)
	}
	l.ASR.Engine = "funasr"
	if l.Streamer() != nil {
		t.Fatal("funasr primary streams")
	}
}

func d0(l Loaded) *asr.Doubao { return l.Doubao() }

// asr.funasr.llm names an existing file; ~ expands like the other paths.
func TestLLMOverride(t *testing.T) {
	dir := isolate(t)
	t.Setenv("HOME", t.TempDir())
	gguf := filepath.Join(os.Getenv("HOME"), "model.gguf")
	if err := os.WriteFile(gguf, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, dir, "[asr.funasr]\nllm = \"~/model.gguf\"\n")
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if l.ASR.FunASR.LLM != gguf {
		t.Fatalf("llm %q, want %q", l.ASR.FunASR.LLM, gguf)
	}
	if l, err := Load(LoadOpts{Sets: []string{`asr.funasr.llm=""`}}); err != nil || l.ASR.FunASR.LLM != "" {
		t.Fatalf("--set llm=\"\" (the rollback): %q, %v", l.ASR.FunASR.LLM, err)
	}
}

// Compare mode is read from the file as each take starts: a toggle
// applies at the next take without a restart.
func TestFanoutReadsCompareAtEachTake(t *testing.T) {
	dir := isolate(t)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("k\n"), 0o600)
	base := "[asr.doubao]\nkey_file = \"" + key + "\"\nkey_var = \"\"\n"
	path := writeConfig(t, dir, base)
	c := Default()
	fan := c.Fanout(LoadOpts{}, nil)
	names := func() string {
		var out []string
		for _, e := range fan.Engines() {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "" {
		t.Fatalf("compare off: engines %q", got)
	}
	if err := Set(path, "compare.on", "true", nil); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "funasr,doubao" || fan.Primary != "funasr" {
		t.Fatalf("compare on: engines %q, primary %s", got, fan.Primary)
	}
	os.WriteFile(path, []byte("[compare\n"), 0o644) // a broken file keeps the last good setting
	if got := names(); got != "funasr,doubao" {
		t.Fatalf("broken file: engines %q", got)
	}
}

// The mic server listens on every interface at 7866 and keeps its state
// under XDG_STATE_HOME; its control socket is in that state directory, so
// whoever reads the server's config file finds the socket.
func TestMicServerDefaults(t *testing.T) {
	isolate(t)
	home, _ := os.UserHomeDir()
	l, err := Load(LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := l.MicServer
	if m.Listen != ":7866" || m.Name != "" || m.State != filepath.Join(home, ".local", "state", "megavoice", "mic") {
		t.Fatalf("mic_server %+v", m)
	}
	if h, _ := os.Hostname(); m.HostName() != h {
		t.Errorf("HostName %q, want the hostname %q", m.HostName(), h)
	}
	dir := isolate(t)
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if Default().MicServer.State != "/xdg/megavoice/mic" {
		t.Errorf("state ignores XDG_STATE_HOME: %s", Default().MicServer.State)
	}
	writeConfig(t, dir, "[mic_server]\nname = \"porch\"\nstate = \"/var/lib/megavoice-mic\"\n")
	if l, err = Load(LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	if l.MicServer.HostName() != "porch" || l.MicServer.CtlPath() != "/var/lib/megavoice-mic/ctl.sock" || l.MicServer.AdminGroup != "megavoice-mic" {
		t.Fatalf("mic_server %+v", l.MicServer)
	}
	// a system service moves the socket out of its 0700 state directory
	writeConfig(t, dir, "[mic_server]\nstate = \"/var/lib/megavoice-mic\"\nsocket = \"/run/megavoice-mic/ctl.sock\"\n")
	if l, err = Load(LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	if l.MicServer.CtlPath() != "/run/megavoice-mic/ctl.sock" {
		t.Fatalf("socket: %s", l.MicServer.CtlPath())
	}
}

// Seed writes the example vocabulary only where the user has none: an
// existing file is read as it is and never overwritten.
func TestSeedKeepsUserFiles(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "# mine\nFooBar\n"
	if err := os.WriteFile(HotwordsPath(), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	Seed()
	if b, _ := os.ReadFile(HotwordsPath()); string(b) != mine {
		t.Errorf("hotwords.txt overwritten: %q", b)
	}
	if got := asr.ReadHotwords(HotwordsPath()); !reflect.DeepEqual(got, []string{"FooBar"}) {
		t.Errorf("hotwords %q, want the user's", got)
	}
	seeded, err := os.Open(CorrectionsPath())
	if err != nil {
		t.Fatalf("corrections.tsv not seeded: %v", err)
	}
	defer seeded.Close()
	c, err := post.ParseCorrections(seeded)
	if err != nil || len(c) == 0 {
		t.Errorf("seeded corrections: %d rules, %v", len(c), err)
	}
	if got := c.Apply("Get hub 上有个mark down"); got != "GitHub 上有个Markdown" {
		t.Errorf("seeded corrections: %q", got)
	}
}
