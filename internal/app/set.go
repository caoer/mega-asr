package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Value is a --set style value as TOML: a valid TOML value stays as it is,
// a bare word becomes a string.
func Value(v string) string {
	if _, err := toml.Decode("k = "+v, new(map[string]any)); err != nil {
		return strconv.Quote(v)
	}
	return v
}

var (
	tableRE = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.\- ]+?)\s*\]\s*(#.*)?$`)
	keyRE   = regexp.MustCompile(`^\s*([A-Za-z0-9_\-]+(?:\s*\.\s*[A-Za-z0-9_\-]+)*)\s*=`)
)

// Set writes one key of the config file at path — key is dotted
// (meeting.capture.mic), val a TOML value — and keeps every other line,
// comments included. The key's line is replaced; a missing key goes under
// its table's header, a missing table at the end. The result must load as
// a valid config, check included (the caller's own, as LoadOpts.Check), or
// the file is left as it was. A symlink is written through.
func Set(path, key, val string, check func(Config) []string) error {
	dot := strings.LastIndex(key, ".")
	if dot <= 0 || dot == len(key)-1 {
		return fmt.Errorf("config set: %q: want section.key", key)
	}
	table, name := key[:dot], key[dot+1:]
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	out := setLine(string(b), table, name, name+" = "+val)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	l, err := Load(LoadOpts{Path: tmp.Name(), Check: check})
	if err != nil {
		return fmt.Errorf("config set %s: %w", key, err)
	}
	if l.SourceOf(key) != "file" {
		return fmt.Errorf("config set %s: the key does not read back from the file", key)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// setLine replaces or adds `line` for key name in table within doc.
func setLine(doc, table, name, line string) string {
	lines := strings.Split(doc, "\n")
	cur, header := "", -1
	for i, l := range lines {
		if m := tableRE.FindStringSubmatch(l); m != nil {
			cur = normKey(m[1])
			if cur == table {
				header = i
			}
			continue
		}
		m := keyRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		full := normKey(m[1])
		if cur != "" {
			full = cur + "." + full
		}
		if full == table+"."+name {
			indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
			if cur == "" {
				line = table + "." + line
			}
			lines[i] = indent + line
			return strings.Join(lines, "\n")
		}
	}
	if header >= 0 {
		lines = append(lines[:header+1], append([]string{line}, lines[header+1:]...)...)
		return strings.Join(lines, "\n")
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if doc != "" {
		doc += "\n"
	}
	return doc + "[" + table + "]\n" + line + "\n"
}

func normKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ".")
}
