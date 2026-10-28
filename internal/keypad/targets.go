package keypad

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
)

// Group is a Telegram group the knob can pick, with its forum topics.
type Group struct {
	Chat   int64   `json:"chat"`
	Label  string  `json:"label"`
	Topics []Topic `json:"topics,omitempty"`
}

// Topic is one forum topic of a Group.
type Topic struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
	Alias string `json:"alias,omitempty"` // replaces the group/topic display labels
}

// Pick is the picked group and topic. Topic is 0 and TopicLabel "" for a
// group with no topics listed.
type Pick struct {
	Chat       int64
	Topic      int64
	Group      string // label
	TopicLabel string
	Locked     bool
}

// targetsFile is keypad.json: the targets, then the pick and the lock by
// id, so a reordered or refreshed list never moves them.
type targetsFile struct {
	Groups []Group `json:"groups"`
	Chat   int64   `json:"chat,omitempty"`  // the picked group
	Topic  int64   `json:"topic,omitempty"` // the picked topic; 0 when the group lists none
	Locked bool    `json:"locked,omitempty"`
}

// Targets is the knob's pick and key 4's lock, kept in one file so they
// survive Mega Voice and keypad restarts. Safe for concurrent use.
type Targets struct {
	path   string
	mu     sync.Mutex
	groups []Group
	g, t   int // the pick, as indexes into groups and groups[g].Topics
	locked bool
}

// LoadTargets reads the file at path. A missing file is no targets. An
// unreadable or corrupt one is no targets too, with the error returned for
// the log; with no targets nothing changes, so the file stays as it is until
// fixed by hand. The Targets is usable either way.
func LoadTargets(path string) (*Targets, error) {
	t := &Targets{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	var f targetsFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		return t, err
	}
	t.resolve(f)
	return t, nil
}

// resolve finds the saved pick in the list by id. A pick no longer listed
// falls back to the first topic of its group while the group is listed, else
// to the first group; a lock on it is dropped, never carried to another
// target.
func (t *Targets) resolve(f targetsFile) {
	t.groups = f.Groups
	if len(t.groups) == 0 {
		if f.Locked {
			log.Printf("keypad: %s lists no groups; lock dropped", t.path)
		}
		return
	}
	t.g, t.t = slices.IndexFunc(t.groups, func(g Group) bool { return g.Chat == f.Chat }), -1
	if t.g >= 0 {
		if topics := t.groups[t.g].Topics; len(topics) > 0 {
			t.t = slices.IndexFunc(topics, func(tp Topic) bool { return tp.ID == f.Topic })
		} else if f.Topic == 0 {
			t.t = 0
		}
	}
	if t.g >= 0 && t.t >= 0 {
		t.locked = f.Locked
		return
	}
	if f.Locked {
		log.Printf("keypad: %s: locked target chat %d topic %d is no longer listed; lock dropped", t.path, f.Chat, f.Topic)
	}
	t.g, t.t = max(t.g, 0), 0
}

// Turn moves the pick n steps, wrapping around: the group, or with held the
// topic within the group. A new group starts at its first topic. While
// locked it does nothing — the target stays fixed until key 4 unlocks it.
// It reports whether the pick moved.
func (t *Targets) Turn(n int, held bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locked || len(t.groups) == 0 || n == 0 {
		return false
	}
	if held {
		k := len(t.groups[t.g].Topics)
		if k < 2 {
			return false
		}
		t.t = wrap(t.t+n, k)
	} else {
		if len(t.groups) < 2 {
			return false
		}
		t.g, t.t = wrap(t.g+n, len(t.groups)), 0
	}
	t.save()
	return true
}

func wrap(i, n int) int { return (i%n + n) % n }

// ToggleLock locks key 1 to the pick, or unlocks it; ok is false, and
// nothing changes, when there are no targets to lock to.
func (t *Targets) ToggleLock() (locked, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.groups) == 0 {
		return false, false
	}
	t.locked = !t.locked
	t.save()
	return t.locked, true
}

// Current returns the pick; ok is false when there are no targets.
func (t *Targets) Current() (p Pick, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pick()
}

func (t *Targets) pick() (p Pick, ok bool) {
	if len(t.groups) == 0 {
		return Pick{}, false
	}
	g := t.groups[t.g]
	p = Pick{Chat: g.Chat, Group: g.Label, Locked: t.locked}
	if len(g.Topics) > 0 {
		tp := g.Topics[t.t]
		p.Topic, p.TopicLabel = tp.ID, tp.Label
		if tp.Alias != "" {
			p.Group, p.TopicLabel = tp.Alias, ""
		}
	}
	return p, true
}

// Locked returns the pick when key 1 is locked to it.
func (t *Targets) Locked() (Pick, bool) {
	p, ok := t.Current()
	return p, ok && p.Locked
}

// Fill writes the pick and the lock into a snapshot, for Link.Update.
func (t *Targets) Fill(s *Snapshot) {
	p, _ := t.Current()
	s.Lock, s.Group, s.Topic = p.Locked, p.Group, p.TopicLabel
	s.Chat, s.TopicID = "", 0
	if p.Locked {
		s.Chat, s.TopicID = strconv.FormatInt(p.Chat, 10), p.Topic
	}
}

// save writes the file atomically: a temporary file beside it, synced, then
// renamed over it. A failure keeps the state in memory; the next change
// writes the whole state again. t.mu is held.
func (t *Targets) save() {
	p, _ := t.pick()
	f := targetsFile{Groups: t.groups, Chat: p.Chat, Topic: p.Topic, Locked: t.locked}
	if err := writeAtomic(t.path, f); err != nil {
		log.Printf("keypad: save %s: %v; kept in memory, retried on the next change", t.path, err)
	}
}

func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".keypad-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
