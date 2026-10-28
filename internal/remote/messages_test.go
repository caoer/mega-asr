package remote

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateMessages = flag.Bool("update-messages", false, "regenerate testdata/remote-messages.json")

type messageVector struct {
	Name string `json:"name"`
	Text string `json:"text"` // the message as the Mac sends it
}

// makeMessageVectors is the Mac's messages to a phone as this package's
// encoder writes them, with now_ms fixed.
func makeMessageVectors() []messageVector {
	const (
		epoch = "AAECAwQFBgc" // 8 bytes, b64u
		now   = 1770000000123
		take  = "20261020-101500"
	)
	st := func(phase, take string, elapsed int64, seq uint64, owner string) State {
		return State{Phase: phase, Take: take, ElapsedMS: elapsed, Epoch: epoch, Seq: seq, Owner: owner}
	}
	recent := []Recent{
		{Take: take, Final: "", AtMS: 1770000000000},
		{Take: "20261020-101200", Final: "undelivered", Why: "deliver_failed", AtMS: 1769999820000},
	}
	msgs := []struct {
		name string
		v    any
	}{
		{"hello", helloMsg{Type: "hello", Mac: "studio", State: st("idle", "", 0, 1, ""), Recent: recent}},
		{"reply ok", reply{Type: "reply", ID: "c1", OK: true, Take: take, State: st("recording", take, 0, 2, "stream"), Recent: recent}},
		{"reply other_take", reply{Type: "reply", ID: "c2", Code: "other_take", Reason: ReasonStreamTake(), State: st("recording", take, 2500, 2, "stream"), Recent: recent}},
		{"state idle", stateMsg{Type: "state", State: st("idle", "", 0, 3, "")}},
		{"state recording mac", stateMsg{Type: "state", State: st("recording", "20261020-101800", 1500, 4, "mac")}},
		{"state recording stream", stateMsg{Type: "state", State: st("recording", take, 1500, 5, "stream")}},
		{"state transcribing", stateMsg{Type: "state", State: st("transcribing", "", 0, 6, "stream")}},
		{"take", takeMsg{Type: "take", Take: take, Final: "undelivered", Why: "deliver_failed"}},
		{"text", textMsg{Type: "text", Take: take, Text: "你好，\"Mega\" \\ <Voice>"}},
	}
	out := make([]messageVector, len(msgs))
	for i, m := range msgs {
		out[i] = messageVector{Name: m.name, Text: string(encodeAt(m.v, now))}
	}
	return out
}

// The messages live once beside Go; Swift reads this exact generated file
// (ios/MegaVoiceKit/Tests/MacRemoteTests/MacMessagesTests.swift).
func TestRemoteMessageVectors(t *testing.T) {
	vectors := makeMessageVectors()
	want, err := json.MarshalIndent(struct {
		Messages []messageVector `json:"messages"`
	}{vectors}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	const path = "testdata/remote-messages.json"
	if *updateMessages {
		if err := os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("remote messages differ: regenerate with -update-messages")
	}
	for _, v := range vectors {
		owner := strings.Contains(v.Text, `"owner":`)
		switch {
		case !strings.HasSuffix(v.Text, `,"now_ms":1770000000123}`):
			t.Errorf("%s: no now_ms last: %s", v.Name, v.Text)
		case strings.Contains(v.Name, "idle") || v.Name == "hello":
			if owner {
				t.Errorf("%s: an idle state names an owner: %s", v.Name, v.Text)
			}
		case strings.HasPrefix(v.Name, "state") || strings.HasPrefix(v.Name, "reply"):
			if !owner {
				t.Errorf("%s: no owner: %s", v.Name, v.Text)
			}
		}
	}
}
