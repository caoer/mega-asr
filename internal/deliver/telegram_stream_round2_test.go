package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/session"
)

func TestRound2StartupProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--startup-fixture" {
		return
	}
	mode, path := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	if mode == "bad" {
		os.Stdout.WriteString("not ready\n")
		os.Exit(0)
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestRound2StartupCancellationJoinsChild(t *testing.T) {
	for _, mode := range []string{"bad", "held"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pid")
			s := &TelegramStream{Argv: []string{os.Args[0], "-test.run=^TestRound2StartupProcess$", "--", "--startup-fixture", mode, path}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.Run(ctx) }()
			until := time.Now().Add(3 * time.Second)
			var pid int
			for time.Now().Before(until) {
				if b, err := os.ReadFile(path); err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
					if pid > 0 {
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("fixture did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("supervisor/child did not join")
			}
			var status syscall.WaitStatus
			_, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if !errors.Is(err, syscall.ECHILD) {
				t.Fatalf("child %d was not reaped: %v", pid, err)
			}
		})
	}
}

func TestRound2AlreadyDeadBeforeWriteFallsBack(t *testing.T) {
	done := make(chan struct{})
	close(done)
	s := &TelegramStream{conn: &telegramConnection{in: closedSenderPipe{}, cancel: func() {}, done: done, replies: make(chan telegramReply, 1)}}
	g := fakeSend(`echo 'Sent → message 84'`)
	g.Stream = s
	target := &TelegramTarget{Chat: "688092930"}
	if err := g.Deliver(target, "fixture"); err != nil || target.MessageID != 84 {
		t.Fatalf("id=%d err=%v", target.MessageID, err)
	}
}

type reviewDelayedZeroPipe struct{ delay time.Duration }

func (p reviewDelayedZeroPipe) Write([]byte) (int, error) {
	time.Sleep(p.delay)
	return 0, os.ErrClosed
}
func (p reviewDelayedZeroPipe) Close() error { return nil }

func TestRound2ZeroByteFallbackSharesOriginalDeadline(t *testing.T) {
	p := reviewDelayedZeroPipe{300 * time.Millisecond}
	s := &TelegramStream{conn: &telegramConnection{in: p, cancel: func() {}, done: make(chan struct{}), replies: make(chan telegramReply, 1)}}
	g := fakeSend(`exec sleep 0.25`)
	g.Stream, g.Wait = s, 400*time.Millisecond
	began := time.Now()
	target := &TelegramTarget{Chat: "688092930"}
	err := g.Deliver(target, "fixture")
	elapsed := time.Since(began)
	t.Logf("deadline=%v elapsed=%v message_id=%d err=%v", g.Wait, elapsed, target.MessageID, err)
	if elapsed > g.Wait+50*time.Millisecond {
		t.Fatal("zero-byte fallback extended the original deadline")
	}
}

type reviewDeadZeroPipe struct {
	done     chan struct{}
	released chan struct{}
	once     sync.Once
}

func (p *reviewDeadZeroPipe) Write([]byte) (int, error) {
	close(p.done)
	<-p.released
	return 0, os.ErrClosed
}
func (p *reviewDeadZeroPipe) Close() error { p.once.Do(func() { close(p.released) }); return nil }

func TestRound2DeadConnectionZeroWriteFallsBack(t *testing.T) {
	p := &reviewDeadZeroPipe{done: make(chan struct{}), released: make(chan struct{})}
	s := &TelegramStream{conn: &telegramConnection{in: p, cancel: func() {}, done: p.done, replies: make(chan telegramReply, 1)}}
	g := fakeSend(`echo 'Sent → message 81'`)
	g.Stream, g.Wait = s, time.Second
	target := &TelegramTarget{Chat: "688092930"}
	err := g.Deliver(target, "fixture")
	t.Logf("message_id=%d err=%v maybe_sent=%v", target.MessageID, err, errors.Is(err, session.ErrMaybeSent))
	if err != nil || target.MessageID != 81 {
		t.Fatal("dead stream skipped fallback despite accepting zero request bytes")
	}
}

func TestRound2BusyStreamFallsBack(t *testing.T) {
	s := &TelegramStream{}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	g := fakeSend(`echo 'Sent → message 82'`)
	g.Stream = s
	target := &TelegramTarget{Chat: "688092930"}
	if err := g.Deliver(target, "fixture"); err != nil || target.MessageID != 82 {
		t.Fatalf("id=%d err=%v", target.MessageID, err)
	}
}

type reviewPartialPipe struct{}

func (reviewPartialPipe) Write([]byte) (int, error) { return 1, os.ErrClosed }
func (reviewPartialPipe) Close() error              { return nil }
func TestRound2PartialWriteNeverFallsBack(t *testing.T) {
	s := &TelegramStream{conn: &telegramConnection{in: reviewPartialPipe{}, cancel: func() {}, done: make(chan struct{}), replies: make(chan telegramReply, 1)}}
	g := fakeSend(`echo 'Sent → message 83'`)
	g.Stream = s
	target := &TelegramTarget{Chat: "688092930"}
	err := g.Deliver(target, "fixture")
	if !errors.Is(err, session.ErrMaybeSent) || target.MessageID != 0 {
		t.Fatalf("id=%d err=%v", target.MessageID, err)
	}
}

type round2HealthPipe struct {
	replies chan telegramReply
	ops     chan string
}

func (p round2HealthPipe) Close() error { return nil }
func (p round2HealthPipe) Write(b []byte) (int, error) {
	var r struct{ ID, Op string }
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	p.ops <- r.Op
	yes := true
	p.replies <- telegramReply{ID: r.ID, OK: &yes, Alive: true}
	return len(b), nil
}
func TestRound2HealthUsesTelegramProbe(t *testing.T) {
	replies := make(chan telegramReply, 1)
	ops := make(chan string, 1)
	s := &TelegramStream{conn: &telegramConnection{in: round2HealthPipe{replies, ops}, cancel: func() {}, done: make(chan struct{}), replies: replies}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !s.Check(ctx) {
		t.Fatal("health check failed")
	}
	if op := <-ops; op != "ping" {
		t.Fatal(op)
	}
}

func TestRound2EnsureRunsHealthMonitor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe")
	s := startStream(t, "health:"+path)
	s.Ensure()
	until := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(until) {
		if b, err := os.ReadFile(path); err == nil && string(b) == "ping" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("take-start Ensure did not run the API health protocol")
}
func TestRound2ReconnectRetiresStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	s := &TelegramStream{Argv: []string{os.Args[0], "-test.run=^TestRound2StartupProcess$", "--", "--startup-fixture", "held", path}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	readPID := func() string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
	until := time.Now().Add(3 * time.Second)
	for readPID() == "" && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	first := readPID()
	if first == "" {
		t.Fatal("startup did not begin")
	}
	s.Ensure()
	until = time.Now().Add(500 * time.Millisecond)
	for (readPID() == "" || readPID() == first) && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if next := readPID(); next == "" || next == first {
		t.Fatal("reconnect did not retire startup and bypass backoff")
	}
}
