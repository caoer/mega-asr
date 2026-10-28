package main

import (
	"testing"

	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

func TestPhaseOf(t *testing.T) {
	for st, want := range map[session.State]mac.Phase{
		session.Idle:         mac.PhaseIdle,
		session.Recording:    mac.PhaseRecording,
		session.Transcribing: mac.PhaseProcessing,
		session.Delivering:   mac.PhaseProcessing,
	} {
		if got := phaseOf(st); got != want {
			t.Errorf("phaseOf(%s) = %v, want %v", st, got, want)
		}
	}
}
