package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
)

func tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mm") // short: a unix socket path has ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// held is a Source that hands the recorder its samples as fast as it takes
// them, then stays open until Stop, as a live device does: how fast the host
// runs decides when audio arrives, never how much arrives nor when a take
// ends. Stop closes the stream once every sample is delivered.
type held struct {
	s    []int16
	stop chan struct{}
	once sync.Once
}

func newHeld(s []int16) *held { return &held{s: s, stop: make(chan struct{})} }

func (h *held) Start(context.Context) (<-chan []int16, error) {
	ch := make(chan []int16, 256)
	go func() {
		defer close(ch)
		for i := 0; i < len(h.s); i += audio.Block {
			ch <- h.s[i:min(i+audio.Block, len(h.s))]
		}
		<-h.stop
	}()
	return ch, nil
}

func (h *held) Stop()      { h.once.Do(func() { close(h.stop) }) }
func (h *held) Err() error { return nil }

func constant(v int16, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// heldRecorder records remote and mic from held sources.
func heldRecorder(dir string, remote, mic []int16, info func() audio.TapInfo) *recorder {
	return &recorder{
		dir:  filepath.Join(dir, "recordings"),
		host: "testhost",
		sources: func(apps []string) Sources {
			return Sources{Remote: newHeld(remote), Mic: newHeld(mic), Info: info}
		},
		now: time.Now,
	}
}

// patience outlasts go test's -timeout: a test here waits on audio and
// answers, so a slow host fails it only at that timeout.
const patience = time.Hour

func request(t *testing.T, sock, cmd string, args any, v any) {
	t.Helper()
	req := ctl.Request{Cmd: cmd}
	if args != nil {
		req.Args, _ = json.Marshal(args)
	}
	resp, err := ctl.Call(sock, req, patience)
	if err != nil || !resp.OK {
		t.Fatalf("%s: %v %s", cmd, err, resp.Error)
	}
	if v != nil {
		if err := json.Unmarshal(resp.Data, v); err != nil {
			t.Fatal(err)
		}
	}
}

// start and stop over the socket leave two tracks and a meta.toml that
// describes them.
func TestStartStopOverSocket(t *testing.T) {
	dir := tempDir(t)
	r := heldRecorder(dir, constant(1000, 5*audio.Rate), constant(-1000, 5*audio.Rate), nil)
	sock := filepath.Join(dir, "s.sock")
	ln, err := ctl.Listen(sock, handler(r, []string{"com.example.meeting"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var started Meta
	request(t, sock, "start", StartArgs{Title: "weekly", Speakers: 2}, &started)
	if started.ID != recID(started.Started, "testhost") || started.Apps[0] != "com.example.meeting" {
		t.Fatalf("started %+v", started)
	}
	var st Status
	request(t, sock, "status", nil, &st)
	if !st.Recording || st.Current.ID != started.ID {
		t.Fatalf("status %+v", st)
	}
	var stopped struct {
		Meta
		Dir string `json:"dir"`
	}
	request(t, sock, "stop", nil, &stopped)

	m, err := readMeta(stopped.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Stopped.IsZero() || m.Title != "weekly" || m.Speakers != 2 || m.Error != "" || m.DurationS != 5 {
		t.Fatalf("meta %+v", m)
	}
	for i, tr := range m.Tracks {
		s, err := audio.ReadWAV(filepath.Join(stopped.Dir, tr.File))
		if err != nil {
			t.Fatal(err)
		}
		if tr.Name != trackNames[i] || len(s) != tr.Samples || len(s) != 5*audio.Rate || s[0] != [2]int16{1000, -1000}[i] {
			t.Fatalf("track %+v: %d samples, first %v", tr, len(s), s[:min(1, len(s))])
		}
	}
	request(t, sock, "status", nil, &st)
	if st.Recording || st.Last == nil || st.Last.ID != m.ID {
		t.Fatalf("status after stop %+v", st)
	}
}

// A recorder killed mid-take leaves WAVs whose headers name no samples and
// a meta.toml with no stop; repair makes both whole.
func TestRepairInterruptedRecording(t *testing.T) {
	root := tempDir(t)
	start := time.Date(2020, 8, 15, 10, 15, 0, 0, time.Local)
	m := Meta{ID: recID(start, "host-a"), Source: "mac", Host: "host-a", Started: start}
	dir := filepath.Join(root, m.ID)
	if err := os.MkdirAll(filepath.Join(dir, "tracks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range trackNames {
		f, err := os.Create(filepath.Join(dir, "tracks", name+".wav"))
		if err != nil {
			t.Fatal(err)
		}
		audio.WriteWAV(f, nil) // the header as a take begins it
		binary.Write(f, binary.LittleEndian, make([]int16, 2*audio.Rate))
		f.Close()
		m.Tracks = append(m.Tracks, Track{Name: name, Role: name, File: filepath.Join("tracks", name+".wav")})
	}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}

	done, err := repair(root)
	if err != nil || len(done) != 1 || done[0] != dir {
		t.Fatalf("repair: %v %v", done, err)
	}
	got, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Interrupted || got.DurationS != 2 || !got.Stopped.Equal(start.Add(2*time.Second)) {
		t.Fatalf("repaired meta %+v", got)
	}
	for _, tr := range got.Tracks {
		s, err := audio.ReadWAV(filepath.Join(dir, tr.File))
		if err != nil || len(s) != 2*audio.Rate || tr.Samples != len(s) {
			t.Fatalf("%s: %d samples (meta %d), %v", tr.Name, len(s), tr.Samples, err)
		}
	}
}

// A mic delivering digital silence (a muted or disconnected input)
// makes status and start's check warn within the first seconds; a quiet
// room's mic (±10 LSB hiss, -70 dBFS) does not.
func TestMicSilenceWarning(t *testing.T) {
	for _, c := range []struct {
		name string
		mic  func(k int) int16
		warn bool
	}{
		{"zeros", func(int) int16 { return 0 }, true},
		{"quiet room", func(k int) int16 { return int16(10 * (1 - 2*(k%2))) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := tempDir(t)
			mic := make([]int16, 10*audio.Rate)
			for k := range mic {
				mic[k] = c.mic(k)
			}
			r := heldRecorder(dir, constant(0, 10*audio.Rate), mic,
				func() audio.TapInfo { return audio.TapInfo{Mic: "Test Input"} })
			if _, err := r.start(StartArgs{}); err != nil {
				t.Fatal(err)
			}
			defer r.stop()
			got := checkMic(func() (Status, error) { return r.status(), nil }, 10*time.Millisecond, patience)
			st := r.status()
			m := st.Mic
			if c.warn != strings.HasPrefix(got, "WARNING: ") || !strings.Contains(got, `"Test Input"`) ||
				m == nil || m.Device != "Test Input" || c.warn != m.Silent || c.warn != (m.Warning != "") ||
				c.warn != !m.Since.IsZero() || c.warn != (m.LevelDBFS == -120) {
				t.Fatalf("check %q, status mic %+v; want silent %v", got, m, c.warn)
			}
		})
	}
}
