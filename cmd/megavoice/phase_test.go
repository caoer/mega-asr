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
		if got := tapPhase(st, false); got != want {
			t.Errorf("tapPhase(%s, false) = %v, want %v", st, got, want)
		}
	}
	// a take waits on the model step: the tap takes Esc for it
	if got := tapPhase(session.Transcribing, true); got != mac.PhaseWaiting {
		t.Errorf("tapPhase(transcribing, true) = %v, want %v", got, mac.PhaseWaiting)
	}
}
