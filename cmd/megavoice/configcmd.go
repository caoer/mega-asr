package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/caoer/mega-asr/internal/app"
)

const configUsage = `usage: megavoice [--config FILE] [--set KEY=VALUE]... config <command>

  show          the effective config, each value with its source (default, file, flag)
  check [FILE]  validate FILE (default: the config file megavoice reads)
  init [FILE]   write the commented default config to FILE (default: the config
                file megavoice reads; - for stdout); an existing file is kept
  set KEY VALUE write one key into the config file (a bare word is a string),
                keeping its other lines and comments; refused when the result
                does not load (e.g. set meeting.capture.mic <uid from megameet mics>)
`

func configCmd(o app.LoadOpts, args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, configUsage)
		return exitCode(2)
	}
	cmd, args := args[0], args[1:]
	switch {
	case cmd == "show" && len(args) == 0:
		l, err := load(o)
		if err != nil {
			return err
		}
		l.WriteEffective(os.Stdout, "megavoice", app.VoiceTables...)
		return nil
	case cmd == "check" && len(args) <= 1:
		if len(args) == 1 {
			o = app.LoadOpts{Path: args[0]}
		}
		path, _ := app.Path(o.Path)
		l, err := load(o)
		if err != nil {
			return err
		}
		if l.Path == "" {
			fmt.Printf("%s: absent; the defaults are valid\n", path)
			return nil
		}
		fmt.Printf("%s: ok\n", l.Path)
		return nil
	case cmd == "set" && len(args) == 2:
		path, _ := app.Path(o.Path)
		if err := set(path, args[0], app.Value(args[1])); err != nil {
			return err
		}
		fmt.Printf("%s: %s = %s\n", path, args[0], app.Value(args[1]))
		return nil
	case cmd == "init" && len(args) <= 1:
		b := app.Template()
		path, _ := app.Path(o.Path)
		if len(args) == 1 {
			path = args[0]
		}
		if path == "-" {
			_, err := os.Stdout.Write(b)
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s exists; kept as it is", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}
	fmt.Fprint(os.Stderr, configUsage)
	return exitCode(2)
}

// exitCode ends the process with a code and no message.
type exitCode int

func (e exitCode) Error() string { return "exit " + strconv.Itoa(int(e)) }
