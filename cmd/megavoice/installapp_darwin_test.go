//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installRig runs scripts/install-app.sh on a scratch bundle under a
// temporary HOME, with launchctl, codesign, security and the bundle's own
// megavoice replaced by fakes that log their calls. The fake megavoice exits
// with the codes in codes, one per call, the last one repeating.
type installRig struct {
	dir, home, src, log string
	env                 []string
}

func newInstallRig(t *testing.T, loaded bool) *installRig {
	t.Helper()
	dir := t.TempDir()
	r := &installRig{dir: dir, home: filepath.Join(dir, "home"), src: filepath.Join(dir, "src", "MegaVoiceTest.app"), log: filepath.Join(dir, "calls")}
	bin := filepath.Join(dir, "bin")
	print := "exit 1"
	if loaded {
		print = "exit 0"
	}
	fakes := map[string]string{
		"launchctl": `echo "launchctl $*" >>"$FAKE_LOG"; [ "$1" = print ] && { ` + print + `; }; exit 0`,
		"codesign":  `echo "codesign $1" >>"$FAKE_LOG"; exit 0`,
		"security":  `echo '  1) 0000 "Apple Development: Alice Example (TEAM000)"'`,
	}
	for name, body := range fakes {
		writeExec(t, filepath.Join(bin, name), "#!/bin/sh\n"+body+"\n")
	}
	writeExec(t, filepath.Join(r.src, "Contents", "MacOS", "megavoice"), `#!/bin/sh
echo "megavoice $*" >>"$FAKE_LOG"
n=$(grep -c '^megavoice' "$FAKE_LOG")
code=$(sed -n "${n}p" "$FAKE_CODES")
[ -n "$code" ] || code=$(tail -1 "$FAKE_CODES")
[ "$code" = 0 ] || echo "a take is in flight" >&2
exit "$code"
`)
	os.WriteFile(filepath.Join(r.src, "Contents", "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>app.example.megavoice.test</string>
<key>CFBundleExecutable</key><string>megavoice</string>
</dict></plist>
`), 0o644)
	r.env = append(os.Environ(), "HOME="+r.home, "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_LOG="+r.log,
		"FAKE_CODES="+filepath.Join(dir, "codes"), "MEGAVOICE_RESTART_WAIT=3", "MEGAVOICE_SIGN_IDENTITY=")
	return r
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// run runs the script with the fake megavoice answering codes; it returns
// the exit code, the output, and the calls this run made.
func (r *installRig) run(t *testing.T, codes ...string) (int, string, []string) {
	t.Helper()
	os.WriteFile(filepath.Join(r.dir, "codes"), []byte(strings.Join(codes, "\n")+"\n"), 0o644)
	os.Remove(r.log)
	cmd := exec.Command("sh", "../../scripts/install-app.sh", r.src)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.log)
	return code, string(out), strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
}

func (r *installRig) stamped() bool {
	b, err := os.ReadFile(filepath.Join(r.home, "Applications", ".MegaVoiceTest.app.restarted"))
	return err == nil && strings.HasPrefix(string(b), r.src+" ")
}

func (r *installRig) installed() bool {
	_, err := os.Stat(filepath.Join(r.home, "Applications", "MegaVoiceTest.app", "Contents", "Resources", "source"))
	return err == nil
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// The agent accepts restart --when-idle: the stamp is written after it, and
// the next run is a no-op.
func TestInstallAppRestartAccepted(t *testing.T) {
	r := newInstallRig(t, true)
	code, out, calls := r.run(t, "0")
	if code != 0 || !r.installed() || !r.stamped() || count(calls, "megavoice_restart_--when-idle") != 1 || count(calls, "launchctl_kickstart") != 0 {
		t.Fatalf("exit %d, installed %v, stamped %v, calls %q\n%s", code, r.installed(), r.stamped(), calls, out)
	}
	if !strings.Contains(out, "accepted the restart") {
		t.Errorf("output %s", out)
	}
	code, out, calls = r.run(t, "0")
	if code != 0 || count(calls, "megavoice") != 0 || count(calls, "codesign_--force") != 0 || count(calls, "launchctl") != 0 {
		t.Fatalf("second run: exit %d, calls %q\n%s", code, calls, out)
	}
}

// An agent that refuses for the whole wait: the run fails with the bundle
// installed and no stamp; the next run installs nothing and asks again.
func TestInstallAppRestartRefused(t *testing.T) {
	r := newInstallRig(t, true)
	code, out, calls := r.run(t, "75")
	if code != 1 || !r.installed() || r.stamped() || count(calls, "megavoice_restart") < 2 || count(calls, "launchctl_kickstart") != 0 {
		t.Fatalf("exit %d, installed %v, stamped %v, calls %q\n%s", code, r.installed(), r.stamped(), calls, out)
	}
	if !strings.Contains(out, "refused the restart (megavoice: a take is in flight") && !strings.Contains(out, "refused the restart (a take is in flight") {
		t.Errorf("output %s", out)
	}
	code, out, calls = r.run(t, "75", "0")
	if code != 0 || !r.stamped() || count(calls, "codesign_--force") != 0 || count(calls, "megavoice_restart") != 2 {
		t.Fatalf("next run: exit %d, stamped %v, calls %q\n%s", code, r.stamped(), calls, out)
	}
}

// An agent launchd knows but that is not running is started with
// kickstart -k; one launchd does not know is left alone. Both stamp.
func TestInstallAppAgentDown(t *testing.T) {
	r := newInstallRig(t, true)
	code, out, calls := r.run(t, "69")
	if code != 0 || !r.stamped() || count(calls, "launchctl_kickstart_-k") != 1 {
		t.Fatalf("not running: exit %d, stamped %v, calls %q\n%s", code, r.stamped(), calls, out)
	}
	r = newInstallRig(t, false)
	code, out, calls = r.run(t, "0")
	if code != 0 || !r.stamped() || count(calls, "megavoice") != 0 || count(calls, "launchctl_kickstart") != 0 {
		t.Fatalf("unknown agent: exit %d, stamped %v, calls %q\n%s", code, r.stamped(), calls, out)
	}
}
