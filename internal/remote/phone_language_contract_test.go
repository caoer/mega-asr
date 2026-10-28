package remote

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/caoer/mega-asr/internal/l10n"
)

func TestPhoneBusyReasonLanguageContract(t *testing.T) {
	data, err := os.ReadFile("../../ios/MegaVoiceKit/Tests/MacRemoteTests/Resources/MacSessionReasons.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Language string `json:"language"`
		Busy     string `json:"busy"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	original := l10n.Current()
	defer l10n.Set(original)
	for _, row := range rows {
		l10n.Set(l10n.FromTag(row.Language))
		if got := reasonBusy(); got != row.Busy {
			t.Errorf("%s: producer %q, contract %q", row.Language, got, row.Busy)
		}
	}
}
