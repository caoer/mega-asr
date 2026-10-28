package deliver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/session"
)

// TelegramStream owns one supervised stdio sender. Run starts it before a take
// needs delivery; an unavailable stream never delays the one-shot path.
type TelegramStream struct {
	Argv             []string
	mu               sync.Mutex
	sendMu           sync.Mutex
	conn             *telegramConnection
	wake             chan struct{}
	probe            chan struct{}
	activeCancel     context.CancelFunc
	unsupportedUntil time.Time
	forceWake        bool
}

const senderUnsupportedBackoff = 30 * time.Minute

// senderDiagnostic retains only the startup diagnostic needed for capability
// detection while the complete stderr stream still goes to the normal log.
type senderDiagnostic struct{ strings.Builder }

func (d *senderDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if left := 4096 - d.Len(); left > 0 {
		d.Builder.Write(p[:min(left, n)])
	}
	return n, nil
}

type telegramConnection struct {
	in      io.WriteCloser
	cancel  context.CancelFunc
	done    chan struct{}
	replies chan telegramReply
}

type telegramReply struct {
	Ready      bool    `json:"ready"`
	Alive      bool    `json:"alive"`
	Version    int     `json:"version"`
	ID         string  `json:"id"`
	OK         *bool   `json:"ok"`
	MessageIDs []int64 `json:"message_ids"`
	MaybeSent  *bool   `json:"maybe_sent"`
	Error      string  `json:"error"`
}

func (s *TelegramStream) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// Run retains the connection and reconnects with bounded backoff. Cancellation
// closes only this instance's child process and its pipes.
func (s *TelegramStream) Run(ctx context.Context) {
	wake, probe := s.signals()
	monitorCtx, stop := context.WithCancel(ctx)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-probe:
				check, cancel := context.WithTimeout(monitorCtx, 750*time.Millisecond)
				s.Check(check)
				cancel()
			}
		}
	}()
	defer func() { stop(); <-joined }()
	delay := time.Second
	for ctx.Err() == nil {
		ready := s.run(ctx)
		if ready {
			delay = time.Second
		}
		for {
			s.mu.Lock()
			wait := max(delay, time.Until(s.unsupportedUntil))
			s.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
				s.mu.Lock()
				forced := s.forceWake
				s.forceWake = false
				if forced {
					s.unsupportedUntil = time.Time{}
				}
				suppressed := time.Now().Before(s.unsupportedUntil)
				s.mu.Unlock()
				if suppressed {
					continue
				}
				delay = time.Second
			case <-timer.C:
			}
			break
		}
		if !ready {
			delay = min(30*time.Second, delay*2)
		}
	}
}

func (s *TelegramStream) run(ctx context.Context) (ready bool) {
	if len(s.Argv) == 0 {
		return false
	}
	childCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.activeCancel = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.activeCancel = nil; s.mu.Unlock() }()
	cmd := exec.CommandContext(childCtx, s.Argv[0], s.Argv[1:]...)
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	defer out.Close()
	var diagnostic senderDiagnostic
	cmd.Stderr = io.MultiWriter(log.Writer(), &diagnostic)
	if err = cmd.Start(); err != nil {
		return false
	}
	c := &telegramConnection{in: in, cancel: cancel, done: make(chan struct{}), replies: make(chan telegramReply, 1)}
	defer func() {
		s.mu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		s.mu.Unlock()
		cancel()
		in.Close()
		out.Close()
		err := cmd.Wait()
		var exit *exec.ExitError
		if !ready && errors.As(err, &exit) && exit.ExitCode() == 251 &&
			strings.Contains(diagnostic.String(), "No command registered for `sender`") {
			s.mu.Lock()
			s.unsupportedUntil = time.Now().Add(senderUnsupportedBackoff)
			s.mu.Unlock()
		}
		close(c.done)
	}()
	stopPipes := context.AfterFunc(childCtx, func() { in.Close(); out.Close() })
	defer stopPipes()
	startup := time.AfterFunc(TelegramWait, cancel)
	defer startup.Stop()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var hello telegramReply
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &hello) != nil || !hello.Ready || hello.Version != 1 {
		return false
	}
	startup.Stop()
	if childCtx.Err() != nil {
		return false
	}
	s.mu.Lock()
	s.conn = c
	s.unsupportedUntil = time.Time{}
	s.mu.Unlock()
	for scanner.Scan() {
		var reply telegramReply
		if json.Unmarshal(scanner.Bytes(), &reply) != nil || reply.ID == "" {
			break
		}
		select {
		case c.replies <- reply:
		case <-childCtx.Done():
			return true
		}
	}
	return true
}

func (s *TelegramStream) discard(c *telegramConnection) {
	s.mu.Lock()
	if s.conn == c {
		s.conn = nil
	}
	s.mu.Unlock()
	c.cancel()
	c.in.Close()
	s.retry()
}

// request returns used=false only when no request bytes reached the stream.
func (s *TelegramStream) request(ctx context.Context, request telegramRequest) (reply telegramReply, used bool, err error) {
	if !s.sendMu.TryLock() {
		return reply, false, nil
	}
	defer s.sendMu.Unlock()
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return reply, false, nil
	}
	select {
	case <-c.done:
		return reply, false, nil
	default:
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return reply, true, err
	}
	request.ID = hex.EncodeToString(nonce)
	b, err := json.Marshal(request)
	if err != nil {
		return reply, true, err
	}
	b = append(b, '\n')
	type written struct {
		n   int
		err error
	}
	wrote := make(chan written, 1)
	go func() { n, e := c.in.Write(b); wrote <- written{n, e} }()
	uncertain := func(why string) (telegramReply, bool, error) {
		s.discard(c)
		return telegramReply{}, true, fmt.Errorf("telegram: %s; %w", why, session.ErrMaybeSent)
	}
	var w written
	select {
	case w = <-wrote:
	case <-c.done:
		s.discard(c)
		select {
		case w = <-wrote:
		case <-ctx.Done():
			return uncertain("sender closed with an unknown write result")
		}
	case <-ctx.Done():
		s.discard(c)
		select {
		case w = <-wrote:
		default:
			return uncertain("sender request timed out with an unknown write result")
		}
	}
	if w.err != nil || w.n != len(b) {
		s.discard(c)
		if w.n == 0 {
			return reply, false, nil
		}
		return uncertain("sender disconnected during request")
	}
	select {
	case reply = <-c.replies:
	case <-c.done:
		select {
		case reply = <-c.replies:
		default:
			return uncertain("sender closed before acknowledgement")
		}
	case <-ctx.Done():
		return uncertain("sender acknowledgement timed out")
	}
	if reply.ID != request.ID {
		return uncertain("sender acknowledgement does not match the request")
	}
	if reply.OK == nil {
		return uncertain("sender returned an incomplete acknowledgement")
	}
	if !*reply.OK {
		if reply.MaybeSent == nil || *reply.MaybeSent || reply.Error == "" {
			return uncertain(reply.Error)
		}
		if request.Op == "ping" {
			return uncertain(reply.Error)
		}
		return reply, true, errors.New("telegram: " + reply.Error)
	}
	if request.Op == "ping" {
		if !reply.Alive {
			return uncertain("sender health probe was not confirmed")
		}
	} else {
		if len(reply.MessageIDs) == 0 {
			return uncertain("sender returned no message id")
		}
		for _, v := range reply.MessageIDs {
			if v <= 0 {
				return uncertain("sender returned an invalid message id")
			}
		}
	}
	return reply, true, nil
}

type telegramRequest struct {
	ID    string `json:"id"`
	Op    string `json:"op,omitempty"`
	Chat  string `json:"chat,omitempty"`
	Topic int64  `json:"topic,omitempty"`
	Text  string `json:"text,omitempty"`
}

func (s *TelegramStream) send(ctx context.Context, t *TelegramTarget, text string) (int64, bool, error) {
	topic := t.Topic
	if topic <= 0 {
		topic = 1
	}
	reply, used, err := s.request(ctx, telegramRequest{Chat: t.Chat, Topic: topic, Text: text})
	if err != nil || !used {
		return 0, used, err
	}
	return reply.MessageIDs[len(reply.MessageIDs)-1], true, nil
}

// Check validates both SSH and Telegram through a read-only API request.
// A busy sender is left alone; it already has a request in flight.
func (s *TelegramStream) Check(ctx context.Context) bool {
	reply, used, err := s.request(ctx, telegramRequest{Op: "ping"})
	if !used {
		return false
	}
	if err != nil || !reply.Alive {
		return false
	}
	return true
}

func (s *TelegramStream) signals() (chan struct{}, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
		s.probe = make(chan struct{}, 1)
	}
	return s.wake, s.probe
}
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (s *TelegramStream) retry() { wake, _ := s.signals(); poke(wake) }

// Ensure is nonblocking: take-start uses the speaking time to validate an
// existing stream or interrupt reconnect backoff for a cold one.
// A capability refusal keeps its periodic retry until wake/network resets it.
func (s *TelegramStream) Ensure() {
	_, probe := s.signals()
	if s.Ready() {
		poke(probe)
	} else {
		s.reconnect(false)
	}
}

// Reconnect invalidates startup as well as a published connection after wake
// or a network change. Requests already written remain possibly sent.
func (s *TelegramStream) Reconnect() {
	s.reconnect(true)
}

func (s *TelegramStream) reconnect(force bool) {
	s.mu.Lock()
	if !force && time.Now().Before(s.unsupportedUntil) {
		s.mu.Unlock()
		return
	}
	if force {
		s.forceWake = true
		s.unsupportedUntil = time.Time{}
	}
	s.conn = nil
	cancel := s.activeCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.retry()
}
