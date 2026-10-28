package deliver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestUnsupportedSenderRetriesAfterThirtyMinutesAndWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "attempts")
		s := &TelegramStream{Argv: []string{os.Args[0], "-test.run=^TestTelegramStreamProcess$", "--", "--sender-fixture", "unsupported:" + path}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		defer func() { cancel(); <-done }()
		count := func(want int) {
			t.Helper()
			synctest.Wait()
			b, _ := os.ReadFile(path)
			if got := strings.Count(string(b), "attempt\n"); got != want {
				t.Fatalf("attempts=%d, want %d", got, want)
			}
		}
		count(1)
		if s.Ready() {
			t.Fatal("unsupported sender became ready")
		}
		time.Sleep(10 * time.Second)
		count(1)
		for range 10 {
			s.Ensure()
		}
		count(1)
		time.Sleep(30*time.Minute - 10*time.Second - time.Nanosecond)
		count(1)
		time.Sleep(time.Nanosecond)
		count(2)
		s.Reconnect() // workspace wake
		count(3)
		s.Reconnect() // network configuration change
		count(4)
		s.Ensure()
		count(4)
		g := fakeSend(`cat > ` + filepath.Join(filepath.Dir(path), "recorded") + `; echo 'Sent → message 93'`)
		g.Stream = s
		target := &TelegramTarget{Chat: "688092930"}
		if err := g.Deliver(target, "local fixture only"); err != nil || target.MessageID != 93 {
			t.Fatalf("fallback id=%d err=%v", target.MessageID, err)
		}
		count(4)
	})
}
