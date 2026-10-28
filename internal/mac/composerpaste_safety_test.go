package mac

import (
	"errors"
	"testing"
	"time"
)

// The original field loses unrelated text while Cmd-V lands in another field.
func TestReviewRejectsDeletionAroundExistingTranscript(t *testing.T) {
	for _, tc := range []struct{ before, after string }{
		{"prefix text suffix", "prefix text"},
		{"prefix old text suffix", "prefix text suffix"},
		{"text prefix old text suffix", "text prefix text suffix"},
	} {
		t.Run(tc.before, func(t *testing.T) {
			f := newComposerFake()
			f.fields[1] = tc.before
			ops := f.ops()
			ops.post = func() error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.posts++
				f.fields[2] += f.clipboard
				f.fields[1] = tc.after
				return nil
			}
			b := &clipboardBoard{}
			p, pasteErr := pasteComposer(b, ops, "text", 30*time.Millisecond)
			defer b.clear(p)
			defer p.Release()
			submitErr := p.Submit()
			value, confirmed := p.Value()
			t.Logf("before=%q original=%q other=%q paste=%v submit=%v confirmed=%v value=%q enters=%d", tc.before, f.fields[1], f.fields[2], pasteErr, submitErr, confirmed, value, f.enters)
			if !errors.Is(pasteErr, ErrPasteUnconfirmed) || !errors.Is(submitErr, ErrPasteUnconfirmed) || f.enters != 0 {
				t.Fatal("unrelated deletion acknowledged a paste into the other composer and enabled Return")
			}
		})
	}
}

func TestReviewSubmitRejectsAnotherCompatibleDraft(t *testing.T) {
	f := newComposerFake()
	f.fields[1] = "prefix old text suffix"
	b := &clipboardBoard{}
	p, err := pasteComposer(b, f.ops(), "text", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer b.clear(p)
	defer p.Release()
	firstValue, _ := p.Value()
	// A later edit replaces the confirmed draft with a different, old-text draft.
	f.fields[1] = "prefix old text"
	submitErr := p.Submit()
	value, confirmed := p.Value()
	t.Logf("confirmed-first=%q original=%q submit=%v confirmed=%v value=%q enters=%d", firstValue, f.fields[1], submitErr, confirmed, value, f.enters)
	if !errors.Is(submitErr, ErrPasteUnconfirmed) || f.enters != 0 {
		t.Fatal("Submit accepted a different draft by matching it to the pre-paste value")
	}
}
