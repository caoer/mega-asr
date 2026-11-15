package refine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/caoer/mega-asr/internal/store"
)

// Spot is a take's locked target as the text sources read it: the field
// dictated into, or the herdr pane. A target without one of them reports
// ok false for it.
type Spot interface {
	// Secure reports whether what is typed there is secret now: the field
	// is a password field, or the system's secure keyboard input is on.
	Secure() bool
	// FieldText is the text of the field dictated into and the cursor's
	// place in it, in UTF-16 units; ok is false when the target has no
	// field or the field's value is not text.
	FieldText(ctx context.Context) (text string, cursor int, ok bool)
	// PaneText is the last lines of the herdr pane dictated into; ok is
	// false when the target is not a pane.
	PaneText(ctx context.Context, lines int) (text string, ok bool, err error)
}

// Pasteboard is the machine's clipboard as the clipboard source reads it.
type Pasteboard interface {
	// Count is the clipboard's change count, which moves with every copy;
	// own reports that what it holds is Mega Voice's own last copy.
	Count() (count int, own bool)
	// Text is the text on the clipboard; ok is false when it holds none,
	// or when its copier marked it concealed or transient (a password
	// manager's copy).
	Text() (text string, ok bool)
}

// The text sources (refine.context), in the order a call's <context>
// carries them.
const (
	SourceApp       = "app"       // the application and its window's title
	SourceField     = "field"     // the text before the cursor in the field dictated into
	SourcePane      = "pane"      // the last lines of the herdr pane dictated into
	SourceClipboard = "clipboard" // what was copied just before the take or during it
)

// Why a text source sent nothing (Part.Withheld), besides WithheldSecure,
// WithheldExcluded, WithheldEmpty and WithheldError.
const (
	WithheldNone  = "none"  // the target has no such thing: no field, not a pane, no application
	WithheldStale = "stale" // the clipboard was last copied to before ClipWithin ahead of the take
	WithheldOwn   = "own"   // the clipboard holds Mega Voice's own last copy
	// WithheldText is the screen's when the field or the pane gave the
	// call its text: the screen goes only where they give nothing.
	WithheldText = "text"
)

// The text sources' bounds.
const (
	// FieldChars is how much of a field is sent: the characters before
	// the cursor, up to this many.
	FieldChars = 2000
	// PaneLines is how many of a pane's last lines are read, and PaneChars
	// how many of their characters are sent, from the end.
	PaneLines = 60
	PaneChars = 6000
	// ClipChars is how much of the clipboard is sent, from its start.
	ClipChars = 2000
	// ClipWithin is how long before a take a copy still counts as copied
	// for it.
	ClipWithin = 8 * time.Second
)

// Part is what one text source got for a take.
type Part struct {
	Source   string
	Text     string        // as sent; "" when Withheld is set
	Took     time.Duration // the read
	Withheld string        // why nothing of it is sent; "" when Text is
	Err      error
}

// TextSources is the model step's text sources as one take reads them.
type TextSources struct {
	App, Field, Pane, Clipboard bool
	// Exclude lists applications by bundle id from which nothing is read.
	Exclude []string
	Clip    *ClipWatch // the clipboard's watcher; nil reads no clipboard
}

// On reports whether any text source is switched on.
func (s TextSources) On() bool { return s.App || s.Field || s.Pane || s.Clipboard }

// Start is s as a take starts: the clipboard as it stands then. Read is
// the call that reads the take's sources at its stop.
func (s TextSources) Start() *TakeSources {
	ts := &TakeSources{src: s, start: time.Now()}
	if s.Clipboard && s.Clip != nil {
		ts.count, ts.copied = s.Clip.Last()
	}
	return ts
}

// TakeSources is one take's text sources, started.
type TakeSources struct {
	src    TextSources
	start  time.Time
	count  int       // the clipboard's change count as the take started
	copied time.Time // when it last moved before the take; zero when unknown
}

// Read reads the take's switched-on sources from its locked target, which
// target records and spot reads (nil when the target has neither field nor
// pane). Nothing is read from an excluded application or while what is
// typed there is secret.
func (ts *TakeSources) Read(ctx context.Context, target *store.Target, spot Spot) []Part {
	s := ts.src
	var on []string
	for _, src := range []struct {
		name string
		on   bool
	}{{SourceApp, s.App}, {SourceField, s.Field}, {SourcePane, s.Pane}, {SourceClipboard, s.Clipboard}} {
		if src.on {
			on = append(on, src.name)
		}
	}
	all := func(why string) []Part {
		parts := make([]Part, len(on))
		for i, name := range on {
			parts[i] = Part{Source: name, Withheld: why}
		}
		return parts
	}
	if target != nil && target.BundleID != "" && slices.Contains(s.Exclude, target.BundleID) {
		return all(WithheldExcluded)
	}
	if spot != nil && spot.Secure() {
		return all(WithheldSecure)
	}
	parts := make([]Part, 0, len(on))
	for _, name := range on {
		t0 := time.Now()
		var p Part
		switch name {
		case SourceApp:
			p = appPart(target)
		case SourceField:
			p = fieldPart(ctx, spot)
		case SourcePane:
			p = panePart(ctx, target, spot)
		case SourceClipboard:
			p = ts.clipPart()
		}
		p.Source, p.Took = name, time.Since(t0)
		parts = append(parts, p)
	}
	return parts
}

func appPart(t *store.Target) Part {
	if t == nil || t.App == "" {
		return Part{Withheld: WithheldNone}
	}
	line := "Application in front of the speaker: " + t.App
	if t.Title != "" {
		line += fmt.Sprintf(", window %q", t.Title)
	}
	if t.Kind == "herdr" {
		line += "; dictating into a herdr terminal pane"
		if t.Workspace != "" {
			line += fmt.Sprintf(" in workspace %q", t.Workspace)
		}
		if t.Agent != "" {
			line += ", where the agent " + t.Agent + " runs"
		}
	}
	return Part{Text: line + "."}
}

func fieldPart(ctx context.Context, spot Spot) Part {
	if spot == nil {
		return Part{Withheld: WithheldNone}
	}
	text, cursor, ok := spot.FieldText(ctx)
	if !ok {
		return Part{Withheld: WithheldNone}
	}
	before := strings.TrimSpace(beforeCursor(text, cursor))
	if before == "" {
		return Part{Withheld: WithheldEmpty}
	}
	return Part{Text: "Text already in the field being dictated into, up to the cursor:\n" + tail(before, FieldChars)}
}

// beforeCursor is text up to cursor, a UTF-16 offset; all of it when the
// offset lies past its end.
func beforeCursor(text string, cursor int) string {
	u := utf16.Encode([]rune(text))
	if cursor < 0 || cursor >= len(u) {
		return text
	}
	return string(utf16.Decode(u[:cursor]))
}

func panePart(ctx context.Context, target *store.Target, spot Spot) Part {
	if spot == nil {
		return Part{Withheld: WithheldNone}
	}
	text, ok, err := spot.PaneText(ctx, PaneLines)
	switch {
	case !ok:
		return Part{Withheld: WithheldNone}
	case err != nil:
		return Part{Withheld: WithheldError, Err: err}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Part{Withheld: WithheldEmpty}
	}
	what := "Last lines of the terminal pane being dictated into"
	if target != nil && target.Agent != "" {
		what += ", where the agent " + target.Agent + " runs"
	}
	return Part{Text: what + ":\n" + tail(text, PaneChars)}
}

func (ts *TakeSources) clipPart() Part {
	w := ts.src.Clip
	if w == nil {
		return Part{Withheld: WithheldNone}
	}
	count, own := w.Board.Count()
	switch {
	case own:
		return Part{Withheld: WithheldOwn}
	case count == ts.count && (ts.copied.IsZero() || ts.start.Sub(ts.copied) > ClipWithin):
		return Part{Withheld: WithheldStale}
	}
	text, ok := w.Board.Text()
	if text = strings.TrimSpace(text); !ok || text == "" {
		return Part{Withheld: WithheldEmpty}
	}
	if r := []rune(text); len(r) > ClipChars {
		text = string(r[:ClipChars])
	}
	return Part{Text: "Copied to the clipboard just before or while speaking:\n" + text}
}

// tail is the last n characters of s.
func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// Compose is a call's <context>: the text sources' parts that were read,
// in their order, then the screen's, each its own paragraph; "" when none
// gave anything. The screen goes only when neither the field nor the pane
// gave text; otherwise it is marked WithheldText and its picture dropped.
func Compose(parts []Part, sc *Screen) string {
	var out []string
	text := false
	for _, p := range parts {
		if p.Withheld != "" || p.Text == "" {
			continue
		}
		out = append(out, p.Text)
		text = text || p.Source == SourceField || p.Source == SourcePane
	}
	if sc != nil && sc.Withheld == "" {
		if text {
			sc.Withheld, sc.Text, sc.Image = WithheldText, "", nil
		} else {
			out = append(out, sc.Context())
		}
	}
	return strings.Join(out, "\n\n")
}

// ClipWatch keeps when the clipboard last moved: macOS counts copies but
// does not date them, so the watcher looks every Every while On says the
// clipboard source is switched on.
type ClipWatch struct {
	Board Pasteboard
	Every time.Duration // 0 is half a second
	On    func() bool   // nil: always

	mu      sync.Mutex
	known   bool // count has been seen; false while the source is off
	count   int
	changed time.Time // when count was first seen at its value; zero when that was the first look
}

// Run watches until ctx ends.
func (w *ClipWatch) Run(ctx context.Context) {
	every := w.Every
	if every == 0 {
		every = 500 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		w.look(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// look reads the count once at now.
func (w *ClipWatch) look(now time.Time) {
	if w.On != nil && !w.On() {
		w.mu.Lock()
		w.known = false
		w.mu.Unlock()
		return
	}
	count, _ := w.Board.Count()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case !w.known:
		w.known, w.count, w.changed = true, count, time.Time{}
	case count != w.count:
		w.count, w.changed = count, now
	}
}

// Last is the clipboard's count now and when it moved to it, as far as the
// watcher saw; zero time when it did not see the move. A move since the
// watcher's last look is dated now.
func (w *ClipWatch) Last() (count int, changed time.Time) {
	count, _ = w.Board.Count()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case !w.known:
		return count, time.Time{}
	case count != w.count:
		return count, time.Now()
	}
	return count, w.changed
}
