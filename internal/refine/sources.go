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
	// Read is one look at the clipboard, taken whole: want gets its change
	// count and own as Count gives them and says whether its text is read.
	// Nothing copies to the clipboard between the two. The text's ok is
	// false when want said no, when it holds no text, or when its copier
	// marked it concealed or transient (a password manager's copy).
	Read(want func(count int, own bool) bool) (text string, ok bool)
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

// Start is s as a take starts: the clipboard as its watcher last saw it,
// with no call to the machine, so a take may start under a lock. Read is
// the call that reads the take's sources at its stop.
func (s TextSources) Start() *TakeSources {
	ts := &TakeSources{src: s, start: time.Now()}
	if s.Clipboard && s.Clip != nil {
		ts.count, ts.copied, ts.known = s.Clip.Last()
	}
	return ts
}

// TakeSources is one take's text sources, started.
type TakeSources struct {
	src    TextSources
	start  time.Time
	known  bool      // the watcher had looked at the clipboard as the take started
	count  int       // the clipboard's change count then
	copied time.Time // when it last moved before the take; zero when unknown
}

// Within is ts with the sources now switched off left off: a switch turned
// off during the take holds at its stop, and an application excluded
// since is excluded. A switch turned on during it stays off for this take.
func (ts *TakeSources) Within(now TextSources) *TakeSources {
	n := *ts
	n.src.App = n.src.App && now.App
	n.src.Field = n.src.Field && now.Field
	n.src.Pane = n.src.Pane && now.Pane
	n.src.Clipboard = n.src.Clipboard && now.Clipboard
	n.src.Exclude = append(slices.Clip(n.src.Exclude), now.Exclude...)
	return &n
}

// Read reads the take's switched-on sources from its locked target, which
// target records and spot reads (nil when the target has neither field nor
// pane). Nothing is read from an excluded application or while what is
// typed there is secret, and nothing for a take sent to Telegram: it is
// not dictated into anything on the Mac, so it has no application, field
// or pane of its own, and the clipboard's guards (a secret field, an
// excluded application) have nothing to look at.
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
	if target != nil && target.Kind == "telegram" {
		return all(WithheldNone)
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
	why := ""
	text, ok := w.Board.Read(func(count int, own bool) bool {
		switch {
		case own:
			why = WithheldOwn
		case !ts.known: // the watcher had not looked: no copy can be dated
			why = WithheldStale
		case count == ts.count && (ts.copied.IsZero() || ts.start.Sub(ts.copied) > ClipWithin):
			why = WithheldStale
		}
		return why == ""
	})
	if why != "" {
		return Part{Withheld: why}
	}
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
// clipboard source is switched on. While it is off the watcher waits for
// Wake, with no timer and no look at the clipboard.
type ClipWatch struct {
	Board Pasteboard
	Every time.Duration // 0 is half a second
	On    func() bool   // nil: always

	mu      sync.Mutex
	wake    chan struct{} // Wake's signal to a parked Run; made on first use
	known   bool          // count has been seen; false while the source is off
	count   int
	changed time.Time // when count was first seen at its value; zero when that was the first look
}

// Run watches until ctx ends: every Every while On, parked until Wake
// while not.
func (w *ClipWatch) Run(ctx context.Context) {
	every := w.Every
	if every == 0 {
		every = 500 * time.Millisecond
	}
	wake := w.wakeChan()
	for {
		if !w.look(time.Now()) {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(every):
		}
	}
}

// Wake has a parked Run look again, as the clipboard source may have been
// switched on; it never waits.
func (w *ClipWatch) Wake() {
	select {
	case w.wakeChan() <- struct{}{}:
	default:
	}
}

func (w *ClipWatch) wakeChan() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wake == nil {
		w.wake = make(chan struct{}, 1)
	}
	return w.wake
}

// look reads the count once at now; false, reading nothing, while On says
// the source is off.
func (w *ClipWatch) look(now time.Time) bool {
	if w.On != nil && !w.On() {
		w.mu.Lock()
		w.known = false
		w.mu.Unlock()
		return false
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
	return true
}

// Last is the clipboard's count at the watcher's last look and when it
// moved to it, zero when the watcher did not see the move; known is false
// when the watcher has not looked since the source was switched on. It
// asks the machine nothing: a copy since the last look shows at the take's
// stop as a count that moved.
func (w *ClipWatch) Last() (count int, changed time.Time, known bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.known {
		return 0, time.Time{}, false
	}
	return w.count, w.changed, true
}
