package takes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/compare"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// Pane is a herdr pane a resend can reach.
type Pane struct {
	ID, Workspace string // Workspace: its workspace's label
	Title, Agent  string // the pane's title, the process running in it
	Cwd           string
}

// App is a running app a resend can reach, with its front window's title.
// Front is the frontmost app: the browser showing the Takes page when the
// picker opens or a resend arrives.
type App struct {
	PID            int
	BundleID, Name string
	Title          string
	Front          bool
}

// showsPage says whether a paste into a would land in a Takes page: a is
// the frontmost app (the browser the page is used in), or its front window
// shows the page, as a browser window behind the one in use can.
func (a App) showsPage() bool { return a.Front || strings.Contains(a.Title, PageTitle) }

// Target is one entry of the resend picker. To is what the resend sends as
// "to"; Label names the target in full.
type Target struct {
	Group     string `json:"group"`        // own, recent, pane, app, clipboard
	To        string `json:"to,omitempty"` // pane:<id>, app:<pid>:<bundle id> or clipboard; none for a closed own target
	Label     string `json:"label"`
	Workspace string `json:"workspace,omitempty"` // a pane's workspace label
	Gone      bool   `json:"gone,omitempty"`      // own or recent: the target cannot be reached now
	Pane      bool   `json:"pane,omitempty"`      // gone: the target is a herdr pane
	Front     bool   `json:"front,omitempty"`     // own: the take's target is an app that shows this page (App.showsPage)
}

// Targets is the resend picker: the take's own target, the targets of recent
// deliveries, herdr's panes, the running apps and the clipboard, in that
// order. Herdr is false when herdr did not answer; the list then holds no
// pane. A recent target that cannot be reached now is listed gone, with no
// "to". An app that shows the page (the frontmost, the browser in use, or
// one whose front window is the page) is offered nowhere: a paste into it
// lands in the page. Nor is an app without a bundle id, which a resend
// cannot name.
type Targets struct {
	Targets []Target `json:"targets"`
	Herdr   bool     `json:"herdr"`
}

// recent is how many targets of recent deliveries the picker offers.
const recent = 5

// targets answers GET /takes/api/targets[?id=<take>]: with id, the list leads
// with that take's own target, marked gone when it no longer exists.
func (h *Handler) targets(w http.ResponseWriter, r *http.Request) {
	if h.Panes == nil || h.Apps == nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", errors.New("megavoice lists no targets here"))
		return
	}
	var own *store.Target
	if id := r.URL.Query().Get("id"); id != "" {
		t, ok := h.index.Take(id)
		if !ok {
			refuse(w, fmt.Errorf("%w: %s", session.ErrNoTake, id))
			return
		}
		own = t.Target
	}
	sent, err := h.index.Sent(recent)
	if err != nil {
		refuse(w, err)
		return
	}
	panes, perr := h.Panes()
	var apps, page []App // page: the apps that show this page
	for _, a := range h.Apps() {
		switch {
		case a.showsPage():
			page = append(page, a)
		case a.BundleID != "": // a resend names an app by pid and bundle id
			apps = append(apps, a)
		}
	}
	out := Targets{Targets: []Target{}, Herdr: perr == nil}
	listed := map[string]bool{}
	add := func(group string, t Target) {
		k := t.To
		if t.Gone {
			k = "gone:" + t.Label
		}
		if k != "" {
			if listed[k] {
				return
			}
			listed[k] = true
		}
		t.Group = group
		out.Targets = append(out.Targets, t)
	}
	if own != nil {
		if t, ok := live(own, panes, apps); ok {
			add("own", t)
		} else if t, ok := live(own, nil, page); ok {
			add("own", Target{Label: t.Label, Front: true})
		} else {
			add("own", Target{Label: targetLabel(own), Gone: true, Pane: own.Pane != ""})
		}
	}
	for i := range sent {
		s := &sent[i]
		if t, ok := live(s, panes, apps); ok {
			if t.To != "clipboard" {
				add("recent", t)
			}
		} else if _, ok := live(s, nil, page); !ok {
			add("recent", Target{Label: targetLabel(s), Gone: true, Pane: s.Pane != ""})
		}
	}
	for _, p := range panes {
		add("pane", paneTarget(p))
	}
	for _, a := range apps {
		add("app", appTarget(a))
	}
	add("clipboard", Target{To: "clipboard", Label: "剪贴板"})
	reply(w, out, nil)
}

// live is the target a record names, as it can be reached now: its pane by
// id, its app by bundle id (else by name) with the pid it runs as now; of
// two instances, the one with the recorded pid.
func live(t *store.Target, panes []Pane, apps []App) (Target, bool) {
	switch {
	case t.Kind == "clipboard":
		return Target{To: "clipboard", Label: "剪贴板"}, true
	case t.Pane != "":
		for _, p := range panes {
			if p.ID == t.Pane {
				return paneTarget(p), true
			}
		}
	case t.BundleID != "" || t.App != "":
		found := -1
		for i, a := range apps {
			if isApp(t, a) {
				if found < 0 || a.PID == t.PID {
					found = i
				}
			}
		}
		if found >= 0 {
			return appTarget(apps[found]), true
		}
	}
	return Target{}, false
}

// isApp says whether the record's app target is the app a: by bundle id,
// else by name.
func isApp(t *store.Target, a App) bool {
	if t.Pane != "" {
		return false
	}
	if t.BundleID != "" {
		return a.BundleID == t.BundleID
	}
	return t.App != "" && a.Name == t.App
}

// paneTarget labels a pane `workspace › pane title or process · folder · id`.
func paneTarget(p Pane) Target {
	var parts []string
	name := p.Title
	if name == "" {
		name = p.Agent
	}
	if name != "" {
		parts = append(parts, name)
	}
	if p.Cwd != "" {
		parts = append(parts, filepath.Base(p.Cwd))
	}
	parts = append(parts, p.ID)
	label := strings.Join(parts, " · ")
	if p.Workspace != "" {
		label = p.Workspace + " › " + label
	}
	return Target{To: "pane:" + p.ID, Label: label, Workspace: p.Workspace}
}

// appTarget labels an app `name — its front window's title`; its to names
// the pid with the bundle id, so a resend reaches the app the page showed.
func appTarget(a App) Target {
	label := a.Name
	if label == "" {
		label = a.BundleID
	}
	if a.Title != "" {
		label += " — " + a.Title
	}
	return Target{To: "app:" + strconv.Itoa(a.PID) + ":" + a.BundleID, Label: label}
}

// A resend's refusals of its target.
var (
	errTargetGone = errors.New("the target is gone")
	errNoHerdr    = errors.New("herdr does not answer")
	errFrontApp   = errors.New("the frontmost app shows this page: a paste would land in it")
	errPageWindow = errors.New("the app's front window shows this page: a paste would land in it")
	// a pid alone cannot tell the app the page showed from one that took
	// its pid since
	errBarePID = errors.New("to: app:<pid>:<bundle id> wants a pid and the app's bundle id")
	errBadTo   = errors.New("to: want pane:<id>, app:<pid>:<bundle id> or clipboard")
)

// reachable checks a resend's to against what can be reached now, as the
// picker listed it: a pane herdr lists, an app running as that pid with
// that bundle id that does not show this page (App.showsPage), where a
// paste would land. It answers nil when to is reachable.
func (h *Handler) reachable(to string) error {
	kind, arg, _ := strings.Cut(to, ":")
	switch {
	case to == "clipboard":
		return nil
	case kind == "pane":
		panes, err := h.Panes()
		if err != nil {
			return errNoHerdr
		}
		for _, p := range panes {
			if p.ID == arg {
				return nil
			}
		}
		return errTargetGone
	case kind == "app":
		spid, bundle, _ := strings.Cut(arg, ":")
		pid, err := strconv.Atoi(spid)
		if err != nil || pid <= 0 || bundle == "" {
			return errBarePID
		}
		for _, a := range h.Apps() {
			switch {
			case a.PID != pid:
			case a.BundleID != bundle:
				return errTargetGone
			case a.Front:
				return errFrontApp
			case a.showsPage():
				return errPageWindow
			default:
				return nil
			}
		}
		return errTargetGone
	}
	return errBadTo
}

// resend answers POST /takes/api/resend {"id", "to", "send", "text"}: the
// take's text to the target the page picked, through the controller's
// Resend, via page. "send" presses Enter after the paste and is false when
// absent. A target gone since the picker listed it, and an app that shows
// this page, are refused with 409 and nothing appended (reachable). The answer is the
// deliver line; a delivery that failed is a line with ok false and its err,
// and a line the record could not take carries not_recorded (200: the
// delivery ran).
func (h *Handler) resend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   string `json:"id"`
		To   string `json:"to"`
		Send bool   `json:"send"`
		Text string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	if h.Ctrl == nil || h.Resolve == nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", errors.New("megavoice resends nothing here"))
		return
	}
	switch {
	case in.To == "" || in.To == "front":
		// the page has the focus: front would paste into the browser
		refuse(w, errBadTo)
		return
	case in.Send && in.To == "clipboard":
		fail(w, http.StatusBadRequest, "bad_request", errors.New("send: the clipboard has no Enter to press"))
		return
	}
	if err := h.reachable(in.To); err != nil {
		refuse(w, err)
		return
	}
	to, err := h.Resolve(in.To)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err)
		return
	}
	de, err := h.Ctrl.Resend(session.Resend{ID: in.ID, To: to, Send: in.Send, Text: in.Text, Via: "page"})
	if err != nil && !errors.Is(err, session.ErrNotRecorded) {
		refuse(w, err)
		return
	}
	h.index.Take(in.ID)
	reply(w, notRecorded(de, err), nil)
}

// notRecorded is a resend's answer: the deliver line, and when the record
// could not take it, the line with not_recorded saying why. The delivery
// ran either way, so the page shows what the line says went out.
func notRecorded(de store.Event, err error) any {
	b, merr := json.Marshal(de)
	if err == nil || merr != nil {
		return de
	}
	why, _ := json.Marshal(err.Error())
	return json.RawMessage(append(append(append(b[:len(b)-1:len(b)-1], `,"not_recorded":`...), why...), '}'))
}

// retranscribe answers POST /takes/api/retranscribe {"id", "engine"}: the
// take's audio decoded again by the controller's Retranscribe. The answer
// is the retranscribe line; an engine that failed is a line with its err.
// The decode runs to its end when the page goes away, and its line is
// appended.
func (h *Handler) retranscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Engine string `json:"engine"`
	}
	if !decode(w, r, &in) {
		return
	}
	if h.Ctrl == nil || h.Engine == nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", errors.New("megavoice re-transcribes nothing here"))
		return
	}
	e, err := h.Engine(in.Engine)
	if err != nil {
		fail(w, http.StatusBadRequest, "unknown_engine", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	re, err := h.Ctrl.Retranscribe(ctx, in.ID, e)
	if err != nil {
		refuse(w, err)
		return
	}
	h.index.Take(in.ID)
	reply(w, re, nil)
}

// label answers POST /takes/api/label {"id", "ref", "note", "correct",
// "clear"} (compare.Label): the take's label appended to the label file,
// answered with the row it got.
func (h *Handler) label(w http.ResponseWriter, r *http.Request) {
	var in compare.Label
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil { // a long take's text
		fail(w, http.StatusBadRequest, "bad_request", err)
		return
	}
	if h.Labels == nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", errors.New("megavoice keeps no labels here"))
		return
	}
	row, err := h.Labels.Save(h.Dir, in)
	if err != nil {
		refuse(w, err)
		return
	}
	reply(w, row, nil)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err)
		return false
	}
	return true
}

// Refusal is the answer to a request the API refuses: a code the page words
// it by, and the server's own words, which the page shows for a code it
// does not know.
type Refusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// refusals are the errors refuse answers with a status and a code, the
// first that matches.
var refusals = []struct {
	err    error
	status int
	code   string
}{
	{session.ErrNoTake, http.StatusNotFound, "no_take"},
	{compare.ErrNoTake, http.StatusNotFound, "no_take"},
	{session.ErrRecording, http.StatusConflict, "recording"},
	{session.ErrTranscribing, http.StatusConflict, "transcribing"},
	{session.ErrResending, http.StatusConflict, "resending"},
	{session.ErrRetranscribing, http.StatusConflict, "retranscribing"},
	{session.ErrDeliveryBusy, http.StatusConflict, "delivery_busy"},
	{errTargetGone, http.StatusConflict, "target_gone"},
	{errNoHerdr, http.StatusConflict, "no_herdr"},
	{errFrontApp, http.StatusConflict, "front_app"},
	{errPageWindow, http.StatusConflict, "page_window"},
	{errBarePID, http.StatusBadRequest, "bad_request"},
	{errBadTo, http.StatusBadRequest, "bad_request"},
	{session.ErrNoText, http.StatusUnprocessableEntity, "no_text"},
	{session.ErrNotNamed, http.StatusUnprocessableEntity, "not_named"},
	{compare.ErrEmpty, http.StatusBadRequest, "label_empty"},
	{compare.ErrEngine, http.StatusBadRequest, "no_answer"},
	{compare.ErrBackup, http.StatusBadRequest, "backup_text"},
	{syscall.ENOSPC, http.StatusInternalServerError, "disk_full"},
	{fs.ErrPermission, http.StatusInternalServerError, "no_permission"},
}

// refuse answers a request refused, nothing appended, by refusals: 404 no
// such take; 409 the take or the delivery lock is busy, or the target
// cannot take the text; 422 no text of that kind, or a cloud engine the
// config does not name; 400 a label the file does not take; 500 for the
// rest, a full disk, a file megavoice may not write and a
// re-transcription's line the record could not take
// (session.ErrNotRecorded) among them, with the code "failed" when no other
// says why.
func refuse(w http.ResponseWriter, err error) {
	for _, r := range refusals {
		if errors.Is(err, r.err) {
			fail(w, r.status, r.code, err)
			return
		}
	}
	fail(w, http.StatusInternalServerError, "failed", err)
}

// fail answers status with a Refusal.
func fail(w http.ResponseWriter, status int, code string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Refusal{Code: code, Error: err.Error()})
}
