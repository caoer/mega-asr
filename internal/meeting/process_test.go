package meeting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/pages"
	"github.com/caoer/mega-asr/internal/session"
)

// everySecs cuts a stream every n seconds.
type everySecs struct{ n, pushed int }

func (e *everySecs) Push(s []int16) []audio.Cut {
	var cuts []audio.Cut
	step := e.n * audio.Rate
	for k := (e.pushed/step + 1) * step; k <= e.pushed+len(s); k += step {
		cuts = append(cuts, audio.Cut{End: k})
	}
	e.pushed += len(s)
	return cuts
}

// toneASR answers each clip by its loudest sample: a fixture speaks in
// tones, one amplitude per line of text; silence is no speech. A clip
// longer than loopOver samples, when set, loops instead.
type toneASR struct {
	mu       sync.Mutex
	texts    map[int16]string
	loopOver int
	clips    []int // each clip's length in samples, in call order
}

func (a *toneASR) Transcribe(_ context.Context, wav string, _ bool) (string, error) {
	s, err := audio.ReadWAV(wav)
	if err != nil {
		return "", err
	}
	var peak int16
	for _, v := range s {
		peak = max(peak, v)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clips = append(a.clips, len(s))
	if a.loopOver > 0 && len(s) > a.loopOver {
		return "滴答滴答滴答滴答滴答滴答", nil
	}
	return a.texts[peak], nil
}

// track10s is 10 s of silence with a tone of amplitude amp over each
// [from, to) second span given.
func track10s(t *testing.T, path string, spans ...[3]int) {
	t.Helper()
	s := make([]int16, 10*audio.Rate)
	for _, sp := range spans {
		for i := sp[0] * audio.Rate; i < sp[1]*audio.Rate; i++ {
			s[i] = int16(sp[2])
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := audio.SaveWAV(path, s); err != nil {
		t.Fatal(err)
	}
}

func needFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not on PATH")
	}
}

// pulled lays out a pulled directory for rec with WAV tracks.
func pulled(t *testing.T, r Record) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "rec.json"), r); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A mac recording with nobody named: each track is its speaker. A mic
// piece whose text is mostly the remote text within 2 s is echo and goes; a
// mic line sharing a word or two with it stays, as does one out of its
// reach. Silent pieces are sent and make no segment.
func TestProcessMacEchoGate(t *testing.T) {
	needFFmpeg(t)
	r := Record{ID: "20200117-203000-mac-host-a", Source: "mac", Files: []File{{Role: "mic", Codec: "wav"}, {Role: "remote", Codec: "wav"}}}
	dir := pulled(t, r)
	track10s(t, filepath.Join(dir, "tracks", "remote.wav"), [3]int{3, 4, 700})
	track10s(t, filepath.Join(dir, "tracks", "mic.wav"), [3]int{1, 2, 900}, [3]int{3, 4, 150}, [3]int{8, 9, 400})
	a := &toneASR{texts: map[int16]string{
		700: "充电底座的指示灯一直闪。",
		150: "充电底座的指示灯一直闪", // the remote line, played into the mic
		900: "指示灯是什么颜色",    // three runes of eight in common: kept
		400: "周末再议",        // no remote text within 2 s
	}}
	p := &Processor{ASR: a, Chunker: func() session.Chunker { return &everySecs{n: 1} }, Engine: "e1", Root: "/engine"}
	o, err := p.Process(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if o.Chunks != 20 || o.AudioS != 20 || o.Echoes != 1 || o.Failed != 0 {
		t.Fatalf("outcome %+v", o)
	}
	var segs []Segment
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "segments.json")), &segs); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range segs {
		got = append(got, fmt.Sprintf("%s %s %g-%g %s", s.Track, s.Speaker, s.StartS, s.EndS, s.Text))
		if s.Engine != "e1" || s.Root != "/engine" || s.Raw != s.Text {
			t.Errorf("segment %+v", s)
		}
	}
	if want := "mic mic 1-2 指示灯是什么颜色|remote remote 3-4 充电底座的指示灯一直闪。|mic mic 8-9 周末再议"; strings.Join(got, "|") != want {
		t.Fatalf("segments %q, want %q", strings.Join(got, "|"), want)
	}
	if md := string(mustRead(t, filepath.Join(dir, "transcript.md"))); md != "00:00:01 mic: 指示灯是什么颜色\n00:00:03 remote: 充电底座的指示灯一直闪。\n00:00:08 mic: 周末再议\n" {
		t.Fatalf("transcript.md:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(dir, "work")); !os.IsNotExist(err) {
		t.Fatalf("work dir left behind: %v", err)
	}
}

// A track's speaker: the record's speaker with that role, the recorder's
// owner for the mic, the first named speaker for a track that holds
// everyone, else the role.
func TestTrackSpeaker(t *testing.T) {
	named := Record{Owner: "Alice Example", Speakers: []Speaker{{Role: "remote"}, {Name: "Bob Example", Role: "beam"}}}
	for _, c := range []struct {
		rec        Record
		role, want string
	}{
		{named, "media", "Bob Example"},
		{Record{}, "beam", "Speaker 1"},
		{named, "mic", "Alice Example"},
		{Record{Speakers: []Speaker{{Name: "Bob Example", Role: "mic"}}, Owner: "Alice Example"}, "mic", "Bob Example"},
		{Record{}, "mic", "mic"},
		{named, "remote", "remote"},
		{named, "beam", "Bob Example"},
	} {
		if got := trackSpeaker(c.rec, c.role); got != c.want {
			t.Errorf("trackSpeaker(%+v, %s) = %q, want %q", c.rec, c.role, got, c.want)
		}
	}
}

// cancelASR cancels the run as its nth call returns: the stop arrives while
// that piece is decoding.
type cancelASR struct {
	n, calls int
	cancel   context.CancelFunc
}

func (a *cancelASR) Transcribe(context.Context, string, bool) (string, error) {
	if a.calls++; a.calls == a.n {
		a.cancel()
	}
	return "一句话", nil
}

// A stop mid-record: Process returns the cancellation once the piece in
// flight is done, sends no later piece to the ASR and writes no output.
func TestProcessStopsWithinOnePiece(t *testing.T) {
	needFFmpeg(t)
	r := Record{ID: "20200311-150000-file-host-a", Source: "file", Files: []File{{Role: "media", Codec: "wav"}}}
	dir := pulled(t, r)
	track10s(t, filepath.Join(dir, "tracks", "media.wav"), [3]int{0, 10, 500})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &cancelASR{n: 2, cancel: cancel}
	p := &Processor{ASR: a, Chunker: func() session.Chunker { return &everySecs{n: 1} }}
	_, err := p.Process(ctx, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if a.calls != 2 {
		t.Fatalf("%d ASR calls of 10 pieces, want 2", a.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "segments.json")); !os.IsNotExist(err) {
		t.Fatalf("segments.json written: %v", err)
	}
}

// A piece whose text loops is decoded again as two halves, cut in its
// quiet middle; the halves' text replaces the loop.
func TestProcessRedecodesALoop(t *testing.T) {
	needFFmpeg(t)
	r := Record{ID: "20200402-084517-feishu-host-a", Source: "file", Files: []File{{Role: "media", Codec: "wav"}}}
	dir := pulled(t, r)
	// one 10 s piece: 4 s of speech, a quiet second, 4 s of other speech
	track10s(t, filepath.Join(dir, "tracks", "media.wav"), [3]int{0, 10, 500}, [3]int{4, 5, 1}, [3]int{5, 9, 700})
	a := &toneASR{texts: map[int16]string{500: "起头", 700: "收尾"}, loopOver: 8 * audio.Rate}
	half := 0
	p := &Processor{ASR: a, Chunker: func() session.Chunker { return &everySecs{n: 10} }}
	o, err := p.Process(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range a.clips[1:] {
		half += n
	}
	if o.Redecoded != 1 || o.Loops != 0 || len(a.clips) != 3 || half != 10*audio.Rate {
		t.Fatalf("outcome %+v; clips %v", o, a.clips)
	}
	if cut := a.clips[1]; cut < 4*audio.Rate || cut > 5*audio.Rate {
		t.Fatalf("cut at sample %d, not in the quiet second", cut)
	}
	if o.Segments[0].Raw != "起头收尾" || o.Segments[0].Speaker != "Speaker 1" {
		t.Fatalf("segment %+v", o.Segments[0])
	}
}

// A Feishu minute whose media export was denied has nothing to transcribe:
// its results are empty, not a failure.
func TestProcessTranscriptOnlyMinute(t *testing.T) {
	dir := pulled(t, Record{ID: "20200520-100000-feishu-host-a", Source: "feishu", Files: []File{{Role: "feishu-transcript", Codec: "json"}}})
	os.WriteFile(filepath.Join(dir, "feishu-transcript.json"), []byte(`{"segments":[{"speaker":"S1","start_s":3.5,"text":"灯还亮着"}]}`), 0o600)
	o, err := (&Processor{}).Process(context.Background(), dir)
	if err != nil || len(o.Segments) != 0 || string(mustRead(t, filepath.Join(dir, "segments.json"))) != "[]\n" {
		t.Fatalf("outcome %+v, %v", o, err)
	}
}

// Media too short to hold speech fails with the
// reason instead of ending with empty results.
func TestProcessTooShortMediaFails(t *testing.T) {
	needFFmpeg(t)
	r := Record{ID: "20200613-190212-feishu-host-a", Source: "feishu", Files: []File{{Role: "media", Codec: "wav"}}}
	dir := pulled(t, r)
	os.MkdirAll(filepath.Join(dir, "tracks"), 0o700)
	if err := audio.SaveWAV(filepath.Join(dir, "tracks", "media.wav"), make([]int16, audio.Rate/5)); err != nil {
		t.Fatal(err)
	}
	_, err := (&Processor{ASR: &toneASR{}}).Process(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "0.2 s of audio in 1 track(s), too short to hold speech") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "segments.json")); !os.IsNotExist(err) {
		t.Fatalf("segments.json written: %v", err)
	}
}

// Repeats counts back-to-back copies of a run of up to 16 tokens: a Han
// or kana character, or a word of letters, digits and apostrophes, without
// case; a run of 17 is out of reach.
func TestRepeats(t *testing.T) {
	sixteen := "甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳"
	for _, c := range []struct {
		text string
		want int
	}{
		{"第3版第3版第3版第3版第3版", 5},
		{sixteen + sixteen, 2},
		{"一", 1},
		{"Tick tick TICK, tick!", 4},
		{sixteen + "午" + sixteen + "午", 1},
		{"don't don't don't", 3},
		{"", 0},
		{"ありありありあり", 4},
	} {
		if got := Repeats(c.text); got != c.want {
			t.Errorf("Repeats(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// Each segment takes the speaker of the turn it overlaps most, a turn
// running until the next one starts; a segment before every turn takes the
// first; with no turns, speakers stay.
func TestLabelByTurns(t *testing.T) {
	turns := []Turn{{Speaker: "A", StartS: 5}, {Speaker: "B", StartS: 20}, {Speaker: "C", StartS: 21}}
	segs := []Segment{{StartS: 18, EndS: 26}, {StartS: 0, EndS: 3}, {StartS: 19.5, EndS: 20.8}}
	LabelByTurns(segs, turns)
	var got []string
	for _, s := range segs {
		got = append(got, s.Speaker)
	}
	if strings.Join(got, ",") != "C,A,B" {
		t.Fatalf("speakers %v", got)
	}
	LabelByTurns(segs, nil)
	if segs[0].Speaker != "C" {
		t.Fatalf("no turns relabelled: %+v", segs)
	}
}

// pageFake is the page's records and files.
type pageFake struct {
	objs    map[string]pages.Object
	files   map[string][]byte
	deleted []string
	n       int
}

func (f *pageFake) Record(_ context.Context, key string) (pages.Object, error) {
	o, ok := f.objs[key]
	if !ok {
		return o, &pages.Error{Status: 404, Code: "no_such_object"}
	}
	return o, nil
}

func (f *pageFake) CAS(_ context.Context, key, schema string, data any, version int) (int, error) {
	if f.objs[key].Version != version {
		return 0, &pages.Error{Status: 409, Code: "version_conflict"}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	f.objs[key] = pages.Object{Key: key, Value: pages.Value{Schema: schema, Data: b}, Version: version + 1}
	return version + 1, nil
}

func (f *pageFake) Get(_ context.Context, id string, _, _ int64) (io.ReadCloser, error) {
	b, ok := f.files[id]
	if !ok {
		return nil, &pages.Error{Status: 404, Code: "no_such_file"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *pageFake) Upload(_ context.Context, path string, meta map[string]any) (string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	f.n++
	id := fmt.Sprintf("f%d-%s-%s", f.n, meta["rec"], meta["role"])
	f.files[id] = b
	return id, sha(b), nil
}

func (f *pageFake) DeleteFile(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	delete(f.files, id)
	return nil
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (f *pageFake) value(t *testing.T, key string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.objs[key].Value.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Pull lays out the contract directory, checks every digest, and marks the
// files verified; a second pull fetches nothing; a digest that differs is
// sha_mismatch; process's results replace their earlier files by role and
// leave the claim alone.
func TestPullAndPublish(t *testing.T) {
	media, transcript := []byte("fake media bytes"), []byte(`{"segments":[{"speaker":"S1","start_s":3.5,"text":"灯还亮着"}]}`)
	pg := &pageFake{objs: map[string]pages.Object{}, files: map[string][]byte{"fm": media, "ft": transcript, "old-seg": []byte("[]")}}
	ctx := context.Background()
	id := "20200402-084517-feishu-host-a"
	r := map[string]any{"id": id, "source": "feishu", "state": "processing", "claim": map[string]any{"by": "host-b"},
		"feishu": map[string]any{"token": "obcnexampletokenaaaaaaaa"},
		"files": []File{{Role: "media", File: "fm", Bytes: int64(len(media)), SHA256: sha(media), Codec: "mp4"},
			{Role: "feishu-transcript", File: "ft", Bytes: int64(len(transcript)), SHA256: sha(transcript), Codec: "json"},
			{Role: "segments", File: "old-seg", SHA256: sha([]byte("[]")), Codec: "json"}}}
	if _, err := pg.CAS(ctx, "rec."+id, Schema, r, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "pull", id)
	got, err := Pull(ctx, pg, id, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Files[0].Verified || !got.Files[1].Verified || got.Files[2].Verified || pg.objs["rec."+id].Version != 2 {
		t.Fatalf("verified: %+v, v%d", got.Files, pg.objs["rec."+id].Version)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tracks", "media.mp4")); !bytes.Equal(b, media) {
		t.Fatalf("tracks/media.mp4: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "feishu-transcript.json")); !bytes.Equal(b, transcript) {
		t.Fatalf("feishu-transcript.json: %q", b)
	}
	var m Meta
	if _, err := toml.DecodeFile(filepath.Join(dir, "meta.toml"), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != id || m.Source != "feishu" || len(m.Tracks) != 2 || m.Tracks[0].File != "tracks/media.mp4" || m.Tracks[0].SHA256 != sha(media) || m.Feishu.Token != "obcnexampletokenaaaaaaaa" {
		t.Fatalf("meta.toml %+v", m)
	}
	if rp, err := ReadPulled(dir); err != nil || rp.ID != id || rp.State != "processing" {
		t.Fatalf("rec.json %+v %v", rp, err)
	}

	pg.files["fm"] = []byte("gone") // a second pull fetches nothing: the file on disk has the digest
	if _, err := Pull(ctx, pg, id, dir); err != nil || pg.objs["rec."+id].Version != 2 {
		t.Fatalf("second pull: %v, v%d", err, pg.objs["rec."+id].Version)
	}
	os.Remove(filepath.Join(dir, "tracks", "media.mp4"))
	var pe *PullError
	if _, err := Pull(ctx, pg, id, dir); !asPullError(err, &pe) || pe.Code != "sha_mismatch" || pe.Role != "media" {
		t.Fatalf("changed bytes: %v", err)
	}

	if err := WriteSegments(filepath.Join(dir, "segments.json"), []Segment{{Track: "media", Text: "灯还亮着"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteTranscript(filepath.Join(dir, "transcript.md"), []Segment{{Speaker: "S1", Text: "灯还亮着"}}); err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, pg, id, dir, Outcome{AudioS: 100, WallS: 17, Loops: 0}, Engine{Root: "/nix/store/x-funasr-root"}); err != nil {
		t.Fatal(err)
	}
	v := pg.value(t, "rec."+id)
	fs := rawFiles(v)
	roles := []string{}
	for _, f := range fs {
		roles = append(roles, f.Role)
	}
	seg, tr := fs[2], fs[3]
	if strings.Join(roles, ",") != "media,feishu-transcript,segments,transcript" || seg.File == "old-seg" || tr.Codec != "md" ||
		!bytes.Equal(pg.files[seg.File], mustRead(t, filepath.Join(dir, "segments.json"))) {
		t.Fatalf("files %+v", fs)
	}
	if len(pg.deleted) != 1 || pg.deleted[0] != "old-seg" {
		t.Fatalf("deleted %v", pg.deleted)
	}
	sc := v["scores"].(map[string]any)
	eng := v["engine"].(map[string]any)
	if sc["rtf"] != 0.17 || sc["loops"] != 0.0 || eng["root"] != "/nix/store/x-funasr-root" || v["state"] != "processing" || v["claim"].(map[string]any)["by"] != "host-b" {
		t.Fatalf("record %v", v)
	}
}

func asPullError(err error, pe **PullError) bool {
	e, ok := err.(*PullError)
	*pe = e
	return ok
}

func mustRead(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A recording with no speech publishes its empty segments list and no
// transcript file: the page refuses zero-byte uploads.
func TestPublishNoSpeech(t *testing.T) {
	pg := &pageFake{objs: map[string]pages.Object{}, files: map[string][]byte{"old-tr": []byte("x")}}
	ctx := context.Background()
	id := "20200704-120000-mac-host-a"
	r := map[string]any{"id": id, "state": "processing", "files": []File{{Role: "transcript", File: "old-tr", Codec: "md"}}}
	if _, err := pg.CAS(ctx, "rec."+id, Schema, r, 0); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := WriteSegments(filepath.Join(dir, "segments.json"), nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteTranscript(filepath.Join(dir, "transcript.md"), nil); err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, pg, id, dir, Outcome{AudioS: 120, WallS: 1}, Engine{Root: "r"}); err != nil {
		t.Fatal(err)
	}
	fs := rawFiles(pg.value(t, "rec."+id))
	if len(fs) != 1 || fs[0].Role != "segments" || len(pg.deleted) != 1 || pg.deleted[0] != "old-tr" {
		t.Fatalf("files %+v deleted %v", fs, pg.deleted)
	}
}
