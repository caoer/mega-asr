package app

import (
	"os"
	"path/filepath"
	"testing"
)

// A cloud engine is Named only while the config runs it, as asr.engine or
// in compare.engines with compare.on; the page's list and the function's
// lookup read the config file alike, as it reads at each call, and keep the
// running config's answer when the file no longer loads.
func TestRetranscribersNameWhatTheConfigRuns(t *testing.T) {
	dir := isolate(t)
	keyFile := filepath.Join(t.TempDir(), "doubao.key")
	if err := os.WriteFile(keyFile, []byte("a key written for the test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := "[asr.doubao]\nkey_file = \"" + keyFile + "\"\n"
	for _, c := range []struct {
		name, body string
		named      bool
	}{
		{"compare off", "[asr]\nengine = \"funasr\"\n[compare]\non = false\nengines = [\"funasr\", \"doubao\"]\n", false},
		{"compare on", "[asr]\nengine = \"funasr\"\n[compare]\non = true\nengines = [\"funasr\", \"doubao\"]\n", true},
		{"compare on without it", "[asr]\nengine = \"funasr\"\n[compare]\non = true\nengines = [\"funasr\"]\n", false},
		{"the primary", "[asr]\nengine = \"doubao\"\n", true},
		{"a file that no longer loads", "[asr\n", Default().Uses("doubao")},
	} {
		body := c.body
		if c.name != "a file that no longer loads" {
			body += key
		}
		o := LoadOpts{Path: writeConfig(t, dir, body)}
		find := Default().Retranscriber(o)
		cloud, err := find("doubao")
		if err != nil || !cloud.Cloud || cloud.Named != c.named || cloud.File == nil {
			t.Errorf("%s: doubao %+v, %v; want named %v", c.name, cloud, err, c.named)
		}
		if local, err := find("funasr"); err != nil || local.Cloud || !local.Named || local.File != nil {
			t.Errorf("%s: funasr %+v, %v", c.name, local, err)
		}
		if _, err := find("cloudasr"); err == nil {
			t.Errorf("%s: an engine no config has was found", c.name)
		}
		listed := map[string]bool{}
		for _, e := range Default().TakesEngines(o)() {
			listed[e.Name] = e.Named
		}
		if len(listed) != 2 || listed["doubao"] != c.named || !listed["funasr"] {
			t.Errorf("%s: the page lists %v; want doubao named %v", c.name, listed, c.named)
		}
	}
}
