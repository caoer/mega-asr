package session

import (
	"cmp"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/store"
)

// MsgNothingToResend is the hotkey's flash when no take qualifies.
const MsgNothingToResend = "10 分钟内没有可重贴的录音"

const (
	// hotkeyWithin: the hotkey reaches the takes started this long ago or
	// later.
	hotkeyWithin = 10 * time.Minute
	// hotkeyMinCancelled: a cancelled take the hotkey pastes is at least this
	// long, so a false start is never what it finds.
	hotkeyMinCancelled = time.Second
)

// ErrHotkeyNoText is the hotkey's answer when the newest cancelled take
// has no text: its transcription failed, or it ended before its text was
// saved.
var ErrHotkeyNoText = errors.New(MsgASRFailed)

// ErrHotkeySilent is the hotkey's answer when the newest cancelled take is
// silent: its transcription heard no words.
var ErrHotkeySilent = errors.New("没有识别到文字")

// HotkeyTake is the take the hotkey pastes at now: the newest undelivered
// take with text among those started in the last 10 minutes, else the
// newest cancelled take of 1 s or more among them; "" when there is none.
// An undelivered take whose paste an exit cut (delivery_cut: it most likely
// landed) is never the one, nor one the user dismissed. A take still on its
// way (in the finished queue, cancelled ones too, or a recovery decoding the
// text of a take among those) ends the search with ErrTranscribing before
// any take is looked at:
// right after Esc the hotkey is for the take just cancelled, never an older
// one pasted in its place. The search for a cancelled take stops at the
// newest one and never walks past it to an older take: a cancelled take
// that heard no words ends it with ErrHotkeySilent, one without text with
// ErrHotkeyNoText.
func (c *Controller) HotkeyTake(now time.Time) (string, error) {
	since := now.Add(-hotkeyWithin)
	c.mu.Lock()
	// read first: once both are empty, every take's lines are on disk; a
	// recovery of a take older than the hotkey reaches never holds it
	onItsWay := len(c.finished) > 0
	for _, at := range c.recovering {
		onItsWay = onItsWay || !at.Before(since)
	}
	c.mu.Unlock()
	if onItsWay {
		return "", ErrTranscribing
	}
	recs, err := c.Store.Recent(0, since)
	if err != nil {
		log.Printf("session: hotkey: %v", err)
		return "", nil
	}
	for _, r := range recs {
		if st := store.State(r.Events); st.State != "undelivered" || st.Why == "delivery_cut" || st.Dismissed {
			continue
		}
		if _, _, err := textOf(r.Base, r.Events, ""); err == nil {
			return filepath.Base(r.Base), nil
		}
	}
	for _, r := range recs {
		if store.State(r.Events).State != "cancelled" || stopLength(r.Events) < hotkeyMinCancelled {
			continue
		}
		if _, _, err := textOf(r.Base, r.Events, ""); err != nil {
			if b, rerr := os.ReadFile(r.Base + ".txt"); rerr == nil && strings.TrimSpace(string(b)) == "" {
				return "", ErrHotkeySilent // its text was saved, and is empty
			}
			return "", ErrHotkeyNoText
		}
		return filepath.Base(r.Base), nil
	}
	return "", nil
}

// stopLength is a take's length as its stop line gives it; 0 without one.
func stopLength(evs []store.Event) time.Duration {
	for _, e := range evs {
		if e.Ev == "stop" {
			return time.Duration(e.DurS * float64(time.Second))
		}
	}
	return 0
}

// Hotkey is the tap key held with Esc pressed. With megavoice idle it
// pastes HotkeyTake's text into what has focus now, without Enter, as a
// resend via hotkey, and flashes `已粘贴 2:17「…」→ 当前光标`; while a take
// records or is on its way it only flashes why not. The paste runs on its
// own goroutine, so the caller (the tap's dispatcher) is free for the next
// key at once; a chord while one hotkey paste runs flashes 这条正在重发.
func (c *Controller) Hotkey() {
	switch c.State() {
	case Recording:
		c.UI.Flash(ErrRecording.Error())
		return
	case Transcribing, Delivering:
		c.UI.Flash(ErrTranscribing.Error())
		return
	}
	id, err := c.HotkeyTake(c.now())
	switch {
	case err != nil:
		c.UI.Flash(err.Error())
		return
	case id == "":
		c.UI.Flash(MsgNothingToResend)
		return
	case !c.hotkeying.CompareAndSwap(false, true):
		c.UI.Flash(ErrResending.Error())
		return
	}
	c.inflight.Add(1) // Busy from the chord on, so no exit comes between
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.inflight.Add(-1)
		defer c.hotkeying.Store(false)
		c.hotkeyResend(id)
	}()
}

// hotkeyResend is the hotkey's paste of the take id into what has focus.
func (c *Controller) hotkeyResend(id string) {
	if _, err := c.Resend(Resend{ID: id, To: c.Deliver.Capture(), Via: "hotkey", Where: "当前光标"}); err != nil {
		log.Printf("session: hotkey: %s: %v", id, err)
		msg := err.Error()
		switch {
		case errors.Is(err, ErrDeliveryBusy):
			msg = "另一条正在送达"
		case !errors.Is(err, ErrRecording) && !errors.Is(err, ErrTranscribing) && !errors.Is(err, ErrResending):
			msg = "没能重贴 · " + msg
		}
		c.UI.Flash(msg)
	}
}

// hint names the resend hotkey on a flash whose take it reaches: the
// configured tap key with Esc, or the default tap key's (右Option+Esc 重贴)
// when HotkeyKey is empty.
func (c *Controller) hint() string {
	return cmp.Or(c.HotkeyKey, "右Option") + "+Esc 重贴"
}

// becomesUndelivered reports whether appending e to the record at base
// turns the take undelivered from any other state, when a Sound is set.
func (c *Controller) becomesUndelivered(base string, e store.Event) bool {
	if c.Sound == nil || e.Ev != "hold" && e.Ev != "deliver" {
		return false
	}
	evs, _ := store.Read(base)
	return store.State(evs).State != "undelivered" && store.State(append(evs, e)).State == "undelivered"
}
