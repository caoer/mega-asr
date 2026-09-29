package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSet(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"replace in table",
			"# top\n[meeting.capture]\n  mic = \"default\"  # the system's\napps = [\"us.zoom.xos\"]\n",
			"# top\n[meeting.capture]\n  mic = \"BuiltInMicrophoneDevice\"\napps = [\"us.zoom.xos\"]\n"},
		{"add under header",
			"[meeting.capture]\napps = [\"us.zoom.xos\"]\n\n[meeting.page]\nurl = \"https://pages.example\"\nslug = \"x\"\n",
			"[meeting.capture]\nmic = \"BuiltInMicrophoneDevice\"\napps = [\"us.zoom.xos\"]\n\n[meeting.page]\nurl = \"https://pages.example\"\nslug = \"x\"\n"},
		{"add table",
			"[tap]\nkey = \"right_shift\"",
			"[tap]\nkey = \"right_shift\"\n\n[meeting.capture]\nmic = \"BuiltInMicrophoneDevice\"\n"},
		{"dotted key at the root",
			"meeting.capture.mic = \"default\"\n[tap]\nkey = \"right_shift\"\n",
			"meeting.capture.mic = \"BuiltInMicrophoneDevice\"\n[tap]\nkey = \"right_shift\"\n"},
		{"absent file", "", "[meeting.capture]\nmic = \"BuiltInMicrophoneDevice\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if c.in != "" {
				if err := os.WriteFile(path, []byte(c.in), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Set(path, "meeting.capture.mic", Value("BuiltInMicrophoneDevice"), nil); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			if string(b) != c.want {
				t.Errorf("got\n%s\nwant\n%s", b, c.want)
			}
			l, err := Load(LoadOpts{Path: path})
			if err != nil || l.Meeting.Capture.Mic != "BuiltInMicrophoneDevice" {
				t.Errorf("load: %v, mic %q", err, l.Meeting.Capture.Mic)
			}
		})
	}
}

func TestSetRefusesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := "[meeting.capture]\nmic = \"default\"\n"
	os.WriteFile(path, []byte(in), 0o600)
	for _, kv := range [][2]string{{"meeting.capture.mic", `""`}, {"meeting.capture.nope", `"x"`}} {
		if err := Set(path, kv[0], kv[1], nil); err == nil {
			t.Errorf("%s = %s: no error", kv[0], kv[1])
		}
	}
	if b, _ := os.ReadFile(path); string(b) != in {
		t.Errorf("file changed: %s", b)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*")); len(m) > 0 {
		t.Errorf("temp files left: %s", strings.Join(m, " "))
	}
}
