package deliver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/session"
)

func TestTelegramStreamProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--sender-fixture" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if strings.HasPrefix(mode, "unsupported:") {
		path := strings.TrimPrefix(mode, "unsupported:")
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		f.WriteString("attempt\n")
		f.Close()
		os.Stderr.WriteString("No command registered for `sender`, did you mean `sendAsUser`?\n")
		os.Exit(251)
	}
	if mode == "cold" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "version": 1})
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var req struct {
			ID, Chat, Text, Op string
			Topic              int64
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if strings.HasPrefix(mode, "health:") && req.Op == "ping" {
			os.WriteFile(strings.TrimPrefix(mode, "health:"), []byte("ping"), 0600)
			json.NewEncoder(os.Stdout).Encode(map[string]any{"id": req.ID, "ok": true, "alive": true})
			continue
		}
		if mode == "die" {
			os.Exit(0)
		}
		if strings.HasPrefix(mode, "restart:") {
			marker := strings.TrimPrefix(mode, "restart:")
			if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
				os.WriteFile(marker, []byte("fixture"), 0600)
				os.Exit(0)
			}
		}
		if mode == "slow" {
			time.Sleep(time.Second)
		}
		id := req.ID
		if mode == "malformed" {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"id": id})
			continue
		}
		if mode == "wrong-id" {
			id = "another-take"
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": id, "ok": true, "message_ids": []int64{42}})
	}
	os.Exit(0)
}

func startStream(t *testing.T, mode string) *TelegramStream {
	t.Helper()
	s := &TelegramStream{Argv: []string{os.Args[0], "-test.run=^TestTelegramStreamProcess$", "--", "--sender-fixture", mode}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("sender did not stop")
		}
	})
	if mode != "cold" {
		until := time.Now().Add(5 * time.Second)
		for !s.Ready() && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if !s.Ready() {
			t.Fatal("sender did not become ready")
		}
	}
	return s
}

func TestTelegramWarmStreamAcknowledgesWithoutFallback(t *testing.T) {
	s := startStream(t, "ack")
	target := &TelegramTarget{Chat: "688092930", Topic: 1, Group: "Saved Messages"}
	var copies, confirmed int
	g := Telegram{Argv: []string{"sh", "-c", "exit 99"}, Stream: s, Copy: func(string) error { copies++; return nil }, OnSent: func(*TelegramTarget) { confirmed++ }}
	if err := g.Deliver(target, "fixture"); err != nil {
		t.Fatal(err)
	}
	if target.MessageID != 42 || copies != 1 || confirmed != 1 {
		t.Fatalf("target=%+v copies=%d confirmed=%d", target, copies, confirmed)
	}
}

func TestTelegramColdStreamFallsBackImmediately(t *testing.T) {
	s := startStream(t, "cold")
	g := fakeSend(`echo 'Sent → message 73'`)
	g.Stream = s
	target := &TelegramTarget{Chat: "688092930"}
	start := time.Now()
	if err := g.Deliver(target, "fixture"); err != nil {
		t.Fatal(err)
	}
	if target.MessageID != 73 || time.Since(start) > time.Second {
		t.Fatalf("cold send waited for stream: %+v", target)
	}
}

func TestTelegramLostAcknowledgementNeverFallsBack(t *testing.T) {
	for _, mode := range []string{"die", "slow", "wrong-id", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s := startStream(t, mode)
			marker := filepath.Join(t.TempDir(), "fallback")
			g := fakeSend(`echo fallback > ` + marker)
			g.Stream = s
			g.Wait = 50 * time.Millisecond
			var confirmed int
			g.OnSent = func(*TelegramTarget) { confirmed++ }
			err := g.Deliver(&TelegramTarget{Chat: "688092930"}, "fixture")
			if !errors.Is(err, session.ErrMaybeSent) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ambiguous request was replayed")
			}
			if confirmed != 0 {
				t.Fatal("unconfirmed send shown as sent")
			}
		})
	}
}

func TestTelegramStreamCommandOnlyForDefaultUser(t *testing.T) {
	user, _ := TelegramCommand("user", "sender-host")
	bot, _ := TelegramCommand("bot", "sender-host")
	if user.Stream == nil || bot.Stream != nil {
		t.Fatal("stream must be additive for the default user sender")
	}
	if got := strings.Join(user.Stream.Argv, " "); !strings.Contains(got, "ccc-telegram sender") || strings.Contains(got, "{text}") {
		t.Fatal(got)
	}
}

type closedSenderPipe struct{}

func (closedSenderPipe) Write([]byte) (int, error) { return 0, os.ErrClosed }
func (closedSenderPipe) Close() error              { return nil }

func TestTelegramZeroByteWriteFallsBack(t *testing.T) {
	s := &TelegramStream{conn: &telegramConnection{in: closedSenderPipe{}, cancel: func() {}, done: make(chan struct{}), replies: make(chan telegramReply, 1)}}
	g := fakeSend(`echo 'Sent → message 81'`)
	g.Stream = s
	target := &TelegramTarget{Chat: "688092930"}
	if err := g.Deliver(target, "fixture"); err != nil || target.MessageID != 81 {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}

func TestTelegramStreamReconnectsAfterLostAcknowledgement(t *testing.T) {
	s := startStream(t, "restart:"+filepath.Join(t.TempDir(), "first-request"))
	g := Telegram{Argv: []string{"sh", "-c", "exit 99"}, Stream: s, Wait: time.Second}
	if err := g.Deliver(&TelegramTarget{Chat: "688092930"}, "first fixture"); !errors.Is(err, session.ErrMaybeSent) {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for !s.Ready() && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if !s.Ready() {
		t.Fatal("sender did not reconnect")
	}
	target := &TelegramTarget{Chat: "688092930"}
	if err := g.Deliver(target, "second fixture"); err != nil || target.MessageID != 42 {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}
