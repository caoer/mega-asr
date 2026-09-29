// Package app is the configuration megavoice and megameet share: one TOML
// file, its defaults, and the parts built from it. It builds on Linux; what
// only the Mac can check (the tap key) the caller passes in as a Check.
package app

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/caoer/mega-asr/internal/audio"
)

// Config is every setting. Default holds the defaults; the config file,
// then --set flags, override them key by key.
type Config struct {
	Tap     TapConfig     `toml:"tap"`
	Capture CaptureConfig `toml:"capture"`
	Take    TakeConfig    `toml:"take"`
	ASR     ASRConfig     `toml:"asr"`
	// Postprocess is [post]; Post() builds its chain.
	Postprocess PostConfig    `toml:"post"`
	Deliver     DeliverConfig `toml:"deliver"`
	Store       StoreConfig   `toml:"store"`
	Compare     CompareConfig `toml:"compare"`
	Meeting     MeetingConfig `toml:"meeting"`
	// MicServer is the Linux build's `mic serve`: this host's input, served
	// to paired Macs.
	MicServer MicServerConfig `toml:"mic_server"`
}

// VoiceTables are megavoice's own tables, the ones its `config show` prints.
var VoiceTables = []string{"tap", "capture", "take", "asr", "post", "deliver", "store", "compare"}

type TapConfig struct {
	Key    string   `toml:"key"`    // the modifier whose lone tap starts and stops a take
	Window Duration `toml:"window"` // longest press that is a tap
	Quiet  Duration `toml:"quiet"`  // no start this soon after another key press
}

// CaptureConfig is where dictation records from. local is an input device
// of this machine — on a Mac a Core Audio input, on Linux an ALSA PCM read
// by arecord; ssh is an ALSA device on another host, read by arecord over
// ssh; remote is a mic server paired with this Mac (`megavoice mic serve`
// on the other machine). Each source's keys are checked only while it is
// the source, so a file keeps the other's settings across a switch.
type CaptureConfig struct {
	Source     string `toml:"source"`      // local, ssh or remote
	Mic        string `toml:"mic"`         // local: an input's UID; "default" follows the system's input
	MicName    string `toml:"mic_name"`    // local: the pinned input's name, for when it is not connected; "" shows the UID
	MicChannel int    `toml:"mic_channel"` // local: 0 mixes every channel, n records channel n (Linux: of a six-channel USB array)
	// BluetoothWarm is how long a Mac's Bluetooth input stays open after a
	// take (local): a Bluetooth headset opened cold records silence for a
	// few seconds while it switches into call mode. 0 closes it at stop.
	BluetoothWarm Duration `toml:"bluetooth_warm"`
	// Backup is a Mac's second input, recorded beside the local one. A
	// stretch of 2 s or more in which the main input is all zero samples
	// while the backup carries a voice is decoded from the backup instead.
	// "auto" takes a wired input other than the main one, a display's or
	// camera's microphone before the built-in; an input's UID pins one; ""
	// or "off" records none. It is never a Bluetooth or wireless input.
	Backup  string `toml:"backup"`
	Host    string `toml:"host"`    // ssh: destination with the device
	Device  string `toml:"device"`  // ssh: ALSA PCM on that host
	Channel int    `toml:"channel"` // ssh: 1-based; on a six-channel USB array (e.g. XVF3800) 2 is the ASR beam
	Remote  string `toml:"remote"`  // remote: a paired mic server's name
}

// MicServerConfig is `megavoice mic serve` on Linux: it records
// capture.source and serves it to the Macs paired with it.
type MicServerConfig struct {
	Listen string `toml:"listen"` // host:port; ":7866" is every interface
	Name   string `toml:"name"`   // the name a pairing Mac sees; "" is this host's name
	State  string `toml:"state"`  // the TLS key and the paired clients; the control socket unless socket names one
	// Socket is the control socket `mic pair|token|status|clients|revoke`
	// reach the server on; "" is ctl.sock in the state directory. A system
	// service puts it where its admin group can reach it (the state
	// directory is 0700).
	Socket string `toml:"socket"`
	// AdminGroup is the group whose members, with root and the server's own
	// user, may use the control socket; the server checks the caller's
	// credentials from the socket.
	AdminGroup string `toml:"admin_group"`
}

// HostName is the name the server gives itself: name, else the hostname.
func (m MicServerConfig) HostName() string {
	if m.Name != "" {
		return m.Name
	}
	h, _ := os.Hostname()
	return h
}

// CtlPath is the control socket: socket, else ctl.sock in the state
// directory. Either comes from the server's config file, so a caller who
// reads that file (root under sudo or over ssh) finds it.
func (m MicServerConfig) CtlPath() string {
	if m.Socket != "" {
		return m.Socket
	}
	return filepath.Join(m.State, "ctl.sock")
}

// CaptureSources are the values capture.source takes.
var CaptureSources = []string{"local", "ssh", "remote"}

type TakeConfig struct {
	MaxDuration Duration `toml:"max_duration"` // a take stops on its own after this
	ChunkPause  Duration `toml:"chunk_pause"`  // a pause this long ends a chunk
	ChunkMax    Duration `toml:"chunk_max"`    // a chunk is cut at its quietest block by this length
	WholeMax    Duration `toml:"whole_max"`    // a take this long or shorter is decoded whole at stop; 0s streams every take
}

// ASRConfig selects the engine; each engine has its own table under [asr].
type ASRConfig struct {
	Engine string       `toml:"engine"` // funasr (local) or doubao (cloud)
	FunASR FunASRConfig `toml:"funasr"`
	Doubao DoubaoConfig `toml:"doubao"`
}

// Engines are the engines asr.engine and compare.engines can name.
var Engines = []string{"funasr", "doubao"}

// DoubaoConfig is Doubao streaming ASR 2.0 on the Volcengine Agent Plan.
// The key stays in its own file: the config names the file, never the key.
type DoubaoConfig struct {
	URL        string `toml:"url"`         // a plan endpoint: wss://openspeech.bytedance.com/api/v3/plan/sauc/…
	ResourceID string `toml:"resource_id"` // volc.seedasr.sauc.duration: ASR 2.0 on the plan
	KeyFile    string `toml:"key_file"`    // the file holding the plan key
	KeyVar     string `toml:"key_var"`     // the KEY=value line in key_file that holds it; "" when the file is the key alone
	TwoPass    bool   `toml:"two_pass"`    // enable_nonstream: each closed sentence re-decoded by the nostream model
	DDC        bool   `toml:"ddc"`         // enable_ddc: fillers and disfluencies dropped by the server
	Hotwords   bool   `toml:"hotwords"`    // send hotwords.txt as request.corpus.context
}

// CompareConfig is compare mode, read from the file at every take: while
// on, each take also goes to the engines listed, beside the primary.
type CompareConfig struct {
	On      bool     `toml:"on"`
	Engines []string `toml:"engines"` // run beside asr.engine, which is never run twice
	Addr    string   `toml:"addr"`    // the Takes page's listener, on the loopback
}

type FunASRConfig struct {
	Root       string `toml:"root"`        // directory with bin/llama-funasr-cli and models/
	LLM        string `toml:"llm"`         // a Qwen3 GGUF (a fine-tune's export) in place of root's models/qwen3-0.6b-q8_0.gguf; "" keeps root's
	Mode       string `toml:"mode"`        // resident (one --serve process) or exec (one CLI run per chunk)
	GPULayers  int    `toml:"gpu_layers"`  // Qwen3 layers on Metal; 0 decodes on the CPU
	Encoder    string `toml:"encoder"`     // gpu (SAN-M on Metal) or cpu
	VAD        string `toml:"vad"`         // gpu (FSMN-VAD on Metal) or cpu
	Threads    int    `toml:"threads"`     // encoder threads when it runs on the CPU
	LLMThreads int    `toml:"llm_threads"` // Qwen3 threads on the CPU; 0 keeps the CLI's 4
}

// PostConfig is the chain applied to a transcript after ASR.
type PostConfig struct {
	// JoinWords add to the filler stage's built-in clause-final words
	// (post.JoinWords): a full stop right after one, before more Chinese
	// text, is a VAD break inside a sentence and becomes a comma.
	JoinWords []string `toml:"join_words"`
}

type DeliverConfig struct {
	HerdrApps []string `toml:"herdr_apps"` // bundle ids of herdr's host terminals
}

type StoreConfig struct {
	Data   string `toml:"data"`   // where takes and their texts are kept
	Labels string `toml:"labels"` // the labels saved from the Takes page, one JSON row each
}

// MeetingConfig is megameet's: recording a meeting, sending it to the
// meetings page, and processing it on the server.
type MeetingConfig struct {
	Data    string               `toml:"data"` // recordings and the pending-upload ledger
	Capture MeetingCaptureConfig `toml:"capture"`
	Process MeetingProcessConfig `toml:"process"`
	Align   MeetingAlignConfig   `toml:"align"`
	Ingest  MeetingIngestConfig  `toml:"ingest"`
	Summary MeetingSummaryConfig `toml:"summary"`
	Page    MeetingPageConfig    `toml:"page"`
	Upload  MeetingUploadConfig  `toml:"upload"`
	Drain   MeetingDrainConfig   `toml:"drain"`
}

type MeetingCaptureConfig struct {
	Apps []string `toml:"apps"` // bundle ids of the processes that play the meeting's audio (a helper, not the app: com.electron.lark.helper); `start` taps these when --app names none
	Mic  string   `toml:"mic"`  // input device; "default" is the system's
}

type MeetingProcessConfig struct {
	Diarizer string `toml:"diarizer"` // none: each track is one speaker
	Speakers int    `toml:"speakers"` // speakers the diarizer expects; 0 lets it estimate; `start --speakers` overrides
}

type MeetingAlignConfig struct {
	FeishuArchive string `toml:"feishu_archive"` // scripts/feishu-minutes' archive; "" aligns nothing
}

type MeetingIngestConfig struct {
	Auto    bool     `toml:"auto"`    // run the ingest when a record reaches aligned
	Wiki    string   `toml:"wiki"`    // the wiki checkout meetings are filed into; register-feishu reads which minutes it holds, and its directory name is the wiki's name in the ingest's `commit:` lines
	Command []string `toml:"command"` // the ingest program; the drain appends `run <record.json> --out <dir> --commit [--replace]`
	Repos   string   `toml:"repos"`   // REPOS_ROOT for the ingest: where it finds its wiki checkouts; "" is the ingest's own default
	// Assets is the git-annex checkout an ingested meeting's registry
	// record is backed up into, beside its audio; "" backs up nothing.
	Assets            string   `toml:"assets"`
	AssetsRemote      string   `toml:"assets_remote"`       // the assets checkout's git remote: fetched, and its main pushed
	AssetsAnnexRemote string   `toml:"assets_annex_remote"` // the annex remote the backup's content is copied to; "" keeps it in git-annex only
	Sources           []string `toml:"sources"`             // the record sources auto ingests; feishu minutes are the backfill's (a page reingest still ingests any)
	// FeishuSince: with feishu in sources, a minute Feishu created at or
	// after it is ingested when eligible (5 min or more, a transcript, two
	// speakers or more); the minutes before it are the backfill's.
	FeishuSince string `toml:"feishu_since"` // RFC 3339
	// MinDuration: a mac, room, phone or file record shorter than it rests
	// at aligned with ingest_skipped; a page reingest still ingests it.
	// Feishu minutes keep their own 5-minute rule.
	MinDuration Duration `toml:"min_duration"`
	// PeopleResolve: a speaker named by hand on the page is resolved to a
	// wiki person page by `<command> people resolve`, written back onto the
	// record, and an ingested meeting re-ingested. Needs a command that has
	// the verb.
	PeopleResolve bool `toml:"people_resolve"`
}

// Since is feishu_since as a time; zero when unset.
func (c MeetingIngestConfig) Since() (time.Time, error) {
	if c.FeishuSince == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, c.FeishuSince)
}

// MeetingSummaryConfig is the model that writes a meeting's display title,
// labels and summary (not a Feishu minute's summary: Feishu's comes with
// it) — the drain's Summarize stage and `megameet titles`.
type MeetingSummaryConfig struct {
	Command  []string `toml:"command"`   // the claude CLI (on the server, the one the ingest uses); empty writes no summaries
	Model    string   `toml:"model"`     // a cheap model: a meeting costs well under $0.05
	MaxChars int      `toml:"max_chars"` // transcript characters sent at most; a longer meeting sends its head and tail
}

// MeetingDrainConfig is the server's: how it reports a failed record.
type MeetingDrainConfig struct {
	Notify []string `toml:"notify"` // a program the one-line failure report is appended to as its last argument (["ccc-telegram", "sendAsBot"]); empty logs only
}

// MeetingPageConfig names the ccc-pages page that holds the registry and
// the files, and the credential for it: a device's token_file, or the
// owner's ucc identity (the server's).
type MeetingPageConfig struct {
	URL       string `toml:"url"`        // the ccc-pages service
	Slug      string `toml:"slug"`       // the meetings page; "" is no page
	TokenFile string `toml:"token_file"` // a contributor or editor secret for this device
	Identity  string `toml:"identity"`   // ucc: the owner Bearer from $UCC_HOME/user-env.sh
}

type MeetingUploadConfig struct {
	PartMiB       int `toml:"part_mib"`       // multipart part size for files above the single-request limit
	RetentionDays int `toml:"retention_days"` // the local copy stays this long after upload
}

// DefaultASRRoot is the FunASR root when the config names none. A packaged
// build sets it with -ldflags -X (package.nix asrRoot), so the service and
// the shell CLI of one install resolve the same root; unset, it is
// $XDG_DATA_HOME/mega-asr/funasr-root, where runtime/funasr-root.nix builds one.
var DefaultASRRoot string

func defaultASRRoot() string {
	if DefaultASRRoot != "" {
		return DefaultASRRoot
	}
	return filepath.Join(dataHome(), "mega-asr", "funasr-root")
}

func Default() Config {
	return Config{
		Tap:     TapConfig{Key: "right_option", Window: MS(400), Quiet: MS(500)},
		Capture: CaptureConfig{Source: "local", Mic: "default", Channel: 2, BluetoothWarm: Duration(2 * time.Minute), Backup: "auto"},
		Take:    TakeConfig{MaxDuration: Duration(30 * time.Minute), ChunkPause: MS(1900), ChunkMax: Duration(30 * time.Second), WholeMax: Duration(time.Minute)},
		ASR: ASRConfig{Engine: "funasr", FunASR: FunASRConfig{
			Root: defaultASRRoot(), Mode: "resident", GPULayers: 99, Encoder: "gpu", VAD: "gpu", Threads: 8,
		}, Doubao: DoubaoConfig{
			URL: "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async", ResourceID: "volc.seedasr.sauc.duration",
			KeyFile: filepath.Join(ConfigDir(), "doubao.key"), TwoPass: true, Hotwords: true,
		}},
		Postprocess: PostConfig{JoinWords: []string{}},
		Deliver:     DeliverConfig{HerdrApps: []string{"com.github.wez.wezterm"}},
		Store:       StoreConfig{Data: DataDir(), Labels: filepath.Join(dataHome(), "megavoice", "labels.jsonl")},
		Compare:     CompareConfig{Engines: []string{"funasr", "doubao"}, Addr: "127.0.0.1:7865"},
		Meeting: MeetingConfig{
			Data:    meetingDir(),
			Capture: MeetingCaptureConfig{Apps: []string{"com.electron.lark.helper", "us.zoom.xos"}, Mic: "default"},
			Process: MeetingProcessConfig{Diarizer: "none"},
			Align:   MeetingAlignConfig{FeishuArchive: feishuArchive()},
			Page:    MeetingPageConfig{},
			Upload:  MeetingUploadConfig{PartMiB: 16, RetentionDays: 30},
			Ingest:  MeetingIngestConfig{Command: []string{}, Sources: []string{"mac", "room", "phone", "file"}, MinDuration: Duration(2 * time.Minute), AssetsRemote: "origin"},
			Summary: MeetingSummaryConfig{Command: []string{}, Model: "claude-haiku-4-5", MaxChars: 40000},
			Drain:   MeetingDrainConfig{Notify: []string{}},
		},
		MicServer: MicServerConfig{Listen: ":7866", State: filepath.Join(stateHome(), "megavoice", "mic"), AdminGroup: "megavoice-mic"},
	}
}

// Duration is a TOML string with a unit ("400ms", "30s", "30m"); a bare
// number is refused, so 400 can never mean 400 ns.
type Duration time.Duration

func MS(n int) Duration { return Duration(time.Duration(n) * time.Millisecond) }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("duration %q: want a number with a unit, such as \"400ms\", \"30s\", \"30m\"", b)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d Duration) String() string {
	v := time.Duration(d)
	switch {
	case v != 0 && v%time.Minute == 0:
		return strconv.FormatInt(int64(v/time.Minute), 10) + "m"
	case v%time.Second == 0:
		return strconv.FormatInt(int64(v/time.Second), 10) + "s"
	case v%time.Millisecond == 0:
		return strconv.FormatInt(int64(v/time.Millisecond), 10) + "ms"
	}
	return v.String()
}

// LoadOpts are the global flags that choose the settings, and the checks
// only the caller can make.
type LoadOpts struct {
	Path  string                // --config: the file; "" resolves the default
	Sets  []string              // --set section.key=value, applied after the file
	Check func(Config) []string // more problems, each "key: what is wrong"
}

// Loaded is a resolved Config and where each value came from.
type Loaded struct {
	Config
	Path    string            // the file read; "" when none was
	Sources map[string]string // dotted key → "file" or "flag"; absent means default
}

func (l Loaded) SourceOf(key string) string {
	if s, ok := l.Sources[key]; ok {
		return s
	}
	return "default"
}

// Path is the file to read: --config, else MEGAVOICE_CONFIG, else
// config.toml beside hotwords.txt. explicit reports a named file, which
// must exist; the default one may be absent.
func Path(flag string) (path string, explicit bool) {
	if flag != "" {
		return flag, true
	}
	if p := os.Getenv("MEGAVOICE_CONFIG"); p != "" {
		return p, true
	}
	return filepath.Join(ConfigDir(), "config.toml"), false
}

// Load resolves the settings: defaults, then the file, then --set flags.
func Load(o LoadOpts) (Loaded, error) {
	for _, w := range legacyEnv() {
		log.Printf("config: %s", w)
	}
	l := Loaded{Config: Default(), Sources: map[string]string{}}
	path, explicit := Path(o.Path)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := l.decode(string(b), path, "file"); err != nil {
			return l, err
		}
		l.Path = path
	case errors.Is(err, fs.ErrNotExist) && !explicit:
	default:
		return l, fmt.Errorf("config: %w", err)
	}
	for _, s := range o.Sets {
		key, val, ok := strings.Cut(s, "=")
		if !ok {
			return l, fmt.Errorf("--set %q: want section.key=value", s)
		}
		doc := key + " = " + val
		if _, err := toml.Decode(doc, new(map[string]any)); err != nil {
			doc = key + " = " + strconv.Quote(val) // a bare word is a string
		}
		if err := l.decode(doc, "--set "+s, "flag"); err != nil {
			return l, err
		}
	}
	for _, p := range []*string{&l.Store.Data, &l.Store.Labels, &l.ASR.Doubao.KeyFile, &l.ASR.FunASR.Root, &l.ASR.FunASR.LLM, &l.Meeting.Data, &l.Meeting.Align.FeishuArchive, &l.Meeting.Ingest.Wiki, &l.Meeting.Page.TokenFile, &l.Meeting.Ingest.Repos, &l.Meeting.Ingest.Assets, &l.MicServer.State, &l.MicServer.Socket} {
		*p = expandHome(*p)
	}
	errs := l.validate()
	if o.Check != nil {
		errs = append(errs, o.Check(l.Config)...)
	}
	if len(errs) > 0 {
		slices.Sort(errs)
		from := "defaults and flags"
		if l.Path != "" {
			from = l.Path
		}
		return l, fmt.Errorf("%s: %s", from, strings.Join(errs, "; "))
	}
	return l, nil
}

// decode applies one TOML document over the config. A key the Config does
// not have is an error that names it.
func (l *Loaded) decode(doc, from, src string) error {
	md, err := toml.Decode(doc, &l.Config)
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return fmt.Errorf("%s: unknown key %s", from, strings.Join(keys, ", "))
	}
	for _, k := range md.Keys() {
		l.Sources[k.String()] = src
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var lowerWordRE = regexp.MustCompile(`^[a-z]+$`)

// validate reports every invalid value, each by its key.
func (c Config) validate() []string {
	var errs []string
	bad := func(key, format string, a ...any) { errs = append(errs, key+": "+fmt.Sprintf(format, a...)) }
	oneOf := func(key, v string, allowed ...string) {
		if !slices.Contains(allowed, v) {
			bad(key, "%q is not one of %s", v, strings.Join(allowed, ", "))
		}
	}
	if c.Tap.Window <= 0 {
		bad("tap.window", "must be above 0")
	}
	if c.Tap.Quiet < 0 {
		bad("tap.quiet", "must not be negative")
	}
	oneOf("capture.source", c.Capture.Source, CaptureSources...)
	switch c.Capture.Source {
	case "local":
		if c.Capture.Mic == "" {
			bad("capture.mic", "empty; \"default\" is the system's input")
		}
		if c.Capture.MicChannel < 0 {
			bad("capture.mic_channel", "must not be negative")
		}
		if c.Capture.BluetoothWarm < 0 {
			bad("capture.bluetooth_warm", "must not be negative")
		}
	case "ssh":
		if c.Capture.Host == "" {
			bad("capture.host", "empty")
		}
		if c.Capture.Device == "" {
			bad("capture.device", "empty")
		}
		if c.Capture.Channel < 1 || c.Capture.Channel > audio.Channels {
			bad("capture.channel", "%d is outside 1..%d", c.Capture.Channel, audio.Channels)
		}
	case "remote":
		if c.Capture.Remote == "" {
			bad("capture.remote", "empty; pair one with megavoice mic add")
		}
	}
	// An input's UID holds a capital, a digit or punctuation: a bare
	// lowercase word, or auto or off in another case, is a misspelt keyword.
	if b := c.Capture.Backup; b != audio.BackupAuto && b != "off" && (lowerWordRE.MatchString(b) || strings.EqualFold(b, audio.BackupAuto) || strings.EqualFold(b, "off")) {
		bad("capture.backup", "%q is not auto, off, \"\" or an input's UID (megavoice mics)", b)
	}
	for key, d := range map[string]Duration{
		"take.max_duration": c.Take.MaxDuration, "take.chunk_pause": c.Take.ChunkPause, "take.chunk_max": c.Take.ChunkMax,
	} {
		if d <= 0 {
			bad(key, "must be above 0")
		}
	}
	if c.Take.WholeMax < 0 {
		bad("take.whole_max", "must not be negative")
	}
	oneOf("asr.engine", c.ASR.Engine, Engines...)
	d := c.ASR.Doubao
	if u, err := url.Parse(d.URL); err != nil || u.Scheme != "wss" || !strings.HasPrefix(u.Path, "/api/v3/plan/sauc/") {
		bad("asr.doubao.url", "%q is not a plan ASR endpoint (wss://openspeech.bytedance.com/api/v3/plan/sauc/…): speech anywhere else is billed apart from the plan", d.URL)
	}
	if d.ResourceID == "" {
		bad("asr.doubao.resource_id", "empty")
	}
	if c.Uses("doubao") {
		if _, err := d.Key(); err != nil {
			bad("asr.doubao.key_file", "%v", err)
		}
	}
	for i, e := range c.Compare.Engines {
		oneOf("compare.engines", e, Engines...)
		if slices.Contains(c.Compare.Engines[:i], e) {
			bad("compare.engines", "%s twice", e)
		}
	}
	// an IP literal: the menu hands out the address bound, and a host name
	// such as localhost resolves to more addresses than the one bound
	if h, _, err := net.SplitHostPort(c.Compare.Addr); err != nil || net.ParseIP(h) == nil || !net.ParseIP(h).IsLoopback() {
		bad("compare.addr", "%q is not a loopback IP:port, such as 127.0.0.1:7865: the Takes page serves your recordings", c.Compare.Addr)
	}
	if c.ASR.FunASR.Root == "" {
		bad("asr.funasr.root", "empty")
	}
	if llm := c.ASR.FunASR.LLM; llm != "" {
		if st, err := os.Stat(llm); err != nil {
			bad("asr.funasr.llm", "%v", err)
		} else if !st.Mode().IsRegular() {
			bad("asr.funasr.llm", "%s is not a file", llm)
		}
	}
	oneOf("asr.funasr.mode", c.ASR.FunASR.Mode, "resident", "exec")
	if c.ASR.FunASR.GPULayers < 0 {
		bad("asr.funasr.gpu_layers", "must not be negative")
	}
	oneOf("asr.funasr.encoder", c.ASR.FunASR.Encoder, "gpu", "cpu")
	oneOf("asr.funasr.vad", c.ASR.FunASR.VAD, "gpu", "cpu")
	if c.ASR.FunASR.Threads < 1 {
		bad("asr.funasr.threads", "must be at least 1")
	}
	if c.ASR.FunASR.LLMThreads < 0 {
		bad("asr.funasr.llm_threads", "must not be negative")
	}
	if slices.Contains(c.Deliver.HerdrApps, "") {
		bad("deliver.herdr_apps", "empty bundle id")
	}
	if c.Store.Data == "" {
		bad("store.data", "empty")
	}
	if c.Store.Labels == "" {
		bad("store.labels", "empty")
	}

	m := c.Meeting
	if m.Data == "" {
		bad("meeting.data", "empty")
	}
	if slices.Contains(m.Capture.Apps, "") {
		bad("meeting.capture.apps", "empty bundle id")
	}
	if m.Capture.Mic == "" {
		bad("meeting.capture.mic", "empty; \"default\" is the system's input")
	}
	oneOf("meeting.process.diarizer", m.Process.Diarizer, "none")
	if m.Process.Speakers < 0 {
		bad("meeting.process.speakers", "must not be negative")
	}
	if m.Page.URL != "" {
		if u, err := url.Parse(m.Page.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			bad("meeting.page.url", "%q is not an http(s) URL", m.Page.URL)
		}
	} else if m.Page.Slug != "" {
		bad("meeting.page.url", "empty while slug is set: the ccc-pages service that holds the page")
	}
	if m.Page.Slug != "" && !slugRE.MatchString(m.Page.Slug) {
		bad("meeting.page.slug", "%q: want lowercase letters, digits and -", m.Page.Slug)
	}
	oneOf("meeting.page.identity", m.Page.Identity, "", "ucc")
	if m.Page.TokenFile != "" && m.Page.Identity != "" {
		bad("meeting.page", "token_file and identity both set; a host uses one")
	}
	if m.Upload.PartMiB < 5 || m.Upload.PartMiB > 95 {
		bad("meeting.upload.part_mib", "%d is outside 5..95 (R2's smallest part; the 100 MiB request cap)", m.Upload.PartMiB)
	}
	for _, src := range m.Ingest.Sources {
		oneOf("meeting.ingest.sources", src, "mac", "room", "phone", "file", "feishu")
		if src == "feishu" && m.Ingest.FeishuSince == "" {
			bad("meeting.ingest.feishu_since", "unset while sources has feishu: the cutoff keeps the backfill's minutes out")
		}
	}
	if _, err := m.Ingest.Since(); err != nil {
		bad("meeting.ingest.feishu_since", "%v", err)
	}
	if m.Ingest.MinDuration < 0 {
		bad("meeting.ingest.min_duration", "must not be negative")
	}
	if m.Ingest.PeopleResolve && !m.Ingest.Auto {
		bad("meeting.ingest.people_resolve", "on while auto is off: the drain resolves people only when it ingests")
	}
	if m.Ingest.Auto && len(m.Ingest.Command) == 0 {
		bad("meeting.ingest.command", "empty while auto is on")
	}
	if m.Ingest.Assets != "" && m.Ingest.AssetsRemote == "" {
		bad("meeting.ingest.assets_remote", "empty while assets is set")
	}
	if len(m.Summary.Command) > 0 && m.Summary.Model == "" {
		bad("meeting.summary.model", "empty while command is set")
	}
	if m.Summary.MaxChars < 1000 {
		bad("meeting.summary.max_chars", "%d is below 1000", m.Summary.MaxChars)
	}
	if m.Upload.RetentionDays < 0 {
		bad("meeting.upload.retention_days", "must not be negative")
	}

	ms := c.MicServer
	if _, port, err := net.SplitHostPort(ms.Listen); err != nil {
		bad("mic_server.listen", "%q is not host:port (\":7866\" is every interface)", ms.Listen)
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		bad("mic_server.listen", "%q: port %q is not 1..65535", ms.Listen, port)
	}
	if strings.ContainsAny(ms.Name, " \t\n/") {
		bad("mic_server.name", "%q has a space or /", ms.Name)
	}
	if !filepath.IsAbs(ms.State) {
		bad("mic_server.state", "%q is not an absolute path", ms.State)
	}
	if ms.Socket != "" && !filepath.IsAbs(ms.Socket) {
		bad("mic_server.socket", "%q is not an absolute path", ms.Socket)
	}
	if ms.AdminGroup == "" || strings.ContainsAny(ms.AdminGroup, " \t\n/:") {
		bad("mic_server.admin_group", "%q is not a group name", ms.AdminGroup)
	}
	return errs
}

// Uses reports whether this config runs engine: as asr.engine, or in
// compare mode.
func (c Config) Uses(engine string) bool {
	return c.ASR.Engine == engine || (c.Compare.On && slices.Contains(c.Compare.Engines, engine))
}

// Key reads the plan key from key_file: the value of its key_var line
// (KEY=value, export and quotes allowed), or the whole file without
// key_var. An error never quotes the file.
func (d DoubaoConfig) Key() (string, error) {
	b, err := os.ReadFile(d.KeyFile)
	if err != nil {
		return "", err
	}
	if d.KeyVar == "" {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("%s is empty", d.KeyFile)
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, val, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if !ok || strings.TrimSpace(name) != d.KeyVar {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if val != "" {
			return val, nil
		}
	}
	return "", fmt.Errorf("%s has no %s line with a value", d.KeyFile, d.KeyVar)
}

// legacyKeys maps the environment variables megavoice once read to the
// config keys that replace them.
var legacyKeys = map[string]string{
	"MEGAVOICE_TAP_KEY": "tap.key", "MEGAVOICE_TAP_MS": "tap.window", "MEGAVOICE_TAP_QUIET_MS": "tap.quiet",
	"MEGAVOICE_HOST": "capture.host", "MEGAVOICE_DEVICE": "capture.device", "MEGAVOICE_CHANNEL": "capture.channel",
	"MEGAVOICE_MAX_MIN": "take.max_duration", "MEGAVOICE_CHUNK_PAUSE_MS": "take.chunk_pause", "MEGAVOICE_CHUNK_MAX_S": "take.chunk_max",
	"MEGAVOICE_MEGA_ASR": "asr.funasr.root", "MEGAVOICE_ASR": "asr.funasr.mode", "MEGAVOICE_ASR_GPU": "asr.funasr.gpu_layers",
	"MEGAVOICE_ASR_ENC": "asr.funasr.encoder", "MEGAVOICE_ASR_VAD": "asr.funasr.vad", "MEGAVOICE_ASR_THREADS": "asr.funasr.threads",
	"MEGAVOICE_HERDR_APPS": "deliver.herdr_apps", "MEGAVOICE_DATA": "store.data",
}

// legacyEnv warns about each of those variables that is still set: it no
// longer changes anything.
func legacyEnv() []string {
	var out []string
	for name, key := range legacyKeys {
		if os.Getenv(name) != "" {
			out = append(out, fmt.Sprintf("%s is set and ignored; set %s in config.toml", name, key))
		}
	}
	slices.Sort(out)
	return out
}

// WriteEffective prints the config as TOML, each value followed by its
// source; the output is itself a valid config file. tables names the
// top-level tables to print; none prints them all.
func (l Loaded) WriteEffective(w io.Writer, prog string, tables ...string) {
	file := l.Path
	if file == "" {
		file = "none, " + filepath.Join(ConfigDir(), "config.toml") + " is absent"
	}
	fmt.Fprintf(w, "# %s effective config; file: %s\n# each value's source: default | file | flag\n", prog, file)
	v := reflect.ValueOf(l.Config)
	for i := range v.NumField() {
		name := v.Type().Field(i).Tag.Get("toml")
		if len(tables) == 0 || slices.Contains(tables, name) {
			fmt.Fprintf(w, "\n[%s]\n", name)
			writeTable(w, v.Field(i), []string{name}, l.SourceOf)
		}
	}
}

func writeTable(w io.Writer, v reflect.Value, path []string, source func(string) string) {
	t := v.Type()
	var tables []int
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Struct {
			tables = append(tables, i)
			continue
		}
		name := f.Tag.Get("toml")
		key := strings.Join(append(slices.Clone(path), name), ".")
		fmt.Fprintf(w, "%-44s # %s\n", name+" = "+tomlValue(v.Field(i).Interface()), source(key))
	}
	for _, i := range tables {
		p := append(slices.Clone(path), t.Field(i).Tag.Get("toml"))
		fmt.Fprintf(w, "\n[%s]\n", strings.Join(p, "."))
		writeTable(w, v.Field(i), p, source)
	}
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case Duration:
		return strconv.Quote(x.String())
	case string:
		return strconv.Quote(x)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case []string:
		q := make([]string, len(x))
		for i, s := range x {
			q[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	panic(fmt.Sprintf("config: no TOML form for %T", v))
}

func dataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func stateHome() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// DataDir is where megavoice keeps its takes by default.
func DataDir() string { return filepath.Join(dataHome(), "megavoice", "utterances") }

// FinetuneDir holds fine-tune exports, <run>/<model>.gguf, the files
// asr.funasr.llm names; the menu offers each.
func FinetuneDir() string { return filepath.Join(dataHome(), "mega-asr", "finetune") }

func meetingDir() string { return filepath.Join(dataHome(), "megameet") }

// feishuArchive is where scripts/feishu-minutes/fetch.py keeps the minutes.
func feishuArchive() string {
	return filepath.Join(dataHome(), "mega-asr", "corpora", "feishu-minutes")
}

// ConfigDir holds config.toml and the user-editable hotwords.txt and
// corrections.tsv.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "megavoice")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "megavoice")
}

func HotwordsPath() string    { return filepath.Join(ConfigDir(), "hotwords.txt") }
func CorrectionsPath() string { return filepath.Join(ConfigDir(), "corrections.tsv") }

//go:embed defaults
var defaults embed.FS

// Template is the commented default config that `config init` writes.
func Template() []byte {
	b, _ := defaults.ReadFile("defaults/config.toml")
	return b
}

// Seed writes the default hotwords and corrections files when absent; an
// existing file is the user's and is never touched.
func Seed() {
	for _, name := range []string{"hotwords.txt", "corrections.tsv"} {
		path := filepath.Join(ConfigDir(), name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		b, _ := defaults.ReadFile("defaults/" + name)
		err := os.MkdirAll(ConfigDir(), 0o755)
		if err == nil {
			err = os.WriteFile(path, b, 0o644)
		}
		if err != nil {
			log.Printf("config: seed %s: %v", path, err)
		}
	}
}
