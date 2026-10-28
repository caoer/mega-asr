package main

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/remote"
	"github.com/caoer/mega-asr/internal/session"
)

// The receiving Swift tests use these same producer spellings.
func TestPhoneSessionReasonLanguageContract(t *testing.T) {
	data, err := os.ReadFile("../../ios/MegaVoiceKit/Tests/MacRemoteTests/Resources/MacSessionReasons.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Language         string `json:"language"`
		Recording        string `json:"recording"`
		Transcribing     string `json:"transcribing"`
		Resending        string `json:"resending"`
		Retranscribing   string `json:"retranscribing"`
		SaveFailed       string `json:"saveFailed"`
		PrimaryRecording string `json:"primaryRecording"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	original := l10n.Current()
	defer l10n.Set(original)
	for _, row := range rows {
		t.Run(row.Language, func(t *testing.T) {
			l10n.Set(l10n.FromTag(row.Language))
			for _, pair := range [][2]string{
				{session.ErrRecording.Error(), row.Recording},
				{session.ErrTranscribing.Error(), row.Transcribing},
				{session.ErrResending.Error(), row.Resending},
				{session.ErrRetranscribing.Error(), row.Retranscribing},
				{session.MsgSaveFailed(), row.SaveFailed},
			} {
				if pair[0] != pair[1] {
					t.Errorf("producer %q, contract %q", pair[0], pair[1])
				}
			}
			producer := phoneTakes{ctrl: &fakePhoneCtrl{startErr: session.ErrRecording}}
			_, err := producer.Start("watch")
			var refusal *remote.Refusal
			if !errors.As(err, &refusal) || refusal.Code != "recording" || refusal.Reason != row.PrimaryRecording {
				t.Fatalf("primary recording refusal: %v; want recording / %q", err, row.PrimaryRecording)
			}
		})
	}
}
