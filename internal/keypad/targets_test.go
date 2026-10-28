package keypad

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seed = `{
  "groups": [
    {"chat": -1001, "label": "报到群", "topics": [{"id": 1, "label": "General"}, {"id": 77, "label": "ccc-status"}, {"id": 90, "label": "test"}]},
    {"chat": -1002, "label": "蓝蟹的群", "topics": [{"id": 1, "label": "General"}]},
    {"chat": -1003, "label": "安昌的群"}
  ]
}`

func load(t *testing.T, body string) (*Targets, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "megavoice", "keypad.json")
	if body != "" {
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tg, err := LoadTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	return tg, path
}

func pick(t *testing.T, tg *Targets) Pick {
	t.Helper()
	p, ok := tg.Current()
	if !ok {
		t.Fatal("no pick")
	}
	return p
}

// The knob wraps both ways over groups; held, over the group's topics; a new
// group starts at its first topic.
func TestTurnWrapsAround(t *testing.T) {
	tg, _ := load(t, seed)
	if p := pick(t, tg); p.Chat != -1001 || p.Topic != 1 {
		t.Fatalf("start = %+v", p)
	}
	tg.Turn(2, true)
	if p := pick(t, tg); p.Topic != 90 || p.TopicLabel != "test" {
		t.Fatalf("held +2 = %+v", p)
	}
	tg.Turn(1, true)
	if p := pick(t, tg); p.Topic != 1 {
		t.Fatalf("held +1 past the end = %+v, want wrapped to General", p)
	}
	tg.Turn(-1, true)
	if p := pick(t, tg); p.Topic != 90 {
		t.Fatalf("held -1 from the first = %+v, want the last topic", p)
	}
	tg.Turn(-1, false)
	if p := pick(t, tg); p.Chat != -1003 || p.Topic != 0 || p.TopicLabel != "" {
		t.Fatalf("-1 from the first group = %+v, want 安昌的群 with no topic", p)
	}
	if tg.Turn(1, true) {
		t.Fatal("held turn moved in a group with no topics")
	}
	tg.Turn(5, false) // 2 + 5 = 7 ≡ 1
	if p := pick(t, tg); p.Chat != -1002 || p.Topic != 1 {
		t.Fatalf("+5 = %+v, want 蓝蟹的群 at its first topic", p)
	}
}

// Amendment 1: while locked a turn does not move the pick; after unlocking
// it does.
func TestLockMakesTurnInert(t *testing.T) {
	tg, _ := load(t, seed)
	if locked, ok := tg.ToggleLock(); !locked || !ok {
		t.Fatalf("ToggleLock = %v, %v", locked, ok)
	}
	before := pick(t, tg)
	if tg.Turn(1, false) || tg.Turn(1, true) {
		t.Fatal("Turn reported a move while locked")
	}
	if p := pick(t, tg); p != before {
		t.Fatalf("pick moved while locked: %+v → %+v", before, p)
	}
	if p, ok := tg.Locked(); !ok || p.Chat != -1001 {
		t.Fatalf("Locked = %+v, %v", p, ok)
	}
	if locked, _ := tg.ToggleLock(); locked {
		t.Fatal("second ToggleLock did not unlock")
	}
	if _, ok := tg.Locked(); ok {
		t.Fatal("Locked after unlock")
	}
	if !tg.Turn(1, false) || pick(t, tg).Chat != -1002 {
		t.Fatalf("Turn after unlock did not move: %+v", pick(t, tg))
	}
}

// Pick and lock survive a reload, written atomically with no temp file left.
func TestTargetsPersist(t *testing.T) {
	tg, path := load(t, seed)
	tg.Turn(1, false)
	tg.ToggleLock()
	again, err := LoadTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := pick(t, again); p.Chat != -1002 || !p.Locked {
		t.Fatalf("reloaded = %+v", p)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("dir holds %d entries, want keypad.json alone", len(ents))
	}
}

// No file, a corrupt file and a file with no groups all mean no targets:
// lock refused, turns do nothing, the snapshot shows no labels.
func TestNoTargets(t *testing.T) {
	missing, path := load(t, "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("loading created the file: %v", err)
	}
	empty, _ := load(t, `{"groups": [], "locked": true}`)
	corrupt, err := LoadTargets(writeFile(t, "{not json"))
	if err == nil {
		t.Fatal("corrupt file loaded with no error")
	}
	for name, tg := range map[string]*Targets{"missing": missing, "empty": empty, "corrupt": corrupt} {
		if locked, ok := tg.ToggleLock(); locked || ok {
			t.Errorf("%s: ToggleLock = %v, %v; want refused", name, locked, ok)
		}
		if tg.Turn(1, false) {
			t.Errorf("%s: Turn moved", name)
		}
		if _, ok := tg.Current(); ok {
			t.Errorf("%s: Current ok", name)
		}
		s := Snapshot{Lock: true, Group: "stale", Topic: "stale"}
		tg.Fill(&s)
		if s.Lock || s.Group != "" || s.Topic != "" {
			t.Errorf("%s: Fill = %+v", name, s)
		}
	}
}

// The pick and the lock are saved by id: a list reordered under them (a
// topics refresh) keeps the same target locked.
func TestLockSurvivesReorder(t *testing.T) {
	tg, path := load(t, seed)
	tg.Turn(1, true) // 报到群 / ccc-status
	tg.ToggleLock()
	reordered := `{
  "groups": [
    {"chat": -1003, "label": "安昌的群"},
    {"chat": -1001, "label": "报到群", "topics": [{"id": 90, "label": "test"}, {"id": 77, "label": "ccc-status"}, {"id": 1, "label": "General"}]}
  ],
  "chat": -1001, "topic": 77, "locked": true
}`
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), `"chat": -1001`) || !strings.Contains(string(saved), `"topic": 77`) {
		t.Fatalf("saved file does not hold the pick by id:\n%s", saved)
	}
	again, err := LoadTargets(writeFile(t, reordered))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := again.Locked(); !ok || p.Chat != -1001 || p.Topic != 77 {
		t.Fatalf("after reorder Locked = %+v, %v; want 报到群 / ccc-status", p, ok)
	}
}

// A locked target no longer listed drops the lock — never a lock on some
// other target. The pick falls back to the first topic of its group while
// the group is listed, else to the first group.
func TestLockedTargetGoneDropsLock(t *testing.T) {
	for name, c := range map[string]struct {
		body        string
		chat, topic int64
	}{
		"group gone":       {`{"groups": [{"chat": 5, "label": "a", "topics": [{"id": 1, "label": "G"}]}], "chat": 9, "topic": 1, "locked": true}`, 5, 1},
		"topic gone":       {`{"groups": [{"chat": 3, "label": "b"}, {"chat": 5, "label": "a", "topics": [{"id": 1, "label": "G"}]}], "chat": 5, "topic": 7, "locked": true}`, 5, 1},
		"topics now empty": {`{"groups": [{"chat": 3, "label": "b"}, {"chat": 5, "label": "a"}], "chat": 5, "topic": 7, "locked": true}`, 5, 0},
		"no pick saved":    {`{"groups": [{"chat": 5, "label": "a", "topics": [{"id": 1, "label": "G"}]}], "locked": true}`, 5, 1},
	} {
		tg, err := LoadTargets(writeFile(t, c.body))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tg.Locked(); ok {
			t.Errorf("%s: still locked", name)
		}
		if p := pick(t, tg); p.Chat != c.chat || p.Topic != c.topic {
			t.Errorf("%s: pick = %+v, want chat %d topic %d", name, p, c.chat, c.topic)
		}
	}
	// A group with no topics locks and reloads with topic 0.
	tg, err := LoadTargets(writeFile(t, `{"groups": [{"chat": 3, "label": "b"}, {"chat": 5, "label": "a"}], "chat": 5, "locked": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := tg.Locked(); !ok || p.Chat != 5 || p.Topic != 0 {
		t.Fatalf("topicless group: Locked = %+v, %v", p, ok)
	}
}

func TestFill(t *testing.T) {
	tg, _ := load(t, seed)
	tg.Turn(1, true)
	tg.ToggleLock()
	s := Snapshot{State: "idle"}
	tg.Fill(&s)
	if s != (Snapshot{State: "idle", Lock: true, Group: "报到群", Topic: "ccc-status", Chat: "-1001", TopicID: 77}) {
		t.Fatalf("Fill = %+v", s)
	}
	tg.ToggleLock()
	tg.Fill(&s)
	if s.Lock || s.Chat != "" || s.TopicID != 0 {
		t.Fatalf("unlocked Fill retained routing metadata: %+v", s)
	}
}

func TestTopicAlias(t *testing.T) {
	tg, path := load(t, strings.Replace(seed, `"label": "ccc-status"`, `"label": "ccc-status", "alias": "SA"`, 1))
	tg.Turn(1, true)
	tg.ToggleLock()
	again, err := LoadTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, targets := range []*Targets{tg, again} {
		s := Snapshot{State: "recording"}
		targets.Fill(&s)
		if s != (Snapshot{State: "recording", Lock: true, Group: "SA", Chat: "-1001", TopicID: 77}) {
			t.Fatalf("aliased Fill = %+v", s)
		}
		targets.ToggleLock()
		targets.Turn(1, true)
		if p := pick(t, targets); p.Group != "报到群" || p.TopicLabel != "test" || p.Topic != 90 {
			t.Fatalf("neighboring topic = %+v", p)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"label": "报到群"`) || !strings.Contains(string(b), `"label": "ccc-status"`) || !strings.Contains(string(b), `"alias": "SA"`) {
		t.Fatalf("saved labels and alias: %s, %v", b, err)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keypad.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
