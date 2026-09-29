package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRemotesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.toml")
	if rs, err := LoadRemotes(path); err != nil || rs != nil {
		t.Fatalf("absent file: %v %v", rs, err)
	}
	mb := Remote{Name: "micbox", Addr: "micbox:7866", Fingerprint: "ab12", Token: "tok1"}
	den := Remote{Name: "den", Addr: "198.51.100.143:7866", Fingerprint: "cd34", Token: "tok2"}
	for _, r := range []Remote{mb, den} {
		if err := SaveRemote(path, r); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v, want 0600: the file holds tokens", st.Mode().Perm(), err)
	}
	rs, err := LoadRemotes(path)
	if err != nil || !slices.Equal(rs, []Remote{den, mb}) {
		t.Fatalf("loaded %+v %v", rs, err)
	}

	mb.Addr, mb.Token = "203.0.113.76:7866", "tok3" // paired again
	if err := SaveRemote(path, mb); err != nil {
		t.Fatal(err)
	}
	if rs, _ = LoadRemotes(path); !slices.Equal(rs, []Remote{den, mb}) {
		t.Errorf("after re-pairing: %+v", rs)
	}
	if r, ok := FindRemote(rs, "micbox"); !ok || r != mb {
		t.Errorf("FindRemote: %+v %v", r, ok)
	}

	if err := ForgetRemote(path, "den"); err != nil {
		t.Fatal(err)
	}
	if rs, _ = LoadRemotes(path); !slices.Equal(rs, []Remote{mb}) {
		t.Errorf("after forget: %+v", rs)
	}
	if err := ForgetRemote(path, "den"); !errors.Is(err, ErrNoRemote) {
		t.Errorf("forget twice: %v", err)
	}
}
