package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/refine"
)

// RefineBaseURL is where the provider SetRefineKey adds answers: Anthropic's
// Messages API.
var RefineBaseURL = "https://api.anthropic.com"

// keyTestText is the dictation of the test call.
const keyTestText = "This is a test call from Mega Voice, to check that the key works."

// keyTestLimit bounds the test call.
const keyTestLimit = 20 * time.Second

// SetRefineKey keeps key as the model step's key once one call with it, as
// a take would make it, has an answer. It goes to the file the selected
// provider names; with no provider selected, refine.provider becomes
// anthropic, a new [refine.providers.anthropic] at RefineBaseURL whose key
// file is anthropic.key beside the config at path when there is no such
// table yet. A call that fails changes nothing, and the error says why.
// The key is in no error, log line or config value: the config names its
// file. It returns the test call's time.
func SetRefineKey(ctx context.Context, path, key string, check func(Config) []string) (time.Duration, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, errors.New("no key given")
	}
	scrub := func(err error) error { return errors.New(strings.ReplaceAll(err.Error(), key, "[key]")) }
	l, err := Load(LoadOpts{Path: path})
	if errors.Is(err, fs.ErrNotExist) {
		l, err = Load(LoadOpts{Path: os.DevNull}) // no file yet: the defaults
	}
	if err != nil {
		return 0, fmt.Errorf("the config does not load, nothing was changed: %w", scrub(err))
	}
	r := l.Refine
	name, sets := r.Provider, [][2]string(nil)
	p, ok := r.Providers[name]
	if !ok {
		name = "anthropic"
		sets = append(sets, [2]string{"refine.provider", strconv.Quote(name)})
		if p, ok = r.Providers[name]; !ok {
			p = RefineProviderConfig{Kind: "anthropic", BaseURL: RefineBaseURL, KeyFile: filepath.Join(filepath.Dir(path), "anthropic.key")}
			t := "refine.providers." + name + "."
			sets = append(sets, [2]string{t + "kind", strconv.Quote(p.Kind)}, [2]string{t + "base_url", strconv.Quote(p.BaseURL)},
				[2]string{t + "key_file", strconv.Quote(p.KeyFile)})
		}
	}
	if p.Kind != "anthropic" {
		return 0, fmt.Errorf("provider %s is kind %s, which this build has no client for; nothing was changed", name, p.Kind)
	}
	prompt, err := r.Prompt()
	if err != nil {
		return 0, fmt.Errorf("the prompt of mode %s does not read, nothing was changed: %w", r.Mode, err)
	}
	ctx, cancel := context.WithTimeout(ctx, keyTestLimit)
	defer cancel()
	t0 := time.Now()
	_, err = refine.Anthropic{BaseURL: p.BaseURL, Key: key}.Refine(ctx, refine.Request{Model: r.Model, Effort: r.Effort,
		System: []string{prompt}, Dictation: keyTestText, Output: refine.OutputOf(r.Mode)})
	took := time.Since(t0)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("no answer within %s", keyTestLimit)
		}
		return took, fmt.Errorf("the test call to %s (%s) failed, so the key was not kept: %w", name, r.Model, scrub(err))
	}
	if err := keepKey(path, p.KeyFile, key, sets, check); err != nil {
		return took, fmt.Errorf("the test call passed, but the key was not kept: %w", scrub(err))
	}
	return took, nil
}

// keepKey writes key to file, owner-only, and sets on the config at path,
// both or neither.
func keepKey(path, file, key string, sets [][2]string, check func(Config) []string) error {
	if p, err := filepath.EvalSymlinks(file); err == nil {
		file = p // a key file linked elsewhere is written where it is
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	var was []byte
	if len(sets) > 0 {
		was, err = os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := SetAll(path, sets, check); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		if len(sets) > 0 {
			if was == nil {
				os.Remove(path)
			} else if p, rerr := filepath.EvalSymlinks(path); rerr == nil {
				os.WriteFile(p, was, 0o644)
			}
		}
		return err
	}
	return nil
}
