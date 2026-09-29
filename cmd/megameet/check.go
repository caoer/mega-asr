package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/caoer/mega-asr/internal/app"
)

// checkCmd reports whether a pulled directory holds what the ingest reads
// (docs/megameet.md § Directory contract): exit 0 when it does, else each
// thing missing on its own line.
func checkCmd(o app.LoadOpts, args []string) error {
	if len(args) != 1 {
		return errors.New("check <id|DIR>")
	}
	dir := args[0]
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		l, err := app.Load(o)
		if err != nil {
			return err
		}
		dir = filepath.Join(l.Meeting.Data, "pull", strings.TrimPrefix(args[0], "rec."))
	}
	summary, missing := checkDir(dir)
	if len(missing) > 0 {
		for _, m := range missing {
			fmt.Println("missing:", m)
		}
		return fmt.Errorf("%s is incomplete (%d)", dir, len(missing))
	}
	fmt.Printf("complete: %s (%s)\n", dir, summary)
	return nil
}

// pulledMeta is the part of a pulled directory's meta.toml the contract
// names.
type pulledMeta struct {
	ID      string `toml:"id"`
	Source  string `toml:"source"`
	Started any    `toml:"started"`
	Tracks  []struct {
		Name   string `toml:"name"`
		Role   string `toml:"role"`
		File   string `toml:"file"`
		SHA256 string `toml:"sha256"`
		Bytes  int64  `toml:"bytes"`
	} `toml:"tracks"`
	Feishu *struct {
		Token string `toml:"token"`
	} `toml:"feishu"`
}

// checkDir lists what dir lacks; summary describes a complete one.
func checkDir(dir string) (summary string, missing []string) {
	miss := func(format string, a ...any) { missing = append(missing, fmt.Sprintf(format, a...)) }
	var m pulledMeta
	if _, err := toml.DecodeFile(filepath.Join(dir, "meta.toml"), &m); err != nil {
		miss("meta.toml (%v)", err)
		return "", missing
	}
	for k, v := range map[string]bool{"id": m.ID != "", "source": m.Source != "", "started": m.Started != nil} {
		if !v {
			miss("meta.toml: %s", k)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "rec.json")); err != nil {
		miss("rec.json (the record as pulled)")
	}
	for _, t := range m.Tracks {
		p := filepath.Join(dir, t.File)
		if t.File == "" {
			miss("meta.toml: tracks %q has no file", t.Role)
			continue
		}
		sum, n, err := fileSHA(p)
		switch {
		case err != nil:
			miss("%s (track %s)", t.File, t.Role)
		case t.SHA256 != "" && sum != t.SHA256:
			miss("%s: sha256 %s, meta.toml says %s", t.File, sum, t.SHA256)
		case t.Bytes > 0 && n != t.Bytes:
			miss("%s: %d bytes, meta.toml says %d", t.File, n, t.Bytes)
		}
	}
	transcriptOnly := m.Source == "feishu" && len(m.Tracks) == 0
	if len(m.Tracks) == 0 && !transcriptOnly {
		miss("tracks: meta.toml lists none")
	}
	switch m.Source {
	case "feishu":
		if !jsonHas(filepath.Join(dir, "feishu-transcript.json"), "segments") {
			miss("feishu-transcript.json (Feishu's turns)")
		}
	case "room":
		if _, err := os.Stat(filepath.Join(dir, "doa.jsonl")); err != nil {
			miss("doa.jsonl (the room box's azimuths)")
		}
	}
	segs := 0
	if !transcriptOnly {
		var s []json.RawMessage
		b, err := os.ReadFile(filepath.Join(dir, "segments.json"))
		if err == nil {
			err = json.Unmarshal(b, &s)
		}
		if err != nil {
			miss("segments.json (megameet process)")
		}
		segs = len(s)
		if _, err := os.Stat(filepath.Join(dir, "transcript.md")); err != nil {
			miss("transcript.md (megameet process)")
		}
	}
	return fmt.Sprintf("%s, %d track(s), %d segment(s)", m.Source, len(m.Tracks), segs), missing
}

func jsonHas(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var v map[string]json.RawMessage
	return json.Unmarshal(b, &v) == nil && v[key] != nil
}
