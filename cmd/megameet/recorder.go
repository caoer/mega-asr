package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// Sources is one recording's capture: the two tracks and what the device
// reports about them. The Mac's is a process tap plus the mic; tests replay
// files.
type Sources struct {
	Remote, Mic audio.Source
	Info        func() audio.TapInfo
}

// recorder runs at most one recording at a time, each under dir/<id>/.
type recorder struct {
	dir     string // recordings root
	host    string
	sources func(apps []string) Sources
	now     func() time.Time

	onStop func(dir string, m Meta) // after the final meta.toml; may be nil

	mu      sync.Mutex
	cur     *recording
	last    *Meta             // the last finished recording
	uploads map[string]string // id → the upload's latest progress
}

func (r *recorder) setUpload(id, s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.uploads == nil {
		r.uploads = map[string]string{}
	}
	r.uploads[id] = s
}

type recording struct {
	meta   Meta
	dir    string
	src    Sources
	takes  [2]*store.Take // remote, mic
	chans  [2]<-chan []int16
	wg     sync.WaitGroup
	ending sync.Once
	mic    audio.Monitor // the mic's level and digital silence
}

// StartArgs are `start`'s arguments.
type StartArgs struct {
	Apps     []string `json:"apps,omitempty"`
	Title    string   `json:"title,omitempty"`
	Speakers int      `json:"speakers,omitempty"`
	Test     bool     `json:"test,omitempty"`
}

var trackNames = [2]string{"remote", "mic"}

func recID(t time.Time, host string) string {
	return t.Format("20060102-150405") + "-mac-" + host
}

// start creates the recording's directory, both tracks and meta.toml, and
// begins writing the tracks.
func (r *recorder) start(a StartArgs) (Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return Meta{}, fmt.Errorf("already recording %s", r.cur.meta.ID)
	}
	now := r.now()
	m := Meta{ID: recID(now, r.host), Source: "mac", Host: r.host, Title: a.Title, Speakers: a.Speakers, Test: a.Test, Apps: a.Apps, Started: now}
	rec := &recording{meta: m, dir: filepath.Join(r.dir, m.ID)}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return Meta{}, err
	}
	if err := os.Mkdir(rec.dir, 0o755); err != nil {
		return Meta{}, fmt.Errorf("%w: one recording per second", err)
	}
	if err := rec.open(r.sources(a.Apps)); err != nil {
		os.RemoveAll(rec.dir) // nothing was recorded; the directory is this call's
		return Meta{}, err
	}
	r.cur = rec
	for i, ch := range rec.chans {
		rec.wg.Add(1)
		go r.pump(rec, i, ch)
	}
	log.Printf("start %s apps=%v title=%q", m.ID, a.Apps, a.Title)
	return rec.meta, nil
}

func (rec *recording) open(src Sources) error {
	rec.src = src
	st := store.Store{Dir: filepath.Join(rec.dir, "tracks")}
	for i, name := range trackNames {
		t, err := st.Create(name)
		if err != nil {
			rec.closeTakes()
			return err
		}
		rec.takes[i] = t
		rec.meta.Tracks = append(rec.meta.Tracks, Track{Name: name, Role: name, File: filepath.Join("tracks", name+".wav")})
	}
	if err := writeMeta(rec.dir, rec.meta); err != nil {
		rec.closeTakes()
		return err
	}
	for i, s := range []audio.Source{src.Remote, src.Mic} {
		ch, err := s.Start(context.Background())
		if err != nil {
			src.Remote.Stop()
			src.Mic.Stop()
			rec.closeTakes()
			return err
		}
		rec.chans[i] = ch
	}
	return nil
}

func (rec *recording) closeTakes() error {
	var errs []error
	for _, t := range rec.takes {
		if t != nil {
			errs = append(errs, t.Close())
		}
	}
	return errors.Join(errs...)
}

// pump writes one track until its source closes. A source that closes
// before stop ends the recording with the source's error.
func (r *recorder) pump(rec *recording, i int, ch <-chan []int16) {
	defer rec.wg.Done()
	var werr error
	warned := false
	for s := range ch {
		if werr == nil {
			werr = rec.takes[i].Write(s)
		}
		if i == 1 {
			rec.mic.Add(s)
			if dead := rec.mic.Silent() >= audio.DeadAfter; dead != warned {
				warned = dead
				if dead {
					log.Printf("%s: %s", rec.meta.ID, micWarning(rec.micName(), audio.DeadAfter))
				} else {
					log.Printf("%s: mic %q is live again", rec.meta.ID, rec.micName())
				}
			}
		}
	}
	src := [2]audio.Source{rec.src.Remote, rec.src.Mic}[i]
	go r.end(rec, errors.Join(werr, src.Err()))
}

// stop ends the current recording and returns its final meta.
func (r *recorder) stop() (Meta, error) {
	r.mu.Lock()
	rec := r.cur
	r.mu.Unlock()
	if rec == nil {
		return Meta{}, errors.New("not recording")
	}
	return r.end(rec, nil), nil
}

// end stops both sources, waits for both tracks, closes them and writes the
// final meta.toml. The first call does it with its cause; a later one waits
// for it and returns the same meta.
func (r *recorder) end(rec *recording, cause error) Meta {
	rec.ending.Do(func() {
		rec.src.Remote.Stop()
		rec.src.Mic.Stop()
		rec.wg.Wait()
		err := errors.Join(cause, rec.closeTakes())
		m := &rec.meta
		m.Stopped = r.now()
		for i, t := range rec.takes {
			m.Tracks[i].Samples = t.Samples()
			m.Tracks[i].Bytes = int64(44 + 2*t.Samples())
		}
		m.DurationS = float64(m.Tracks[1].Samples) / audio.Rate
		if err != nil {
			m.Error = err.Error()
		}
		if rec.src.Info != nil {
			i := rec.src.Info()
			m.Capture = Capture{Mic: i.Mic, MicUID: i.MicUID, InputRate: i.InputRate, FirstSample: i.First,
				PaddedS: float64(i.Padded) / audio.Rate, Overruns: i.Overruns}
		}
		if werr := writeMeta(rec.dir, *m); werr != nil {
			log.Printf("stop %s: meta.toml: %v", m.ID, werr)
		}
		log.Printf("stop %s: %.1f s, error %q", m.ID, m.DurationS, m.Error)
		r.mu.Lock()
		if r.cur == rec {
			r.cur = nil
		}
		last := *m
		r.last = &last
		r.mu.Unlock()
		if r.onStop != nil {
			r.onStop(rec.dir, *m)
		}
	})
	return rec.meta
}

// micName is the input device's name, as the tap reports it.
func (rec *recording) micName() string {
	if rec.src.Info != nil {
		if n := rec.src.Info().Mic; n != "" {
			return n
		}
	}
	return "the default input"
}

// micWarning says the mic has delivered digital silence for d, and what to
// do about it.
func micWarning(mic string, d time.Duration) string {
	return fmt.Sprintf("mic %q has delivered digital silence for %.0f s (every block at or below %d dBFS): "+
		"the input is muted or disconnected, or it is not the one you speak into — unmute it, "+
		"or stop and pin another input with [meeting.capture] mic", mic, d.Seconds(), audio.DeadLevel)
}

// Mic is the recording mic's state now, for `start`'s check, `status` and
// a menu bar polling the socket.
type Mic struct {
	Device    string    `json:"device"`            // the input's name
	LevelDBFS float64   `json:"level_dbfs"`        // loudest 20 ms block of the last 100 ms; -120 before the first
	Silent    bool      `json:"silent"`            // digital silence for audio.DeadAfter or longer
	Since     time.Time `json:"since,omitzero"`    // when the silence began; zero unless Silent
	Warning   string    `json:"warning,omitempty"` // what to tell the user; set when Silent
}

// Status is what `status` reports.
type Status struct {
	Recording bool              `json:"recording"`
	Current   *Meta             `json:"current,omitempty"`
	Seconds   float64           `json:"seconds,omitempty"` // recorded so far
	Mic       *Mic              `json:"mic,omitempty"`     // while recording
	Dir       string            `json:"dir"`               // recordings root
	Last      *Meta             `json:"last,omitempty"`
	Uploads   map[string]string `json:"uploads,omitempty"` // this serve's uploads: id → progress
}

func (r *recorder) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{Dir: r.dir, Last: r.last, Uploads: maps.Clone(r.uploads)}
	if rec := r.cur; rec != nil {
		m := rec.meta
		s.Recording, s.Current = true, &m
		s.Seconds = float64(rec.takes[1].Samples()) / audio.Rate
		s.Mic = &Mic{Device: rec.micName(), LevelDBFS: rec.mic.Level()}
		if d := rec.mic.Silent(); d >= audio.DeadAfter {
			s.Mic.Silent, s.Mic.Since = true, r.now().Add(-d)
			s.Mic.Warning = micWarning(s.Mic.Device, d)
		}
	}
	return s
}

// repair fixes what a killed recorder left: every track WAV's header is
// patched to the samples on disk, and a meta.toml without a stop time gets
// one from the mic track's length, marked interrupted. It returns the
// directories it completed that way.
func repair(root string) ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "*", "meta.toml"))
	if err != nil {
		return nil, err
	}
	var errs []error
	var done []string
	for _, p := range dirs {
		dir := filepath.Dir(p)
		if _, err := (store.Store{Dir: filepath.Join(dir, "tracks")}).Repair(); err != nil {
			errs = append(errs, err)
			continue
		}
		m, err := readMeta(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		if !m.Stopped.IsZero() {
			continue
		}
		for i, t := range m.Tracks {
			st, err := os.Stat(filepath.Join(dir, t.File))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			m.Tracks[i].Bytes = st.Size()
			m.Tracks[i].Samples = int(st.Size()-44) / 2
		}
		for _, t := range m.Tracks {
			if t.Role == "mic" {
				m.DurationS = float64(t.Samples) / audio.Rate
			}
		}
		m.Stopped = m.Started.Add(time.Duration(m.DurationS * float64(time.Second)))
		m.Interrupted = true
		if err := writeMeta(dir, m); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Printf("repair %s: interrupted take, %.1f s kept", m.ID, m.DurationS)
		done = append(done, dir)
	}
	return done, errors.Join(errs...)
}
