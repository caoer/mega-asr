//go:build darwin

package mac

import (
	"math"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/audio"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// CGRect is a flat CGRect/NSRect: four doubles, passed in float registers.
type CGRect struct{ X, Y, W, H float64 }

var (
	cgPathCreateMutable func() uintptr
	cgPathAddRect       func(path, transform uintptr, r CGRect)
	cgPathRelease       func(uintptr)
	cgColorCreateSRGB   func(r, g, b, a float64) uintptr
)

func init() {
	purego.RegisterLibFunc(&cgPathCreateMutable, libCG, "CGPathCreateMutable")
	purego.RegisterLibFunc(&cgPathAddRect, libCG, "CGPathAddRect")
	purego.RegisterLibFunc(&cgPathRelease, libCG, "CGPathRelease")
	purego.RegisterLibFunc(&cgColorCreateSRGB, libCG, "CGColorCreateSRGB")
}

const (
	ovW, ovH   = 360.0, 96.0
	ovBars     = 100
	ovFloorDB  = -60.0 // bottom of the waveform
	ovFPS      = 30
	labelH     = 18.0
	devH       = 16.0 // the input's name above the waveform
	subH       = 16.0 // the stopwatch line above that
	waveY      = labelH + 8
	waveH      = ovH - labelH - devH - 16
	ovFromBase = 90.0 // above the bottom of the visible frame
)

// warmingText stands in for the waveform while the input delivers nothing.
const warmingText = "麦克风启动中… 先别说话"

// amber is the weak-signal bar colour, and the warming text's.
var amber = [3]float64{1.0, 0.72, 0.2}

// Overlay is a borderless, non-activating panel on every Space, over
// full-screen apps, that ignores the mouse and never becomes key: the
// recording input's name over a waveform of the last 4 s coloured by signal
// quality — or, while that input is still waking up, a pulsing "do not speak
// yet" — a one-line state label, and a stopwatch line on top while a take
// transcribes. It is a dark HUD in light and dark mode alike.
// Methods may be called from any goroutine.
type Overlay struct {
	mu      sync.Mutex
	v       view
	visible bool
	ticker  *time.Ticker
	stop    chan struct{}
	hideAt  *time.Timer

	// main thread only
	panel, text, sub, dev, warm objc.ID
	bars                        [nColours]objc.ID
	curMain, curSub, curDev     string
	curWarm                     bool
}

// Wave is a recording take as the overlay shows it; the zero Wave shows
// no waveform.
type Wave struct {
	Quality func() []audio.Quality // the waveform; nil: none
	Device  string                 // the input's name; "" leaves the line empty
	Warming bool                   // the input delivers nothing yet: say so instead of the waveform
}

// view is what the overlay shows; the animation goroutine gets a copy.
type view struct {
	label    string
	wave     Wave
	started  time.Time            // zero: no stopwatch
	estimate func() time.Duration // time left; nil or <= 0: unknown
}

// NewOverlay builds the panel, hidden.
func NewOverlay() *Overlay {
	o := &Overlay{}
	DoSync(o.build)
	return o
}

func (o *Overlay) build() {
	panel := send(class("NSPanel"), "alloc")
	// NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel, NSBackingStoreBuffered
	panel = send(panel, "initWithContentRect:styleMask:backing:defer:", CGRect{0, 0, ovW, ovH}, uint(1<<7), uint(2), false)
	send(panel, "setLevel:", 1000) // NSScreenSaverWindowLevel
	send(panel, "setOpaque:", false)
	send(panel, "setBackgroundColor:", send(class("NSColor"), "clearColor"))
	send(panel, "setHasShadow:", false)
	send(panel, "setIgnoresMouseEvents:", true)
	send(panel, "setHidesOnDeactivate:", false)
	send(panel, "setReleasedWhenClosed:", false)
	// canJoinAllSpaces | stationary | ignoresCycle | fullScreenAuxiliary
	send(panel, "setCollectionBehavior:", uint(1<<0|1<<4|1<<6|1<<8))

	view := send(panel, "contentView")
	send(view, "setWantsLayer:", true)
	root := send(view, "layer")
	send(root, "setBackgroundColor:", cgColorCreateSRGB(0.08, 0.08, 0.1, 0.82))
	send(root, "setCornerRadius:", 14.0)

	scale := 2.0
	if scr := send(class("NSScreen"), "mainScreen"); scr != 0 {
		scale = objc.Send[float64](scr, sel("backingScaleFactor"))
	}

	fills := [nColours][3]float64{
		colGood:    {0.35, 0.78, 1.0},
		colWeak:    amber,
		colQuiet:   {0.55, 0.56, 0.6},
		colClipped: {1.0, 0.27, 0.27},
	}
	for c, f := range fills {
		l := send(send(class("CAShapeLayer"), "layer"), "retain")
		send(l, "setFrame:", CGRect{12, waveY, ovW - 24, waveH})
		send(l, "setFillColor:", cgColorCreateSRGB(f[0], f[1], f[2], 1.0))
		send(root, "addSublayer:", l)
		o.bars[c] = l
	}

	white := [3]float64{1, 1, 1}
	o.text = textLayer(root, CGRect{8, 4, ovW - 16, labelH}, 12, white, 0.92, scale)
	o.dev = textLayer(root, CGRect{8, ovH - devH - 6, ovW - 16, devH}, 11, white, 0.6, scale)
	o.sub = textLayer(root, CGRect{8, ovH - 2, ovW - 16, subH}, 11, white, 0.75, scale)
	o.warm = textLayer(root, CGRect{12, waveY + (waveH-20)/2, ovW - 24, 20}, 15, amber, 1, scale)
	setText(o.warm, warmingText)
	send(o.warm, "setHidden:", true)
	o.panel = panel
}

func textLayer(root objc.ID, frame CGRect, size float64, rgb [3]float64, alpha, scale float64) objc.ID {
	t := send(send(class("CATextLayer"), "layer"), "retain")
	send(t, "setFrame:", frame)
	send(t, "setFontSize:", size)
	send(t, "setForegroundColor:", cgColorCreateSRGB(rgb[0], rgb[1], rgb[2], alpha))
	send(t, "setAlignmentMode:", nsString("center"))
	send(t, "setTruncationMode:", nsString("end")) // a long device name ends in "…"
	send(t, "setContentsScale:", scale)
	send(root, "addSublayer:", t)
	return t
}

// place centres the panel near the bottom of the screen with the key window,
// tall enough for the stopwatch line when it has one.
func (o *Overlay) place() {
	scr := send(class("NSScreen"), "mainScreen")
	if scr == 0 {
		return
	}
	h := ovH
	if o.curSub != "" {
		h += subH
	}
	f := objc.Send[CGRect](scr, sel("visibleFrame"))
	send(o.panel, "setFrame:display:", CGRect{f.X + (f.W-ovW)/2, f.Y + ovFromBase, ovW, h}, true)
}

// CGPoint is a flat CGPoint/NSPoint.
type CGPoint struct{ X, Y float64 }

// Show displays label, with w's live waveform when it has one, until Hide or
// another Show/Flash. A running stopwatch stays.
func (o *Overlay) Show(label string, w Wave) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancelTimersLocked()
	o.v.label, o.v.wave = label, w
	o.visible = true
	o.startLocked(true)
}

// Stopwatch shows "transcribing 0:07 (~0:03 left)" above the waveform,
// counting from started, prefixed "prev:" while a waveform records; estimate
// gives the time left, unknown when nil or <= 0. A zero started removes it.
// Hide removes it too.
func (o *Overlay) Stopwatch(started time.Time, estimate func() time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.v.started, o.v.estimate = started, estimate
	if o.visible {
		o.stopTickerLocked()
		o.startLocked(false)
	}
}

// Flash shows label without a waveform for d, then hides.
func (o *Overlay) Flash(label string, d time.Duration) {
	o.Show(label, Wave{})
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hideAt = time.AfterFunc(d, o.Hide)
}

// Hide removes the panel and the stopwatch.
func (o *Overlay) Hide() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancelTimersLocked()
	o.v, o.visible = view{}, false
	Do(func() { send(o.panel, "orderOut:", 0) })
}

func (o *Overlay) cancelTimersLocked() {
	o.stopTickerLocked()
	if o.hideAt != nil {
		o.hideAt.Stop()
		o.hideAt = nil
	}
}

func (o *Overlay) stopTickerLocked() {
	if o.ticker != nil {
		o.ticker.Stop()
		close(o.stop)
		o.ticker = nil
	}
}

// startLocked renders the first frame of o.v and, when it moves, starts the
// ticker: 30 fps with a waveform, 10 Hz for a stopwatch alone.
func (o *Overlay) startLocked(front bool) {
	v, f := o.v, &frame{}
	f.next(v)
	fr := *f
	Do(func() {
		o.render(fr, true)
		if front {
			o.place()
			send(o.panel, "orderFrontRegardless")
		}
	})
	if v.wave.Quality == nil && v.started.IsZero() {
		return
	}
	period := time.Second / ovFPS
	if v.wave.Quality == nil {
		period = 100 * time.Millisecond
	}
	o.ticker = time.NewTicker(period)
	o.stop = make(chan struct{})
	go o.animate(o.ticker, o.stop, v, f)
}

func (o *Overlay) animate(t *time.Ticker, stop chan struct{}, v view, f *frame) {
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			f.next(v)
			fr := *f
			done := make(chan struct{})
			Do(func() { defer close(done); o.render(fr, v.wave.Quality != nil) })
			select { // never queue frames behind a busy main thread
			case <-done:
			case <-stop:
				return
			}
		}
	}
}

// frame is one animation step's content; the stopwatch text (and so the
// estimate call) is refreshed once a second.
type frame struct {
	q         []audio.Quality
	main, sub string
	dev       string
	warm      bool
	pulse     float64 // the warming text's opacity
	sw        string
	sec       time.Duration
}

func (f *frame) next(v view) {
	if v.wave.Quality != nil {
		f.q = v.wave.Quality()
	}
	f.dev, f.warm = v.wave.Device, v.wave.Warming
	if f.warm {
		f.pulse = warmPulse(time.Now())
	}
	f.main = v.label
	if clippedRecently(f.q) {
		f.main = "● clipping — too loud"
	}
	f.sub = ""
	if v.started.IsZero() {
		return
	}
	if e := time.Since(v.started).Truncate(time.Second); f.sw == "" || e != f.sec {
		var left time.Duration
		if v.estimate != nil {
			left = v.estimate()
		}
		f.sw, f.sec = stopwatchText(e, left), e
	}
	f.sub = f.sw
	if v.wave.Quality != nil {
		f.sub = "prev: " + f.sw
	}
}

// warmPulse is the warming text's opacity at t: 0.45–1 and back every 1.2 s,
// so a still overlay does not read as a hung one.
func warmPulse(t time.Time) float64 {
	const period = 1200 * time.Millisecond
	ph := float64(t.UnixNano()%int64(period)) / float64(period)
	return 0.725 + 0.275*math.Cos(2*math.Pi*ph)
}

// render updates the labels, resizing the panel when the stopwatch line comes
// or goes, swaps the waveform and the warming text, and redraws the waveform
// when wave is set.
func (o *Overlay) render(f frame, wave bool) {
	if f.main != o.curMain {
		setText(o.text, f.main)
		o.curMain = f.main
	}
	if f.dev != o.curDev {
		setText(o.dev, f.dev)
		o.curDev = f.dev
	}
	if f.sub != o.curSub {
		resize := (f.sub == "") != (o.curSub == "")
		setText(o.sub, f.sub)
		o.curSub = f.sub
		if resize {
			o.place()
		}
	}
	send(class("CATransaction"), "begin")
	send(class("CATransaction"), "setDisableActions:", true)
	if f.warm != o.curWarm {
		send(o.warm, "setHidden:", !f.warm)
		for _, b := range o.bars {
			send(b, "setHidden:", f.warm)
		}
		o.curWarm = f.warm
	}
	if f.warm {
		send(o.warm, "setOpacity:", float32(f.pulse))
	} else if wave {
		o.draw(f.q) // in the same transaction as the bars' unhiding: no stale frame
	}
	send(class("CATransaction"), "commit")
}

func setText(layer objc.ID, s string) {
	send(class("CATransaction"), "begin")
	send(class("CATransaction"), "setDisableActions:", true)
	send(layer, "setString:", nsString(s))
	send(class("CATransaction"), "commit")
}

// draw renders the last ovBars*2 blocks as mirrored bars (dBFS, floor -60),
// each bar in its quality colour's layer.
func (o *Overlay) draw(q []audio.Quality) {
	w, h := ovW-24, waveH
	step := w / ovBars
	var paths [nColours]uintptr
	for c := range paths {
		paths[c] = cgPathCreateMutable()
	}
	for i, b := range barsFor(q, ovBars, ovFloorDB) {
		amp := (b.level - ovFloorDB) / -ovFloorDB
		amp = min(max(amp, 0.03), 1)
		bh := amp * h
		cgPathAddRect(paths[b.colour], 0, CGRect{float64(i)*step + step*0.2, (h - bh) / 2, step * 0.6, bh})
	}
	send(class("CATransaction"), "begin")
	send(class("CATransaction"), "setDisableActions:", true)
	for c, p := range paths {
		send(o.bars[c], "setPath:", p)
	}
	send(class("CATransaction"), "commit")
	for _, p := range paths {
		cgPathRelease(p)
	}
}
