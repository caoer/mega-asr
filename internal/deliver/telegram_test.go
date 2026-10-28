package deliver

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
