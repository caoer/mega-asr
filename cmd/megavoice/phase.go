package main

import (
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/session"
)

// phaseOf tells the key tap which keys a key phase (session.Controller's
// KeyPhase: the Mac's own takes alone) takes. A take an iPhone keyboard
// streams is not in it, so Return and Esc reach the frontmost app while
// only such a take is on its way.
func phaseOf(keys session.State) mac.Phase {
	switch keys {
	case session.Recording:
		return mac.PhaseRecording
	case session.Transcribing, session.Delivering:
		return mac.PhaseProcessing
	}
	return mac.PhaseIdle
}

// tapPhase is phaseOf for the Mac's own key tap: while a take waits on the
// model step (session.Controller's Waiting) Esc is taken too, and cancels
// it.
func tapPhase(keys session.State, waiting bool) mac.Phase {
	if waiting {
		return mac.PhaseWaiting
	}
	return phaseOf(keys)
}
