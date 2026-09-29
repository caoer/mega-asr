package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/store"
)

// The refusals of Resend and Retranscribe: each comes back before a line is
// appended. The Chinese ones are shown on the page as they are.
var (
	ErrNoTake         = errors.New("no such take")
	ErrRecording      = errors.New("还在录音")
	ErrTranscribing   = errors.New("还在识别")
	ErrResending      = errors.New("这条正在重发")
	ErrRetranscribing = errors.New("这条正在重新识别")
	ErrNoText         = errors.New("no text")
	ErrNotNamed       = errors.New("the config does not name this cloud engine")
)

// ErrNotRecorded is Resend's and Retranscribe's error when the line they
// return could not be appended to the take's record: the delivery or the
// decode ran, and the record does not say so.
var ErrNotRecorded = errors.New("the record could not be written")

// takeName is a take's name: YYYYmmdd-HHMMSS, with -N for a second take in
// the same second.
var takeName = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9]+)?$`)

// Base is the path without extension of the take named id in the store, when
// the store holds a WAV, a text or a record of that name.
func (c *Controller) Base(id string) (string, error) {
	if !takeName.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not a take's name", ErrNoTake, id)
	}
	base := filepath.Join(c.Store.Dir, id)
	for _, ext := range []string{".events.jsonl", ".wav", ".txt"} {
		if _, err := os.Stat(base + ext); err == nil {
			return base, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoTake, id)
}

// onItsWay refuses a take that records or transcribes in this process.
// Guarded by c.mu.
func (c *Controller) onItsWayLocked(base string) error {
	if c.rec != nil && c.rec.base == base {
		return ErrRecording
	}
	for _, t := range c.finished {
		if t.base == base {
			return ErrTranscribing
		}
	}
	return nil
}

// Resend is one delivery of a kept take's text, asked for by a command, the
// hotkey or the page.
type Resend struct {
	ID string // the take's name
	To Target // where the text goes; Resend releases it
	// Send presses Enter after the paste; off, the Enter is the user's.
	Send bool
	// Text picks the text: "" or "delivered" (the take's .txt, or the partial
	// text its own delivery carried), "raw" (.raw.txt), an engine's name (its
	// answer in .compare.json), or "retranscription N" (the answer of the
	// take's Nth retranscribe line).
	Text   string
	Via    string        // cli, page or hotkey: the deliver line's via
	Caller *store.Caller // the process that asked, for a command
	// Where names the target in the overlay's flash; "" is To's String.
	Where string
	// Wait is how long to wait for a delivery under way; 0 is 10 s.
	Wait time.Duration
}

// Resend delivers a kept take's text to q.To, under the delivery lock, and
// appends a deliver line with the next n. It returns an error, and appends
// nothing, when the take is unknown, still records or transcribes, is being
// resent already, has no text of the kind asked for, or the lock stays held
// for q.Wait. Otherwise it returns the line it appended: a delivery that
// failed has OK false and its Err, leaves the text on the clipboard, and is
// followed by hold deliver_failed when the take has never reached a target
// (one that has keeps its state). The target is read back after the lock
// is released (readBack). A line that could not be appended comes back with
// ErrNotRecorded. While it runs the controller is Busy.
func (c *Controller) Resend(q Resend) (store.Event, error) {
	defer release(q.To)
	base, err := c.Base(q.ID)
	if err != nil {
		return store.Event{}, err
	}
	c.mu.Lock()
	if err := c.closedLocked(); err != nil {
		c.mu.Unlock()
		return store.Event{}, err
	}
	if err := c.onItsWayLocked(base); err != nil {
		c.mu.Unlock()
		return store.Event{}, err
	}
	if c.resending[base] {
		c.mu.Unlock()
		return store.Event{}, ErrResending
	}
	if c.resending == nil {
		c.resending = map[string]bool{}
	}
	c.resending[base] = true
	c.inflight.Add(1)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.resending, base)
		c.inflight.Add(-1)
		c.mu.Unlock()
	}()

	evs, _ := store.Read(base)
	text, source, err := textOf(base, evs, q.Text)
	if err != nil {
		return store.Event{}, err
	}
	wait := q.Wait
	if wait == 0 {
		wait = resendWait
	}
	unlock, err := c.LockDelivery(wait)
	if err != nil {
		return store.Event{}, err
	}
	defer unlock()

	evs, _ = store.Read(base)
	n, reached := 1, false
	for _, e := range evs {
		if e.Ev == "deliver" {
			n++
			reached = reached || e.OK
		}
	}
	de := store.Event{V: 1, At: time.Now(), Ev: "deliver", N: n, Via: q.Via, Caller: q.Caller, Target: StoreTarget(q.To),
		Chars: chars(text), Submit: "none", Source: source}
	if txt, err := os.ReadFile(base + ".txt"); err != nil || strings.TrimSuffix(string(txt), "\n") != text {
		de.Text = text
	}
	where := q.Where
	if where == "" {
		where = q.To.String()
	}
	name := filepath.Base(base)
	if err := c.Deliver.Deliver(q.To, text); err != nil {
		log.Printf("session: resend %s to %s: %v; text left on the clipboard", name, q.To, err)
		de.At, de.Err = time.Now(), err.Error()
		rerr := c.Record(base, de)
		msg := MsgResendFailed
		if !reached {
			msg = c.deliverFailedMsg()
			if rerr == nil {
				rerr = c.Record(base, store.Event{Ev: "hold", Why: "deliver_failed"})
			}
		}
		c.UI.Alert(msg)
		return de, unrecorded(rerr, "the delivery failed: "+de.Err)
	}
	de.OK = true
	verb := "已粘贴"
	switch {
	case copyOnly(q.To):
		verb = "已复制"
	case q.Send:
		de.Submit = "ok"
		if err := c.Deliver.Submit(q.To); err != nil {
			log.Printf("session: resend %s: Enter to %s: %v", name, q.To, err)
			de.Submit, de.Err = "err", err.Error()
		} else {
			verb = "已发送"
		}
	}
	de.At = time.Now()
	rerr := c.Record(base, de)
	c.readBack(base, n, q.To, text)
	log.Printf("session: resent %s to %s (%s, %d characters, n %d)", name, q.To, source, de.Chars, n)
	if de.Err != "" {
		c.UI.Alert(MsgNotSent + de.Err)
	} else {
		c.UI.Flash(resentMsg(verb, evs, text, where))
	}
	return de, unrecorded(rerr, "the text went to "+where)
}

// unrecorded is ErrNotRecorded for an append that failed with err, saying
// what happened that the record lacks; nil when err is nil.
func unrecorded(err error, what string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v; %s", ErrNotRecorded, err, what)
}

// resendWait is a resend's wait for a delivery under way when its Wait is 0.
var resendWait = 10 * time.Second

// copyOnly reports whether t takes a copy and nothing else: the clipboard,
// where no key is pressed.
func copyOnly(t Target) bool {
	c, ok := t.(interface{ CopyOnly() bool })
	return ok && c.CopyOnly()
}

// resentMsg is the flash of a resend: `已粘贴 2:17「…」→ where`.
func resentMsg(verb string, evs []store.Event, text, where string) string {
	msg := verb
	for _, e := range evs {
		if e.Ev == "stop" && e.DurS > 0 {
			msg += " " + Clock(time.Duration(e.DurS*float64(time.Second)))
		}
	}
	head := []rune(strings.Join(strings.Fields(text), " "))
	if len(head) > 10 {
		head = append(head[:10], '…')
	}
	return fmt.Sprintf("%s「%s」→ %s", msg, string(head), where)
}

// textOf is the take's text of the kind named (Resend's Text) and the
// deliver line's source for it.
func textOf(base string, evs []store.Event, kind string) (text, source string, err error) {
	read := func(ext string) (string, error) {
		b, err := os.ReadFile(base + ext)
		if err != nil {
			return "", err
		}
		return strings.TrimSuffix(string(b), "\n"), nil
	}
	switch {
	case kind == "" || kind == "delivered":
		source = "delivered"
		if text, err = read(".txt"); err != nil {
			// a take whose chunks failed keeps no .txt; its own delivery
			// carried the partial text in its line
			for _, e := range evs {
				if e.Ev == "deliver" && e.Via == "auto" && e.Text != "" {
					text, err = e.Text, nil
				}
			}
		}
	case kind == "raw":
		source = "raw"
		text, err = read(".raw.txt")
	case strings.HasPrefix(kind, "retranscription"):
		n, perr := strconv.Atoi(strings.TrimLeft(strings.TrimPrefix(kind, "retranscription"), " :"))
		if perr != nil || n < 1 {
			return "", "", fmt.Errorf("text %q: want retranscription N", kind)
		}
		source = fmt.Sprintf("retranscription %d", n)
		err = fmt.Errorf("%w: no retranscription %d", ErrNoText, n)
		for _, e := range evs {
			if e.Ev == "retranscribe" && e.N == n {
				text, err = e.Text, nil
				if e.Err != "" && e.Text == "" {
					err = fmt.Errorf("%w: retranscription %d failed: %s", ErrNoText, n, e.Err)
				}
			}
		}
	default:
		source = kind
		// the compare record (compare.Record, which imports this package):
		// each engine's answer by name
		var rec struct {
			Engines map[string]struct {
				Text  string `json:"text"`
				Error string `json:"error"`
			} `json:"engines"`
		}
		b, rerr := os.ReadFile(base + ".compare.json")
		if rerr == nil {
			rerr = json.Unmarshal(b, &rec)
		}
		e, ok := rec.Engines[kind]
		switch {
		case rerr != nil || !ok:
			err = fmt.Errorf("%w: no answer of %s in the compare record", ErrNoText, kind)
		case e.Error != "" && e.Text == "":
			err = fmt.Errorf("%w: %s failed: %s", ErrNoText, kind, e.Error)
		default:
			text = e.Text
		}
	}
	if err == nil && strings.TrimSpace(text) == "" {
		err = fmt.Errorf("%w: the %s text is empty", ErrNoText, source)
	}
	if err != nil && !errors.Is(err, ErrNoText) {
		err = fmt.Errorf("%w: %v", ErrNoText, err)
	}
	return text, source, err
}

// Engine is an engine a take can be decoded again by.
type Engine struct {
	Name, Model string
	// Cloud: the take's audio leaves this machine. Named: the config chose
	// the engine (asr.engine, or compare.engines with compare.on); a cloud
	// engine the config does not name is refused.
	Cloud, Named bool
	// File decodes a WAV; nil is the controller's own chunk ASR, which then
	// takes the take through its queue behind every live take.
	File func(ctx context.Context, wav string) (string, error)
}

// Retranscribe decodes a kept take's audio again with e, the audio its text
// came from (decodeAgain), and appends the answer as a retranscribe line
// with the next n and that audio's track: its raw and post-chain text, or the
// engine's error, ctx's when it ends first. No stored text changes:
// .txt, .raw.txt and .compare.json stay as they are. It returns an error,
// and appends nothing, when the take is unknown, has no WAV, still records
// or transcribes, is being re-transcribed already, or e is a cloud engine
// the config does not name. A line that could not be appended comes back
// with ErrNotRecorded. While it runs the controller is Busy.
func (c *Controller) Retranscribe(ctx context.Context, id string, e Engine) (store.Event, error) {
	base, err := c.Base(id)
	if err != nil {
		return store.Event{}, err
	}
	if e.Cloud && !e.Named {
		return store.Event{}, fmt.Errorf("%w: %s sends the audio off this machine; name it in asr.engine, or in compare.engines with compare.on", ErrNotNamed, e.Name)
	}
	wav := base + ".wav"
	if _, err := os.Stat(wav); err != nil {
		return store.Event{}, fmt.Errorf("%w: %s has no audio", ErrNoTake, id)
	}
	c.mu.Lock()
	if err := c.closedLocked(); err != nil {
		c.mu.Unlock()
		return store.Event{}, err
	}
	if err := c.onItsWayLocked(base); err != nil {
		c.mu.Unlock()
		return store.Event{}, err
	}
	if c.retranscribing[base] {
		c.mu.Unlock()
		return store.Event{}, ErrRetranscribing
	}
	if c.retranscribing == nil {
		c.retranscribing = map[string]bool{}
	}
	c.retranscribing[base] = true
	c.inflight.Add(1)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.retranscribing, base)
		c.inflight.Add(-1)
		c.mu.Unlock()
	}()

	t0 := time.Now()
	raw, track, err := c.decodeAgain(ctx, base, e)
	re := store.Event{Ev: "retranscribe", Engine: e.Name, Model: e.Model, LatencyMS: time.Since(t0).Milliseconds(), Audio: track}
	if raw != "" {
		re.Raw, re.Text = raw, c.Post.Apply(raw)
	}
	if err != nil {
		re.Err = err.Error()
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	evs, _ := store.Read(base)
	re.V, re.At, re.N = 1, time.Now(), 1
	for _, x := range evs {
		if x.Ev == "retranscribe" {
			re.N++
		}
	}
	rerr := c.Record(base, re)
	log.Printf("session: retranscribed %s with %s (n %d, %d ms, %d characters, err %v)", id, e.Name, re.N, re.LatencyMS, chars(re.Text), err)
	return re, unrecorded(rerr, "the answer is not kept")
}

// decodeAgain decodes a kept take's audio with e: the audio its text came
// from, and its track. A take whose last text line says audio backup is
// rebuilt as Recover rebuilds it (withBackup), its main track with the
// backup's audio in the main track's dead spans; the backup track alone
// lacks what only the main track heard. A take whose backup stands in
// nowhere now is its main track.
func (c *Controller) decodeAgain(ctx context.Context, base string, e Engine) (raw, track string, err error) {
	evs, _ := store.Read(base)
	wav, track := base+".wav", "main"
	var s []int16
	if _, from := store.AudioOf(base, evs); from == "backup" || e.File == nil {
		if s, err = audio.ReadWAV(wav); err != nil {
			return "", track, err
		}
		if from == "backup" {
			if out, used, _, err := c.withBackup(base, s); err == nil && used > 0 {
				s, track = out, "backup"
			}
		}
	}
	dir := ""
	if e.File == nil || track == "backup" {
		if err := os.MkdirAll(c.chunkDir(), 0o755); err != nil {
			return "", track, err
		}
		if dir, err = os.MkdirTemp(c.chunkDir(), filepath.Base(base)+".retranscribe-"); err != nil {
			return "", track, err
		}
		defer os.RemoveAll(dir)
	}
	if e.File == nil {
		raw, err = c.decodeQueued(ctx, base, dir, s)
		return raw, track, err
	}
	if track == "backup" {
		wav = filepath.Join(dir, filepath.Base(base)+".wav")
		if err := audio.SaveWAV(wav, s); err != nil {
			return "", track, err
		}
	}
	raw, err = e.File(ctx, wav)
	return raw, track, err
}

// decodeQueued decodes a kept take's audio s with the chunk ASR through the
// background queue, cut as a recovered take is, its chunks' files in dir,
// behind every live take; its chunks fail once ctx is done.
func (c *Controller) decodeQueued(ctx context.Context, base, dir string, s []int16) (string, error) {
	t := &take{chunker: c.newChunker(), base: base, dir: dir, ctx: ctx}
	c.cut(t, s, true)
	stop := context.AfterFunc(ctx, func() { c.drop(t, ctx.Err()) })
	raw, failed := c.stitch(t)
	stop()
	if failed > 0 {
		var first error
		for _, ck := range t.chunks {
			if ck.err != nil {
				first = ck.err
				break
			}
		}
		return raw, fmt.Errorf("%d of %d chunks failed: %v", failed, len(t.chunks), first)
	}
	return raw, nil
}
