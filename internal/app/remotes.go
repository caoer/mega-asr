package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Remote is a mic server this Mac paired with (`megavoice mic add`):
// capture.remote names one. The token grants its mic, so remotes.toml is
// 0600 and config.toml holds only the name.
type Remote struct {
	Name        string `toml:"name"`        // the box's name
	Addr        string `toml:"addr"`        // host:port
	Fingerprint string `toml:"fingerprint"` // the certificate pairing confirmed
	Token       string `toml:"token"`
	Client      string `toml:"client"` // this Mac's name on the box: what `mic revoke` there takes
}

// ErrNoRemote is ForgetRemote's error for a name not paired.
var ErrNoRemote = errors.New("no paired remote mic by that name")

// RemotesPath is remotes.toml, beside config.toml.
func RemotesPath() string { return filepath.Join(ConfigDir(), "remotes.toml") }

type remotesFile struct {
	Remote []Remote `toml:"remote"`
}

// LoadRemotes reads remotes.toml; an absent file is no remotes.
func LoadRemotes(path string) ([]Remote, error) {
	var f remotesFile
	if _, err := toml.DecodeFile(path, &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Remote, nil
}

// FindRemote is the remote called name.
func FindRemote(rs []Remote, name string) (Remote, bool) {
	i := slices.IndexFunc(rs, func(r Remote) bool { return r.Name == name })
	if i < 0 {
		return Remote{}, false
	}
	return rs[i], true
}

// SaveRemote adds r, replacing a remote of the same name.
func SaveRemote(path string, r Remote) error {
	rs, err := LoadRemotes(path)
	if err != nil {
		return err
	}
	rs = slices.DeleteFunc(rs, func(o Remote) bool { return o.Name == r.Name })
	return saveRemotes(path, append(rs, r))
}

// ForgetRemote removes the remote called name.
func ForgetRemote(path, name string) error {
	rs, err := LoadRemotes(path)
	if err != nil {
		return err
	}
	if _, ok := FindRemote(rs, name); !ok {
		return fmt.Errorf("%q: %w", name, ErrNoRemote)
	}
	return saveRemotes(path, slices.DeleteFunc(rs, func(o Remote) bool { return o.Name == name }))
}

func saveRemotes(path string, rs []Remote) error {
	slices.SortFunc(rs, func(a, b Remote) int { return strings.Compare(a.Name, b.Name) })
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(remotesFile{Remote: rs}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".remotes-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
