package remote

import (
	"strings"
	"testing"
)

func TestCPUDiagnosticRequiresAuthenticationAndLogsBeforeReceipt(t *testing.T) {
	logs := &logLines{}
	s := upConfig(t, Config{Logf: logs.logf})
	c, err := dial(t, s, s.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send(t, c, map[string]any{"type": "diagnostic", "id": "unauth", "name": "cpu_decode", "payload": "{}"})
	recv(t, c, "refused")
	if strings.Contains(logs.text(), "diagnostic cpu_decode") {
		t.Fatal("unauthenticated result logged")
	}

	p, err := s.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	q := secretOf(t, p)
	h, err := dial(t, s, q.Get("fp"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.CloseNow()
	send(t, h, map[string]any{"type": "pair", "secret": q.Get("s"), "device": "CPU test phone"})
	recv(t, h, "paired")
	send(t, h, map[string]any{"type": "diagnostic", "id": "run-1", "name": "cpu_decode", "payload": "{\n\"decode_s\":2.5\n}"})
	got := recv(t, h, "reply")
	if got["ok"] != true || got["id"] != "run-1" {
		t.Fatalf("receipt: %v", got)
	}
	if !strings.Contains(logs.text(), `diagnostic cpu_decode from "CPU test phone" id run-1: {"decode_s":2.5}`) {
		t.Fatalf("missing compact result: %s", logs.text())
	}
	for _, payload := range []string{"invalid", strings.Repeat(" ", 3001)} {
		send(t, h, map[string]any{"type": "diagnostic", "id": "bad", "name": "cpu_decode", "payload": payload})
		if recv(t, h, "reply")["ok"] != false {
			t.Fatal("invalid result accepted")
		}
	}
}
