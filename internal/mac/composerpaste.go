package mac

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

var (
	ErrPasteUnconfirmed = errors.New("original composer did not confirm the paste")
	ErrClipboardPending = errors.New("previous paste is unconfirmed; its text remains on the clipboard")
)

// composerOps is the whole app delivery seam. Read always reads the captured
// composer, regardless of the focused element. Prepare finishes all clipboard
// and keyboard-layout work before Activate/Focused/Post.
type composerOps struct {
	prepare   func(string) error
	activate  func() error
	focused   func() bool
	read      func(context.Context) (string, bool)
	selection func(context.Context) (int, int, bool) // UTF-16 location and length
	post      func() error
	enter     func() error
	release   func()
}

// PasteReceipt retains the captured composer until both its owner and the
// clipboard reservation release it. The reservation lasts at most the
// configured bound, even when consumption cannot be observed.
type PasteReceipt struct {
	mu                               sync.Mutex
	ops                              composerOps
	text, before, value              string
	expected                         string
	replacementKnown                 bool
	postedAt                         time.Time
	readable, posted, confirmed      bool
	pending, ownerReleased, released bool
	expires                          time.Time
	timer                            *time.Timer
}

func (p *PasteReceipt) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ownerReleased = true
	p.releaseLocked()
}
func (p *PasteReceipt) releaseLocked() {
	if p.ownerReleased && !p.pending && !p.released {
		p.released = true
		if p.ops.release != nil {
			p.ops.release()
		}
	}
}
func (p *PasteReceipt) unreserve() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = false
	p.releaseLocked()
}
func (p *PasteReceipt) confirms(value string) bool {
	if !p.readable || value == p.before {
		return false
	}
	if p.replacementKnown {
		return value == p.expected
	}
	return value == p.text || strings.Count(value, p.text) > strings.Count(p.before, p.text)
}
func (p *PasteReceipt) acknowledge(ctx context.Context) bool {
	value, ok := p.ops.read(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.posted && ok && p.confirms(value) {
		p.confirmed = true
		p.value = value
		return true
	}
	return false
}

// Value is the captured composer's last verified content, before any Enter.
func (p *PasteReceipt) Value() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value, p.confirmed
}

// Submit uses content confirmation when available. An unreadable composer
// keeps the paste-and-Enter path, with a fresh original-element focus check
// and no activation that could conceal a focus move after the paste.
func (p *PasteReceipt) Submit() error {
	p.mu.Lock()
	confirmed, readable, posted, postedAt := p.confirmed, p.readable, p.posted, p.postedAt
	p.mu.Unlock()
	if !posted || readable && !confirmed {
		return ErrPasteUnconfirmed
	}
	if readable {
		if err := p.ops.activate(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		if !p.acknowledge(ctx) {
			return ErrPasteUnconfirmed
		}
	} else {
		if remaining := time.Until(postedAt.Add(unreadableReturnDelay)); remaining > 0 {
			time.Sleep(remaining)
		}
	}
	if !p.ops.focused() {
		return ErrPasteUnconfirmed
	}
	return p.ops.enter()
}

const clipboardReservation = 2 * time.Second

// Unreadable composers get at least the app delivery's 100 ms paste wait
// plus its 150 ms send settle before Return.
const unreadableReturnDelay = 250 * time.Millisecond

type clipboardBoard struct {
	mu      sync.Mutex
	pending *PasteReceipt
	limit   time.Duration // zero uses clipboardReservation
	onMain  func() bool   // main-thread callers never wait for another clipboard operation
}

// copy waits for confirmed consumption or reservation expiry. Unknown
// consumption never requires a manual clipboard change to make progress.
func (b *clipboardBoard) copy(next *PasteReceipt, write func() error) error {
	for {
		if !b.mu.TryLock() {
			if b.onMain != nil && b.onMain() {
				return ErrClipboardPending
			}
			b.mu.Lock()
		}
		if p := b.pending; p != nil {
			p.mu.Lock()
			canRead := p.readable && p.posted
			p.mu.Unlock()
			consumed := false
			if canRead {
				ctx, cancel := context.WithTimeout(context.Background(), min(250*time.Millisecond, max(time.Until(p.expires), time.Millisecond)))
				consumed = p.acknowledge(ctx)
				cancel()
			}
			if !consumed && time.Now().Before(p.expires) {
				remaining := time.Until(p.expires)
				b.mu.Unlock()
				if b.onMain != nil && b.onMain() {
					return ErrClipboardPending
				}
				time.Sleep(min(10*time.Millisecond, max(remaining, time.Millisecond)))
				continue
			}
			b.clearLocked(p)
		}
		if err := write(); err != nil {
			b.mu.Unlock()
			return err
		}
		if next != nil {
			limit := b.limit
			if limit <= 0 {
				limit = clipboardReservation
			}
			next.mu.Lock()
			next.pending = true
			next.expires = time.Now().Add(limit)
			next.mu.Unlock()
			b.pending = next
			next.timer = time.AfterFunc(limit, func() { b.clear(next) })
		}
		b.mu.Unlock()
		return nil
	}
}
func (b *clipboardBoard) clearLocked(p *PasteReceipt) {
	if b.pending == p {
		b.pending = nil
		if p.timer != nil {
			p.timer.Stop()
		}
		p.unreserve()
	}
}
func (b *clipboardBoard) clear(p *PasteReceipt) { b.mu.Lock(); defer b.mu.Unlock(); b.clearLocked(p) }

// pasteComposer holds the delivery turn until the original composer confirms
// insertion or the readback bound expires. Unreadable fields use focus checks
// and the bounded clipboard reservation without claiming a consumption ACK.
func pasteComposer(b *clipboardBoard, ops composerOps, text string, wait time.Duration) (p *PasteReceipt, err error) {
	p = &PasteReceipt{ops: ops, text: text}
	defer func() {
		if err != nil {
			p.Release()
		}
	}()
	if text == "" {
		return p, ErrPasteUnconfirmed
	}
	if err = b.copy(p, func() error { return ops.prepare(text) }); err != nil {
		return p, err
	}
	posted := false
	defer func() {
		if !posted {
			b.clear(p)
		}
	}()
	if err = ops.activate(); err != nil {
		return p, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	before, readable := ops.read(ctx)
	p.mu.Lock()
	p.before, p.readable = before, readable
	p.mu.Unlock()
	if readable && ops.selection != nil {
		start, length, ok := ops.selection(ctx)
		if ok {
			expected, known := selectedReplacement(before, text, start, length)
			p.mu.Lock()
			p.expected, p.replacementKnown = expected, known
			p.mu.Unlock()
		}
	}
	p.mu.Lock()
	reserved := p.pending
	p.mu.Unlock()
	if !reserved {
		return p, ErrClipboardPending
	}
	if !ops.focused() {
		return p, ErrPasteUnconfirmed
	}
	if err = ops.post(); err != nil {
		return p, err
	}
	posted = true
	p.mu.Lock()
	p.posted = true
	p.postedAt = time.Now()
	p.mu.Unlock()
	if !readable {
		return p, nil
	}
	for {
		if p.acknowledge(ctx) {
			b.clear(p)
			return p, nil
		}
		select {
		case <-ctx.Done():
			return p, ErrPasteUnconfirmed
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// selectedReplacement applies an AX UTF-16 selection without splitting a
// surrogate pair. Invalid or unavailable selection context stays unconfirmed.
func selectedReplacement(before, text string, start, length int) (string, bool) {
	units := utf16.Encode([]rune(before))
	if start < 0 || start > len(units) || length < 0 || length > len(units)-start {
		return "", false
	}
	boundary := func(i int) bool {
		return i == 0 || i == len(units) || !(units[i-1] >= 0xd800 && units[i-1] <= 0xdbff && units[i] >= 0xdc00 && units[i] <= 0xdfff)
	}
	if !boundary(start) || !boundary(start+length) {
		return "", false
	}
	return string(utf16.Decode(units[:start])) + text + string(utf16.Decode(units[start+length:])), true
}
