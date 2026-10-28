package remote

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/relay/e2e"
	"github.com/caoer/mega-asr/internal/relay/wire"
)

func TestOneShotWatchCannotStopPhoneStream(t *testing.T) {
	f := &streamHost{fakeTakes: &fakeTakes{}, got: make(chan []int16, 1)}
	s := upWith(t, f)
	f.s = s
	l, id := withStubRelay(t, s)
	r := &stubRoom{t: t, s: s, f: f.fakeTakes, l: l, id: id}
	phoneSK, watchSK := r.phone("one")
	_, sess, _ := r.handshake(sidOf(1), phoneSK, `{"type":"hello","device":"one"}`)
	r.send(sidOf(1), sess, streamStart("start"))
	start := records(t, r.waitSent(sidOf(1), 2)[1:], sess)[0]
	if start["ok"] != true {
		t.Fatal(start)
	}
	stop := cmd("watch-stop", "stop", "send", time.Now())
	stop["take"] = "take1"
	b, _ := json.Marshal(stop)
	body, pending, err := e2e.SealRequest(r.id.Kem.PublicKey(), r.id.Room, watchSK, uint64(time.Now().UnixMilli()), [wire.NonceLen]byte{1}, b)
	if err != nil {
		t.Fatal(err)
	}
	r.up(sidOf(2), true)
	s.Frame(sidOf(2), body)
	plain, _, err := pending.Finish(r.waitSent(sidOf(2), 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(plain, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["ok"] != false || reply["code"] != "other_take" {
		t.Fatal(reply)
	}
	if state := f.State(); state.Take != "take1" || state.Phase != "recording" {
		t.Fatalf("watch cut source: %+v", state)
	}
	// The owner's authenticated record path still finishes normally.
	audio, _ := (StreamRecord{Stream: 0x0123456789abcdef, Last: true, Samples: []int16{1000}}).Encode()
	rec, err := sess.Seal(e2e.KindAudio, audio[1:])
	if err != nil {
		t.Fatal(err)
	}
	s.Frame(sidOf(1), rec)
	select {
	case <-f.got:
	case <-time.After(time.Second):
		t.Fatal("owner source did not finish")
	}
}
