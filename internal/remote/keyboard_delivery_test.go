package remote

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type deliverySession struct {
	write  func(context.Context, []byte) error
	closed chan struct{}
	once   sync.Once
	code   atomic.Int32
}

func (s *deliverySession) Read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *deliverySession) Write(ctx context.Context, b []byte) error { return s.write(ctx, b) }
func (s *deliverySession) Close(code websocket.StatusCode, _ string, _ []byte) {
	s.once.Do(func() { s.code.Store(int32(code)); close(s.closed) })
}
func (*deliverySession) Addr() string { return "fixture session" }

func runDeliveryWriter(t *testing.T, c *conn) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); c.writer() }()
	t.Cleanup(func() {
		c.close(websocket.StatusNormalClosure, "fixture cleanup", nil)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("writer did not exit")
		}
	})
}

func TestPhoneTextWriteFailureAndStalledWriter(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		name := "write_error"
		if stalled {
			name = "stalled_writer"
		}
		t.Run(name, func(t *testing.T) {
			s := &deliverySession{closed: make(chan struct{})}
			s.write = func(ctx context.Context, _ []byte) error {
				if !stalled {
					return errors.New("fixture write failure")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-s.closed:
					return errGone
				}
			}
			c := newConn(s, false)
			runDeliveryWriter(t, c)
			result := make(chan error, 1)
			go func() { result <- c.DeliverText("take1", "hello") }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("failed writer reported delivery success")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("delivery did not return")
			}
			select {
			case <-s.closed:
			case <-time.After(time.Second):
				t.Fatal("failed writer left session open")
			}
			if s.code.Load() != int32(websocket.StatusInternalError) && !(stalled && s.code.Load() == int32(websocket.StatusTryAgainLater)) {
				t.Fatalf("close code %d", s.code.Load())
			}
		})
	}
}

func TestPhoneTextFullFIFODeadline(t *testing.T) {
	s := &deliverySession{closed: make(chan struct{}), write: func(context.Context, []byte) error { return nil }}
	c := newConn(s, false)
	for range cap(c.out) {
		c.out <- textDelivery{message: []byte(`{}`)}
	}
	result := make(chan error, 1)
	go func() { result <- c.DeliverText("take1", "hello") }()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("full FIFO: %v", err)
		}
	case <-time.After(8 * time.Second):
		c.close(websocket.StatusNormalClosure, "fixture cleanup", nil)
		t.Fatal("full FIFO did not time out")
	}
	if s.code.Load() != int32(websocket.StatusTryAgainLater) {
		t.Fatalf("close code %d", s.code.Load())
	}
}

func TestPhoneTextExactEncodedBoundary(t *testing.T) {
	encodedLen := func(text string) int {
		b, err := json.Marshal(map[string]any{"type": "text", "take": "take1", "text": text, "now_ms": time.Now().UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	text := strings.Repeat("&\"\\\n字", 200)
	text += strings.Repeat("x", MaxTextMessage-encodedLen(text))
	if encodedLen(text) != MaxTextMessage {
		t.Fatal("fixture is not on the encoded byte boundary")
	}
	for _, extra := range []int{0, 1} {
		name := "exact_limit"
		if extra != 0 {
			name = "one_byte_over"
		}
		t.Run(name, func(t *testing.T) {
			written := make(chan []byte, 1)
			s := &deliverySession{closed: make(chan struct{}), write: func(_ context.Context, b []byte) error { written <- append([]byte(nil), b...); return nil }}
			c := newConn(s, false)
			runDeliveryWriter(t, c)
			message := text + strings.Repeat("x", extra)
			err := c.DeliverText("take1", message)
			if extra != 0 {
				if err == nil {
					t.Fatal("oversize message accepted")
				}
				select {
				case <-written:
					t.Fatal("oversize message reached writer")
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b := <-written
			if len(b) != MaxTextMessage {
				t.Fatalf("written %d bytes", len(b))
			}
			var got struct {
				Type, Take, Text string
				NowMS            int64 `json:"now_ms"`
			}
			if err := json.Unmarshal(b, &got); err != nil || got.Text != message || got.Take != "take1" || got.Type != "text" || got.NowMS <= 0 {
				t.Fatalf("message round trip: %+v %v", got, err)
			}
		})
	}
}
