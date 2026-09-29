package deliver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dictation written for the test, in both languages. oneLine and lines25
// repeat one sentence; prose does not, so its head and its tail differ.
var (
	prose = "Harbor tide tables are printed each Tuesday. The ferry leaves at seven and returns by noon. " +
		"这批茶叶要放在阴凉干燥的地方，三个月后再开封。Alice Example keeps the spare keys in the blue tin by the door. " +
		"午饭以后我们去码头看船，顺便把信寄出去。Bob Example repaints the fence every other spring."

	oneLine = strings.Repeat("Tide tables for the harbor are printed each Tuesday. 这批茶叶要放在阴凉干燥的地方，三个月后再开封。", 12)
	lines25 = strings.TrimSuffix(strings.Repeat("Alice Example measured the queue depth every minute.\n", 26), "\n")
)

// wrap lays text out as a terminal program does: rows of width columns,
// each continuation indented.
func wrap(text string, width int) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > width {
			b.WriteString(string(r[:width]) + "\n  ")
			r = r[width:]
		}
		b.WriteString(string(r) + "\n")
	}
	return b.String()
}

const box = "──────────────────────────────────────── model ─\n"

// claudeSent is a Claude Code pane after the Enter: the sent message, the
// answer under it, the empty input box and the status line.
func claudeSent(msg string) string {
	return " ▐▛███▜▌   Claude Code\n  ~/demo\n\n❯ " + wrap(msg, 80) +
		"  ⎿  Done: three files checked.\n\n✻ Brewed for 0s\n\n" + box + "❯ \n" + box + "  ? for shortcuts\n"
}

// claudeInput is a Claude Code pane holding a paste it has not sent.
func claudeInput(input string) string {
	return " ▐▛███▜▌   Claude Code\n\n" + box + "❯ " + input + "\n" + box + "  paste again to expand\n"
}

// zsh is a shell prompt holding a paste in its line editor.
func zsh(input string) string {
	return "alice@host-a demo % " + input
}

func TestScreen(t *testing.T) {
	cut := []rune(oneLine)
	for _, c := range []struct {
		name, text, read, want string
	}{
		{"sent message whole", oneLine, claudeSent(oneLine), Whole},
		{"sent message keeps only its tail", oneLine, claudeSent(string(cut[len(cut)-150:])), Short},
		{"sent message keeps only its head", oneLine, claudeSent(string(cut[:300])), Short},
		{"chip counts fewer lines", lines25, claudeInput("[Pasted text #3 +13 lines]"), Short},
		{"chip counts the text's lines", lines25, claudeInput("[Pasted text #3 +25 lines]"), Unknown},
		{"chip without a count", oneLine, claudeInput("[Pasted text #1]"), Unknown},
		{"an earlier copy up the scrollback leaves a chip unknown",
			oneLine, claudeSent(oneLine) + strings.Repeat("An answer written for the test.\n", 40) + claudeInput("[Pasted text #2]"), Unknown},
		{"shell line whole", oneLine, zsh(wrap(oneLine, 120)), Whole},
		{"shell line keeps its head", lines25, zsh(strings.Join(strings.Split(lines25, "\n")[:9], "\n")), Short},
		{"the read starts inside the text", oneLine, wrap(string(cut[200:]), 80) + box, Unknown},
		{"the text is not on the screen", oneLine, zsh(""), Unknown},
		{"no letters in the text", "。", zsh("。"), Unknown},
		{"a text whose head and tail differ, whole", prose, claudeSent(prose), Whole},
		{"a text whose head and tail differ keeps only its head", prose, claudeSent(string([]rune(prose)[:120])), Short},
		{"a text whose head and tail differ keeps only its tail", prose, claudeSent(string([]rune(prose)[120:])), Short},
		{"its head, then more than the slack of other letters", prose,
			claudeSent(string([]rune(prose)[:120])) + strings.Repeat("An answer written for the test.\n", 40), Unknown},
	} {
		if got, why := screen(c.text, c.read); got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, why, c.want)
		}
	}
}

// A field is read at its tail: an earlier copy of the text, or of its head
// or tail, elsewhere in a document is not this paste, and a paste the app
// changed a letter of cannot be told.
func TestField(t *testing.T) {
	head := string([]rune(oneLine)[:200])
	earlier := strings.Replace(oneLine, "Tuesday", "Friday", 1) // a near-copy: one word differs
	// the app's edit, in the last sentence: oneLine repeats one sentence, so
	// an edit further up leaves a head of it at the value's end
	edited := func(old, new string) string {
		i := strings.LastIndex(oneLine, old)
		return oneLine[:i] + new + oneLine[i+len(old):]
	}
	for _, c := range []struct{ name, value, want string }{
		{"the paste after a draft", "Bob Example wrote this first. " + oneLine, Whole},
		{"only the head arrived", head, Short},
		{"only the head arrived, after a draft", "Bob Example wrote this first. " + head, Short},
		{"the field emptied on Enter", "", Unknown},
		{"the paste has not landed", "Bob Example wrote this first.", Unknown},
		{"not landed, an earlier near-copy up the document", earlier + "\nBob Example wrote this after.", Unknown},
		{"not landed, an earlier copy up the document", oneLine + "\nBob Example wrote this after.", Unknown},
		{"not landed, an earlier copy's head up the document", head + "\nBob Example wrote this after.", Unknown},
		{"the app changed one word", "Bob Example wrote this first. " + edited("Tuesday", "Friday"), Unknown},
		{"the app changed a letter's case", "Bob Example wrote this first. " + edited("Tide", "tide"), Unknown},
		{"text after the paste", "Bob Example wrote this first. " + oneLine + " Then this line, typed after it.", Unknown},
	} {
		if got, why := field(oneLine, c.value); got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, why, c.want)
		}
	}
	if got, why := field(prose, "Bob Example wrote this first. "+string([]rune(prose)[:90])); got != Short || why != "first 74 of 205" {
		t.Errorf("a text whose head and tail differ, only its head arrived: %s (%s), want short", got, why)
	}
}

// reads answers each read with the next screen, or err.
func reads(screens ...string) func(context.Context) (string, error) {
	var mu sync.Mutex
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		s := screens[0]
		screens = screens[1:]
		if s == "ERR" {
			return "", errors.New("herdr pane.read: pane_not_found")
		}
		return s, nil
	}
}

// A screen still drawing a paste looks short once: short needs two reads
// that agree, whole needs one.
func TestReadScreen(t *testing.T) {
	head := zsh(string([]rune(oneLine)[:100]))
	other := zsh(string([]rune(oneLine)[:300]))
	for _, c := range []struct {
		name    string
		screens []string
		want    string
	}{
		{"drawing, then whole", []string{head, zsh(oneLine)}, Whole},
		{"short twice alike", []string{head, head}, Short},
		{"short twice unlike", []string{head, other}, Unknown},
		{"a read errors", []string{"ERR", "ERR"}, Unknown},
		{"whole at once", []string{zsh(oneLine)}, Whole},
	} {
		if got, why := readScreen(context.Background(), reads(c.screens...), oneLine, time.Now(), 0, time.Millisecond); got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, why, c.want)
		}
	}
}

// A readback that has not answered by its deadline is unknown, and returns
// then.
func TestBeforeDeadline(t *testing.T) {
	start := time.Now()
	got, _ := before(start.Add(50*time.Millisecond), func(context.Context) (string, string) { time.Sleep(time.Second); return Whole, "found" })
	if got != Unknown || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("%s after %v, want unknown at the deadline", got, time.Since(start))
	}
}

// fakeHerdrAPI serves pane.get with agent and pane.read with screen, and
// counts the reads; bin is a herdr that reports its socket.
func fakeHerdrAPI(t *testing.T, agent, screen string) (bin string, readsMade func() int) {
	bin, readsMade, _ = fakeHerdrAPIOf(t, agent, screen, "")
	return bin, readsMade
}

// fakeHerdrAPIOf is fakeHerdrAPI with a fault: "get error" answers pane.get
// with an error, "read hangs" never answers pane.read and reports on closed
// when the client closes that connection.
func fakeHerdrAPIOf(t *testing.T, agent, screen, fault string) (bin string, readsMade func() int, closed <-chan time.Time) {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr") // a unix socket path must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	n := 0
	closes := make(chan time.Time, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(conn)
			line, _ := br.ReadBytes('\n')
			var req struct{ Method string }
			json.Unmarshal(line, &req)
			var res any
			switch req.Method {
			case "pane.get":
				if fault == "get error" {
					conn.Write([]byte(`{"id":"megavoice","error":{"code":"pane_not_found","message":"no pane w1:p1"}}` + "\n"))
					conn.Close()
					continue
				}
				res = map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "w1:p1", "agent": agent}}
			case "pane.read":
				mu.Lock()
				n++
				mu.Unlock()
				if fault == "read hangs" {
					go func() {
						br.ReadByte() // returns once the client closes
						closes <- time.Now()
						conn.Close()
					}()
					continue
				}
				res = map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": "w1:p1", "text": screen}}
			}
			b, _ := json.Marshal(map[string]any{"id": "megavoice", "result": res})
			conn.Write(append(b, '\n'))
			conn.Close()
		}
	}()
	bin = filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'status: running'\necho 'socket: "+sock+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int { mu.Lock(); defer mu.Unlock(); return n }, closes
}

// A pane is told for an agent's pane after the Enter and for another
// program's pane before any; the other two cases are unknown unread.
func TestPaneReceived(t *testing.T) {
	for _, c := range []struct {
		name, agent, screen string
		entered             bool
		want                string
		read                bool
	}{
		{"agent pane, sent", "claude", claudeSent(oneLine), true, Whole, true},
		{"agent pane, sent short", "claude", claudeSent(string([]rune(oneLine)[500:])), true, Short, true},
		{"agent pane, pasted", "claude", claudeInput("[Pasted text #1]"), false, Unknown, false},
		{"shell pane, pasted", "", zsh(oneLine), false, Whole, true},
		{"shell pane, sent", "", zsh(oneLine), true, Unknown, false},
	} {
		bin, readsMade := fakeHerdrAPI(t, c.agent, c.screen)
		got, why := Herdr{Bin: bin}.received("w1:p1", oneLine, time.Now().Add(-paneSecond), c.entered)
		if got != c.want || (readsMade() > 0) != c.read {
			t.Errorf("%s: %s (%s) with %d reads, want %s (read %v)", c.name, got, why, readsMade(), c.want, c.read)
		}
	}
	if got, _ := (Herdr{Bin: "/nonexistent/herdr"}).received("w1:p1", oneLine, time.Now(), true); got != Unknown {
		t.Errorf("herdr missing: %s, want unknown", got)
	}
	bin, readsMade, _ := fakeHerdrAPIOf(t, "claude", claudeSent(oneLine), "get error")
	if got, why := (Herdr{Bin: bin}).received("w1:p1", oneLine, time.Now().Add(-paneSecond), false); got != Unknown || readsMade() != 0 {
		t.Errorf("pane.get errs: %s (%s) with %d reads, want unknown unread", got, why, readsMade())
	}
}

// A readback past its deadline ends there: its read is abandoned, the
// connection closed, not left to its own timeout.
func TestPaneReadbackEndsAtItsDeadline(t *testing.T) {
	bin, _, closed := fakeHerdrAPIOf(t, "", zsh(oneLine), "read hangs")
	sent := time.Now().Add(-paneDeadline + 500*time.Millisecond) // the deadline in 0.5 s, the first read due now
	got, _ := (Herdr{Bin: bin}).received("w1:p1", oneLine, sent, false)
	deadline := sent.Add(paneDeadline)
	if got != Unknown {
		t.Fatalf("%s, want unknown", got)
	}
	select {
	case at := <-closed:
		if d := at.Sub(deadline); d > 300*time.Millisecond {
			t.Fatalf("the read was closed %v after the deadline", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the read was still open 2 s after the deadline")
	}
}

// Each herdr call ends within herdrWait: a CLI that never exits, a socket
// that never answers, and a dial whose context has ended; a request also
// ends when its context is cancelled while it waits for the answer.
func TestHerdrTimeouts(t *testing.T) {
	defer func(w time.Duration) { herdrWait = w }(herdrWait)
	herdrWait = 200 * time.Millisecond
	dir, err := os.MkdirTemp("", "herdr") // a unix socket path must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	slow := filepath.Join(dir, "herdr")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	within := func(name string, f func() error) {
		t.Helper()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			if err == nil || time.Since(start) > herdrWait+time.Second {
				t.Errorf("%s: %v after %v", name, err, time.Since(start))
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: no answer within 5 s", name)
		}
	}
	within("run", func() error { _, err := (Herdr{Bin: slow}).run("pane", "list"); return err })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, never answered
		}
	}()
	within("request", func() error { _, err := request(context.Background(), sock, "pane.get", nil); return err })
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	within("dial", func() error { _, err := request(ended, sock, "pane.get", nil); return err })
	// a context cancelled while the request waits for its answer ends it
	herdrWait = time.Minute
	cut, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := request(cut, sock, "pane.get", nil); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("cancelled: %v after %v", err, time.Since(start))
	}
}

// A readback's reads never reach the log: neither the text nor the screen.
func TestReadbackLogsNoText(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, agent := range []string{"claude", ""} {
		bin, _ := fakeHerdrAPI(t, agent, claudeSent(prose))
		Herdr{Bin: bin}.received("w1:p1", prose, time.Now().Add(-paneSecond), agent != "")
	}
	field(prose, "Bob Example wrote this first. "+prose)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "Harbor") || strings.Contains(buf.String(), "茶叶") {
		t.Fatalf("the log holds the text:\n%s", buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
