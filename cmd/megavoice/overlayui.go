//go:build darwin

package main

import (
	"slices"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// overlay is what overlayUI draws on: *mac.Overlay.
type overlay interface {
	Show(label string, w mac.Wave)
	Stopwatch(started time.Time, estimate func() time.Duration)
	Hide()
}

// overlayUI draws the controller's view on the overlay. A message lies over
// whatever the view shows — the waveform of a take that is still recording
// keeps moving under it: a flash for 1.5 s, an alert for 6 s. A flash
// replaces a flash; nothing cuts an alert short, so a failure stays readable.
// A flash that comes during an alert waits for it, the newest such flash
// only; an alert that comes during one waits its turn, unless the same text
// is on the label or waiting already. The no-signal notice is the view's own
// label, not a message: it neither waits for nor cuts short a message, shows
// whenever no message is on the label, and lasts as long as the silence.
type overlayUI struct {
	ov    overlay
	after func(time.Duration, func()) *time.Timer // time.AfterFunc
	mu    sync.Mutex
	v     session.View
	msg   string      // the message on the label; "": the view's own
	alert bool        // msg is an alert
	timer *time.Timer // ends msg
	queue []string    // alerts waiting for msg
	flash string      // the flash waiting for the alerts
}

const (
	flashFor = 1500 * time.Millisecond
	alertFor = 6 * time.Second
)

func (u *overlayUI) Show(v session.View) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.v = v
	u.renderLocked()
}

func (u *overlayUI) Flash(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.alert {
		u.flash = msg
		return
	}
	u.showLocked(msg, false)
}

func (u *overlayUI) Alert(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.alert {
		u.showLocked(msg, true)
		return
	}
	if msg != u.msg && !slices.Contains(u.queue, msg) {
		u.queue = append(u.queue, msg)
	}
}

// showLocked puts msg on the label for its time; then the next waiting alert,
// else the waiting flash, else the view's own label.
func (u *overlayUI) showLocked(msg string, alert bool) {
	if u.timer != nil {
		u.timer.Stop()
	}
	d := flashFor
	if alert {
		d = alertFor
	}
	var t *time.Timer
	t = u.after(d, func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.timer != t {
			return
		}
		u.msg, u.alert, u.timer = "", false, nil
		switch {
		case len(u.queue) > 0:
			next := u.queue[0]
			u.queue = u.queue[1:]
			u.showLocked(next, true)
		case u.flash != "":
			next := u.flash
			u.flash = ""
			u.showLocked(next, false)
		default:
			u.renderLocked()
		}
	})
	u.msg, u.alert, u.timer = msg, alert, t
	u.renderLocked()
}

func (u *overlayUI) renderLocked() {
	v := u.v
	var label string
	switch {
	case u.msg != "":
		label = u.msg
	case v.Quality != nil && v.NoSignal:
		label = session.NoSignal
	case v.Quality != nil && v.Warming:
		label = "◌ 麦克风预热中 — 波形出现再说 · Esc 取消"
	case v.Quality != nil:
		label = "● 录音中 — Shift 粘贴 · Enter 发送 · Esc 取消"
	case v.Started.IsZero():
		u.ov.Hide()
		return
	case v.Send:
		label = "识别中… → 粘贴 + Enter"
	default:
		label = "识别中…（Enter 发送）"
	}
	u.ov.Show(label, mac.Wave{Quality: v.Quality, Device: v.Device, Warming: v.Warming})
	u.ov.Stopwatch(v.Started, v.Estimate)
}
