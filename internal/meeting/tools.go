package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/caoer/mega-asr/scripts/megameet"
)

// Py runs scripts/megameet's helpers with `uv run --script`.
type Py struct {
	Dir string // where the scripts are written out, and their input files
}

// script writes the embedded script out unless the copy there is current.
func (p Py) script(name string) (string, error) {
	b, err := scripts.FS.ReadFile(name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.Dir, name)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b) {
		return path, nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func (p Py) run(ctx context.Context, v any, name string, args ...string) error {
	path, err := p.script(name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "uv", append([]string{"run", "--quiet", "--script", path}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("%s: %w: %.200s", name, err, out)
	}
	return nil
}

func (p Py) Offset(ctx context.Context, feishu string, local []string, expectS float64) (Search, error) {
	var s Search
	args := append([]string{"--expect", fmt.Sprintf("%.3f", expectS), feishu}, local...)
	return s, p.run(ctx, &s, "offset.py", args...)
}

func (p Py) CP(ctx context.Context, ref, hyp []Utterance, collarS float64) (CPResult, error) {
	var r CPResult
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return r, err
	}
	f, err := os.CreateTemp(p.Dir, "cp-*.json")
	if err != nil {
		return r, err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(map[string]any{"collar": collarS, "ref": ref, "hyp": hyp})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return r, err
	}
	return r, p.run(ctx, &r, "cpwer.py", f.Name())
}
