package deliver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
)

// fakeSend is a Telegram whose send command is script run by sh -c with
// {chat} {topic} {text} as $1 $2 $3.
func fakeSend(script string) Telegram {
	return Telegram{Argv: []string{"sh", "-c", script, "sh", "{chat}", "{topic}", "{text}"}}
}

func TestTelegramSent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	g := fakeSend(`{ printf '%s|%s|%s|' "$1" "$2" "$3"; cat; printf '|%s' "$CCC_TELEGRAM_AGENT_BLOCK"; } > ` + out + `; echo "Sent → message 4242"`)
	var copied []string
	g.Copy = func(s string) error { copied = append(copied, s); return nil }
	tt := &TelegramTarget{Chat: "-1001234", Topic: 77, Group: "报到群", TopicLabel: "ccc-status"}
	text := `他说 "it's $HOME" {chat}`
	if err := g.Deliver(tt, text); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if want := "-1001234|77|" + text + "|" + text + "|off"; string(b) != want {
		t.Errorf("command got %q, want %q", b, want)
	}
	if tt.MessageID != 4242 {
		t.Errorf("message id %d, want 4242", tt.MessageID)
	}
	if !slices.Equal(copied, []string{text}) {
		t.Errorf("copied %q, want the text once", copied)
	}
}

func TestTelegramGeneralTopic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	g := fakeSend(`printf %s "$2" > ` + out)
	if err := g.Deliver(&TelegramTarget{Chat: "@somegroup"}, "hi"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "1" {
		t.Errorf("topic %q, want 1 (General)", b)
	}
}

func TestTelegramSentWithoutID(t *testing.T) {
	tt := &TelegramTarget{Chat: "1"}
	if err := fakeSend(`echo Message sent.`).Deliver(tt, "hi"); err != nil {
		t.Fatal(err)
	}
	if tt.MessageID != 0 {
		t.Errorf("message id %d, want 0", tt.MessageID)
	}
}

func TestTelegramFailed(t *testing.T) {
	var copied []string
	g := fakeSend(`echo; echo "error: session expired, log in again" >&2; echo more >&2; exit 3`)
	g.Copy = func(s string) error { copied = append(copied, s); return nil }
	err := g.Deliver(&TelegramTarget{Chat: "1"}, "hi")
	if err == nil || err.Error() != "telegram: error: session expired, log in again" {
		t.Errorf("err %v, want stderr's first line", err)
	}
	if !slices.Equal(copied, []string{"hi"}) {
		t.Errorf("copied %q: a failed send keeps the text on the clipboard", copied)
	}
	if err := fakeSend(`exit 2`).Deliver(&TelegramTarget{Chat: "1"}, "hi"); err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("silent failure: err %v, want the exit status", err)
	}
}

func TestTelegramDeadline(t *testing.T) {
	for _, script := range []string{`exec sleep 30`, `sleep 30; echo late`} {
		g := fakeSend(script)
		g.Wait = 200 * time.Millisecond
		start := time.Now()
		err := g.Deliver(&TelegramTarget{Chat: "1"}, "hi")
		if err == nil || !errors.Is(err, session.ErrMaybeSent) || err.Error() != "telegram: timeout, no answer in 200ms; 可能已发送" {
			t.Errorf("%s: err %v, want a timeout that may have sent", script, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: returned after %s", script, d)
		}
	}
	if err := sendErr(nil, true, time.Second, ""); err != nil {
		t.Errorf("an exit 0 at the deadline: %v, want sent", err)
	}
	if err := sendErr(errors.New("signal: killed"), true, time.Second, "partial\n"); !errors.Is(err, session.ErrMaybeSent) {
		t.Errorf("killed at the deadline: %v, want ErrMaybeSent", err)
	}
	if err := sendErr(errors.New("exit status 1"), false, time.Second, "expired\n"); err == nil || errors.Is(err, session.ErrMaybeSent) {
		t.Errorf("a plain failure: %v", err)
	}
	if TelegramWait != 20*time.Second {
		t.Errorf("the default deadline is %s, want 20s", TelegramWait)
	}
}

func TestTelegramRefusesChat(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	for _, chat := range []string{"", "1; rm -rf ~", "$(id)", "@a b"} {
		err := fakeSend(`touch `+ran).Deliver(&TelegramTarget{Chat: chat}, "hi")
		if err == nil {
			t.Errorf("chat %q accepted", chat)
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the command ran for a refused chat")
	}
}

func TestTelegramCommand(t *testing.T) {
	g, err := TelegramCommand("user", "sender.example")
	if err != nil {
		t.Fatal(err)
	}
	argv := g.Argv
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "sender.example",
		`CCC_TELEGRAM_AGENT_BLOCK=off ccc-telegram sendAsUser --topic {topic} -- {chat} "$(cat)"`}
	if !slices.Equal(argv, want) {
		t.Errorf("user argv %q", argv)
	}
	g, _ = TelegramCommand("bot", "h")
	argv = g.Argv
	if last := argv[len(argv)-1]; !strings.Contains(last, `sendAsBot --plain --chat {chat} --topic {topic} -- "$(cat)"`) || strings.Contains(last, "{text}") {
		t.Errorf("bot argv %q", argv)
	}
	if _, err := TelegramCommand("robot", "h"); err == nil {
		t.Error("sender robot accepted")
	}
	if _, err := TelegramCommand("user", ""); err == nil {
		t.Error("empty via accepted")
	}
}

func TestTelegramTargetRecord(t *testing.T) {
	tt := &TelegramTarget{Chat: "-100", Topic: 5, Group: "报到群", TopicLabel: "ccc-status"}
	r := tt.Record()
	if r.Kind != "telegram" || r.App != "报到群" || r.Title != "ccc-status" {
		t.Errorf("record %+v", r)
	}
	if s := tt.String(); s != "Telegram 报到群 / ccc-status" {
		t.Errorf("String %q", s)
	}
}

// The one-shot command hands ccc-telegram the chat and the text after "--",
// so a dictated text that reads like a flag ("-h", "--help", "-5度") is sent
// as itself. The remote line runs under sh with a fake ccc-telegram that
// records its argv, the text arriving on stdin as over ssh.
func TestTelegramCommandDashText(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "ccc-telegram")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > "+got+"\necho \"Sent → message 7\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, sender := range []string{"user", "bot"} {
		g, err := TelegramCommand(sender, "h")
		if err != nil {
			t.Fatal(err)
		}
		g.Argv = []string{"sh", "-c", g.Argv[len(g.Argv)-1]} // the line ssh would run on via
		for _, text := range []string{"-h", "--help", "-H", "--topic=9", "-s=zitao", "-5度 明天降温", "--", "我说 --help", "-\n-h"} {
			os.Remove(got)
			tt := &TelegramTarget{Chat: "-1004466665857", Topic: 3}
			if err := g.Deliver(tt, text); err != nil || tt.MessageID != 7 {
				t.Errorf("%s %q: err %v, message %d", sender, text, err, tt.MessageID)
				continue
			}
			b, _ := os.ReadFile(got)
			argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
			want := []string{"sendAsUser", "--topic", "3", "--", "-1004466665857", text}
			if sender == "bot" {
				want = []string{"sendAsBot", "--plain", "--chat", "-1004466665857", "--topic", "3", "--", text}
			}
			if !slices.Equal(argv, want) {
				t.Errorf("%s %q: argv %q, want %q", sender, text, argv, want)
			}
		}
	}
}

// fakeTelegram is a send command that behaves as Telegram does with a
// message past its limit: it refuses one over TelegramLimit bytes (the texts
// here are ASCII but for the mark, so bytes bound code points) with
// MESSAGE_TOO_LONG, and keeps each message it takes as dir/1, dir/2, ….
// failAt, when not 0, refuses that message instead.
// fakeTelegram is a send command that takes each message as Telegram does,
// refusing one past 4096 UTF-16 units (MESSAGE_TOO_LONG), and keeps it in
// dir as 1, 2, …; message failAt is refused (FLOOD_WAIT_30). It is this test
// binary run again (TestFakeTelegramProcess).
func fakeTelegram(dir string, failAt int) Telegram {
	return Telegram{Argv: []string{os.Args[0], "-test.run=^TestFakeTelegramProcess$", "--", dir, strconv.Itoa(failAt), "{text}"}}
}

// TestFakeTelegramProcess is fakeTelegram's send command; run by go test it
// does nothing.
func TestFakeTelegramProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 4 {
		return
	}
	dir, failAt, text := args[1], args[2], args[3]
	entries, _ := os.ReadDir(dir)
	n := strconv.Itoa(len(entries) + 1)
	switch {
	case len(utf16.Encode([]rune(text))) > 4096:
		fmt.Fprintln(os.Stderr, "MESSAGE_TOO_LONG")
		os.Exit(1)
	case n == failAt:
		fmt.Fprintln(os.Stderr, "FLOOD_WAIT_30")
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, n), []byte(text), 0o600); err != nil {
		os.Exit(2)
	}
	fmt.Println("Sent → message " + n)
	os.Exit(0)
}

// messages are the texts fakeTelegram took, in order.
func messages(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	for n := 1; ; n++ {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(n)))
		if err != nil {
			return got
		}
		got = append(got, string(b))
	}
}

// A both copy past Telegram's limit arrives whole: in two messages, cut
// where the text as heard starts, every word in order. Sent as one, Telegram
// refuses it and the take is not delivered.
func TestTelegramLongTextArrivesWhole(t *testing.T) {
	dir := t.TempDir()
	fixed := strings.Repeat("Please report to your Super Advisor. ", 80) // 2960 characters
	heard := strings.Repeat("Please report to your super adviser. ", 80)
	sent := fixed + "\n\n[Mega Voice · as heard]\n" + heard
	g := fakeTelegram(dir, 0)
	var copied []string
	g.Copy = func(s string) error { copied = append(copied, s); return nil }
	tt := &TelegramTarget{Chat: "-100"}
	if err := g.Deliver(tt, sent); err != nil {
		t.Fatalf("a %d-character both copy: %v", len([]rune(sent)), err)
	}
	got := messages(t, dir)
	if len(got) != 2 || got[0] != strings.TrimSpace(fixed) || !strings.HasPrefix(got[1], "[Mega Voice · as heard]\n") {
		t.Fatalf("messages %d: %.40q …", len(got), got)
	}
	if !slices.Equal(strings.Fields(strings.Join(got, " ")), strings.Fields(sent)) {
		t.Error("the messages do not carry every word of the copy in order")
	}
	if tt.MessageID != 2 {
		t.Errorf("message id %d, want the last message's, 2", tt.MessageID)
	}
	if !slices.Equal(copied, []string{sent}) {
		t.Errorf("copied %d texts", len(copied))
	}
}

// A part refused after an earlier one went out says how many did and is a
// send that may have reached the chat: the take is held as cut, not lost.
func TestTelegramPartlySent(t *testing.T) {
	dir := t.TempDir()
	text := strings.Repeat("word ", 1000) // 5000 characters: two messages
	err := fakeTelegram(dir, 2).Deliver(&TelegramTarget{Chat: "-100"}, text)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 messages sent, then FLOOD_WAIT_30") || !errors.Is(err, session.ErrMaybeSent) {
		t.Fatalf("err %v", err)
	}
	if got := messages(t, dir); len(got) != 1 {
		t.Fatalf("%d messages went out, want 1", len(got))
	}
}

func TestTelegramParts(t *testing.T) {
	zh := strings.Repeat("这是一句四十个字左右的中文口述，没有空格也没有换行，一直说下去直到句号。", 250) // 9000 code points
	for name, text := range map[string]string{
		"fits":            "  short, spaces kept  ",
		"at the limit":    strings.Repeat("x", TelegramLimit),
		"zh sentences":    zh,
		"no break at all": strings.Repeat("长", 2*TelegramLimit+5),
		"lines":           strings.Repeat("一行字 and words\n", 700),
		"emoji":           strings.Repeat("🎤", 3000),            // 3000 code points, 6000 units
		"ext-b":           strings.Repeat("𠀀𠀁 字", 1200),         // 4800 code points, 7200 units
		"units at limit":  strings.Repeat("🎤", TelegramLimit/2), // 2048 code points, 4096 units
	} {
		parts := telegramParts(text)
		for _, p := range parts {
			if n := len(utf16.Encode([]rune(p))); n > TelegramLimit || n == 0 {
				t.Errorf("%s: a part of %d units", name, n)
			}
		}
		if len(utf16.Encode([]rune(text))) <= TelegramLimit {
			if len(parts) != 1 || parts[0] != text {
				t.Errorf("%s: %q, want the text as it is", name, parts)
			}
			continue
		}
		if nonSpace(strings.Join(parts, "")) != nonSpace(text) {
			t.Errorf("%s: the parts lose or reorder text", name)
		}
		if name == "zh sentences" && !strings.HasSuffix(parts[0], "。") {
			t.Errorf("zh sentences: the first part ends %q, want a sentence's end", []rune(parts[0])[len([]rune(parts[0]))-3:])
		}
	}
}

// nonSpace is s without its whitespace.
func nonSpace(s string) string { return strings.Join(strings.Fields(s), "") }

// A both copy whose corrected text is a few units short of the limit, or at
// it, is cut where the text as heard starts, whatever paragraph breaks and
// spaces the corrected text holds: the first message is the corrected text,
// exactly.
func TestTelegramPartsCutAtTheMark(t *testing.T) {
	heard := strings.TrimSpace(strings.Repeat("heard words ", 100))
	for _, n := range []int{4094, 4095, 4096} {
		for name, unit := range map[string]string{"spaces": "ab cd ", "paragraphs": "abcd\n\n", "emoji": "a🎤 b"} {
			fixed := ofUnits(unit, n)
			text := fixed + "\n\n" + refine.MarkOriginal + "\n" + heard
			parts := telegramParts(text)
			if len(parts) != 2 || parts[0] != fixed || parts[1] != refine.MarkOriginal+"\n"+heard {
				t.Errorf("%d units of %s: %d parts, the first %d units ending %q", n, name, len(parts),
					len(utf16.Encode([]rune(parts[0]))), parts[0][max(0, len(parts[0])-12):])
			}
		}
	}
}

// Each message's id is its own: a last part whose send printed no id leaves
// none, never an earlier part's, and reports no sent message.
func TestTelegramMessageIDPerPart(t *testing.T) {
	dir := t.TempDir()
	g := fakeSend(`n=$(ls ` + dir + ` | wc -l | tr -d ' '); touch ` + dir + `/$n; [ "$n" = 0 ] && echo "Sent → message 7"; exit 0`)
	var reported []int64
	g.OnSent = func(t *TelegramTarget) { reported = append(reported, t.MessageID) }
	tt := &TelegramTarget{Chat: "-100"}
	if err := g.Deliver(tt, strings.Repeat("word ", 1000)); err != nil {
		t.Fatal(err)
	}
	if tt.MessageID != 0 || len(reported) != 0 {
		t.Fatalf("message id %d, reported %v; want none: the last part printed no id", tt.MessageID, reported)
	}
}

// ofUnits is unit repeated to exactly n UTF-16 units, ending in no space: an
// x where the next rune would pass n or where the text would end in a space.
func ofUnits(unit string, n int) string {
	var b []rune
	for k := 0; units(string(b)) < n; k++ {
		r := []rune(unit)[k%len([]rune(unit))]
		if units(string(b))+utf16.RuneLen(r) > n || units(string(b))+1 == n && unicode.IsSpace(r) {
			r = 'x'
		}
		b = append(b, r)
	}
	return string(b)
}
