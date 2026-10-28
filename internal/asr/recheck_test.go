package asr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRechecking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rechecks.tsv")
	list := "# trigger<TAB>terms\npassword\tpasskey\nPaas key\tpasskey, Passkey\nno tab here\nempty\t \n"
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := ReadRechecks(path, filepath.Join(t.TempDir(), "missing.tsv"))
	if len(rs) != 2 || rs[0].Trigger != "password" || !slices.Equal(rs[1].Terms, []string{"passkey", "Passkey"}) {
		t.Fatalf("ReadRechecks = %+v", rs)
	}

	base := []string{"Agent", "EasyTier"}
	var calls [][]string
	fail := false
	tr := TranscribeFunc(func(ctx context.Context, wav string, whole bool) (string, error) {
		hw := withExtra(ctx, base)
		calls = append(calls, hw)
		if len(calls) == 2 && fail {
			return "", errors.New("boom")
		}
		if slices.Contains(hw, "passkey") {
			return "the passkey", nil
		}
		return map[string]string{"a.wav": "这个 Agent", "b.wav": "the PASSWORD"}[wav], nil
	})
	r := Rechecking{Transcriber: tr, Rechecks: func() []Recheck { return rs }}
	for _, c := range []struct {
		wav, want string
		fail      bool
		decodes   int
	}{
		{"a.wav", "这个 Agent", false, 1},    // no trigger: the first decode, once
		{"b.wav", "the passkey", false, 2}, // trigger in any case: decoded again with passkey added
		{"b.wav", "the PASSWORD", true, 2}, // a failed second decode keeps the first
	} {
		calls, fail = nil, c.fail
		got, err := r.Transcribe(context.Background(), c.wav, false)
		if err != nil || got != c.want || len(calls) != c.decodes {
			t.Errorf("%s: %q, %v after %d decodes; want %q after %d", c.wav, got, err, len(calls), c.want, c.decodes)
		}
		if !slices.Equal(calls[0], base) {
			t.Errorf("%s: first decode's hot words %q", c.wav, calls[0])
		}
		if c.decodes == 2 && strings.Join(calls[1], ",") != "Agent,EasyTier,passkey" {
			t.Errorf("%s: second decode's hot words %q", c.wav, calls[1])
		}
	}

	full := make([]string, MaxHotwords)
	for i := range full {
		full[i] = strings.Repeat("x", i+1)
	}
	hw := withExtra(context.WithValue(context.Background(), extraKey{}, []string{"passkey"}), full)
	if len(hw) != MaxHotwords || hw[MaxHotwords-1] != "passkey" || hw[MaxHotwords-2] != full[MaxHotwords-2] {
		t.Errorf("full list: %d terms, last %q", len(hw), hw[len(hw)-2:])
	}
}
