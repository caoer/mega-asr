// Package takes is the Takes page: every take of the store, its texts,
// audio, events and label, served under /takes/ on megavoice's loopback
// listener, and Guard, which answers for everything that listener serves.
//
// A handler is added in routes, with a pattern under /takes/api/; Guard
// already holds it to the loopback, to a page megavoice opened and, for a
// POST, to JSON.
package takes

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

//go:embed page/index.html page/app.js page/styles.css page/fonts/*.woff2
var page embed.FS

// Engine is an engine a take can be re-transcribed with.
type Engine struct {
	Name    string `json:"name"`
	Local   bool   `json:"local"`
	Service string `json:"service,omitempty"` // a cloud engine: the service its audio goes to
	Primary bool   `json:"primary"`           // asr.engine
	Named   bool   `json:"named"`             // re-transcription accepts it: a local engine, or a cloud engine the config runs
}

// PageTitle is the Takes page's title, which a browser window showing the
// page carries in its own title: the resend picker offers no such window.
const PageTitle = "录音历史 · megavoice"

// Handler serves the Takes page and its API under /takes/, and sends the
// listener's root, the compare panel's old address, to the page.
type Handler struct {
	Dir     string          // store.data
	Labels  *compare.Labels // store.labels: the page reads and writes each take's label there
	Engines func() []Engine // read at each request

	// The actions and the resend picker; serve sets them. Unset, their
	// routes answer 503.
	Ctrl    *session.Controller
	Resolve func(to string) (session.Target, error) // a resend's to as a target: pane:<id>, app:<pid>:<bundle id>, clipboard
	Engine  func(name string) (session.Engine, error)
	Panes   func() ([]Pane, error) // herdr's panes; an error when herdr does not answer
	Apps    func() []App           // the running apps

	index *Index
	mux   *http.ServeMux
}

// New is the Takes page over the takes in dir and their labels; it starts
// building the index at once.
func New(dir string, labels *compare.Labels, engines func() []Engine) *Handler {
	h := &Handler{Dir: dir, Labels: labels, Engines: engines, index: &Index{Dir: dir, Labels: labels}, mux: http.NewServeMux()}
	h.routes()
	go h.index.refresh(true)
	return h
}

func (h *Handler) routes() {
	files, _ := fs.Sub(page, "page")
	// the old address of the compare panel: a browser keeps the address's
	// fragment, the key, across the redirect
	h.mux.Handle("GET /{$}", http.RedirectHandler("/takes/", http.StatusFound))
	h.mux.Handle("GET /takes/", http.StripPrefix("/takes/", http.FileServerFS(files)))
	h.mux.HandleFunc("GET /takes/api/takes", h.list)
	h.mux.HandleFunc("GET /takes/api/take/{id}", h.take)
	h.mux.HandleFunc("GET /takes/api/peaks/{id}", h.peaks)
	h.mux.HandleFunc("GET /takes/api/engines", h.engines)
	h.mux.HandleFunc("GET /takes/audio/{file}", h.audio)
	h.mux.HandleFunc("GET /takes/api/targets", h.targets)
	for path, f := range map[string]http.HandlerFunc{"/takes/api/seen": h.seen, "/takes/api/dismiss": h.dismiss, "/takes/api/resend": h.resend, "/takes/api/retranscribe": h.retranscribe, "/takes/api/label": h.label} {
		h.mux.HandleFunc("POST "+path, f)
		// GET /takes/ would serve it as a page file: a GET answers 405
		h.mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		})
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Counted is how many takes are undelivered and not seen: the menu's count.
func (h *Handler) Counted() (int, error) { return h.index.Counted() }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	n, _ := strconv.Atoi(v.Get("n"))
	if n <= 0 || n > 200 {
		n = 50
	}
	pg, err := h.index.List(Query{Q: v.Get("q"), State: v.Get("state"), Target: v.Get("target"), Device: v.Get("device"),
		Day: v.Get("day"), Before: v.Get("before"), N: n, Facets: v.Get("facets") == "1"})
	reply(w, pg, err)
}

func (h *Handler) take(w http.ResponseWriter, r *http.Request) {
	t, ok := h.index.Take(r.PathValue("id"))
	if !ok {
		refuse(w, fmt.Errorf("%w: %s", session.ErrNoTake, r.PathValue("id")))
		return
	}
	reply(w, t, nil)
}

// wav is the WAV the page plays for the take id: the track its text came
// from (wavOf).
func (h *Handler) wav(id string) string {
	evs, _ := store.Read(filepath.Join(h.Dir, id))
	p, _ := wavOf(h.Dir, id, evs)
	return p
}

// peaks is the waveform of the take's played track: n buckets (default
// 600, at most 4000), each the largest absolute sample in it over full
// scale.
func (h *Handler) peaks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !takeID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(h.wav(id))
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		reply(w, nil, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 4000 {
		n = 600
	}
	var data []byte
	if len(b) > 44 {
		data = b[44 : 44+(len(b)-44)&^1]
	}
	samples := len(data) / 2
	out := struct {
		DurS  float64   `json:"dur_s"`
		Peaks []float64 `json:"peaks"`
	}{DurS: float64(samples) / audio.Rate, Peaks: make([]float64, 0, n)}
	for i := range min(n, samples) {
		lo, hi := i*samples/min(n, samples), (i+1)*samples/min(n, samples)
		top := 0
		for j := lo; j < hi; j++ {
			v := int(int16(uint16(data[2*j]) | uint16(data[2*j+1])<<8))
			top = max(top, v, -v)
		}
		out.Peaks = append(out.Peaks, math.Round(float64(top)/32768*1000)/1000)
	}
	reply(w, out, nil)
}

func (h *Handler) engines(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string][]Engine{"engines": h.Engines()}, nil)
}

func (h *Handler) audio(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".wav")
	if !ok || !takeID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, h.wav(id))
}

// seen marks a take as opened on the page: a seen line in its record, which
// takes an undelivered take out of the count. A take without a record (from
// before the record existed) gets none, so it stays a take without one.
// The answer is the take's row.
func (h *Handler) seen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	t, ok := h.index.Take(in.ID)
	if !ok {
		refuse(w, fmt.Errorf("%w: %s", session.ErrNoTake, in.ID))
		return
	}
	if t.Record {
		if err := store.Append(filepath.Join(h.Dir, in.ID), store.Event{Ev: "seen"}); err != nil {
			reply(w, nil, err)
			return
		}
		t, _ = h.index.Take(in.ID)
	}
	reply(w, t.Row, nil)
}

// dismiss dismisses undelivered takes, like marking them read: a dismiss
// line in each one's record takes it out of every 未送达 count, and nothing
// else of it changes. With undo it takes a dismiss back: an undismiss line.
// The takes are ids, or with all (a dismiss only) every take undelivered and
// not dismissed. A take in neither state is left as it is. The answer is the
// ids whose record got the line, which an undo of the same click sends back.
func (h *Handler) dismiss(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs  []string `json:"ids"`
		All  bool     `json:"all"`
		Undo bool     `json:"undo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err)
		return
	}
	if in.All && !in.Undo {
		ids, err := h.index.Undelivered()
		if err != nil {
			reply(w, nil, err)
			return
		}
		in.IDs = ids
	}
	ev, from := "dismiss", "undelivered"
	if in.Undo {
		ev, from = "undismiss", "dismissed"
	}
	done := []string{}
	for _, id := range in.IDs {
		t, ok := h.index.Take(id)
		if !ok || filterState(t.Row) != from {
			continue
		}
		if err := store.Append(filepath.Join(h.Dir, id), store.Event{Ev: ev}); err != nil {
			reply(w, nil, err)
			return
		}
		h.index.Take(id)
		done = append(done, id)
	}
	reply(w, map[string][]string{"ids": done}, nil)
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		refuse(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
