//go:build darwin

// megavoice — tap a key, speak, and the transcript lands where you were
// typing. `serve` runs as the MegaVoice.app launchd agent and owns the key
// tap, the overlay and delivery; the other subcommands talk to it over its
// control socket, or (record, transcribe) run a pipeline stage by hand.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/ctl"
)

const usage = `usage: megavoice [--config FILE] [--set KEY=VALUE]... <command> [args]

  --config FILE             settings file (default: $MEGAVOICE_CONFIG, else
                            ~/.config/megavoice/config.toml)
  --set section.key=value   override one setting for this run; repeatable

  serve [--no-tap] [--file WAV] [--pane ID]
                            run the agent (launchd starts it); the flags are for
                            a scratch instance run for checks (serve --help)
  status                    grants, tap and state of the running agent
  restart [--when-idle]     end the agent between takes; launchd starts it
                            again, reading the config file anew. --when-idle
                            returns at once and the agent exits once nothing
                            has been in flight and no key was pressed for 3 s;
                            without it a take in flight refuses the restart
  quit                      end the agent between takes; it stays down

  A command the agent refuses exits 1, restart or quit refused 75; with no
  agent running, status, toggle, cancel, spike, paste, restart and quit exit 69.
  start                     start the agent again (opening MegaVoice.app does too)
  toggle                    start or stop a recording, as a tap would
  cancel                    end a recording without delivering it; the take
                            and its text are kept as cancelled
  paste [--activate PID] TEXT
                            deliver TEXT by paste, after focusing PID's window
  takes [-n N] [--undelivered] [--json]
                            the newest takes: name, length, state, target, text
  events ID                 the record of the take named ID, one event a line
  resend [ID] [--to front|pane:<id>|app:<pid>:<bundle id>|clipboard] [--send]
         [--text delivered|raw|<engine>|"retranscription N"]
                            deliver a kept take's text again (default: the newest
                            undelivered take, pasted where the focus is, no Enter)
  retranscribe ID --engine funasr|doubao
                            decode a take's audio again; the answer goes into its
                            record, no stored text changes
  spike                     taps the agent has seen
  record [--seconds N] OUT.wav
                            capture from capture.source into a WAV
  mics                      this Mac's inputs: UID, channels, the default, the one in use
  mic add HOST[:PORT] [--name N]
                            pair a mic server (megavoice mic serve on HOST, port
                            7866 by default) as N (this Mac's hostname): over ssh
                            when ssh HOST works without a prompt, else by the PIN
                            its mic pair shows; dictation then records from it
  mic scan                  the mic servers Add Remote Mic… offers: advertised on
                            the network, or an ssh config Host that answers
  mic list | forget NAME    the paired remote mics; forget one
  transcribe [--plain] [--engine funasr|doubao] WAV
                            print a WAV's transcript with hotwords and corrections,
                            by asr.engine or the engine named
  script [--plain] [DIR]    the script check: the scripted takes DIR/script.tsv names (DIR
                            default store.data) transcribed and scored, errors per clip
  replay [-data DIR] [-out DIR] [-chunks DIR] WAV...
                            stream WAVs through the controller at real time, as
                            takes; print each text and its stop-to-text time;
                            -chunks writes each take's chunk WAVs, sample
                            offsets, edge reasons and texts to DIR/<name>/
  panel [-data DIR] [-labels FILE] [-addr HOST:PORT]
                            serve the Takes page over DIR's takes (default
                            store.data) and their labels without the agent,
                            which serves its own at compare.addr; prints its
                            address with the key
  takes url [--rotate]      the Takes page's address, with the key that opens
                            it; --rotate writes a new key, and every page
                            opened before is refused
  config show | check [FILE] | init [FILE]
                            the effective settings and their sources; validate
                            a file; write the commented default config
  version                   this build's id, the chunker version
                            (audio.ChunkerVersion) and the build's commit
`

func main() {
	var o app.LoadOpts
	gfs := flag.NewFlagSet("megavoice", flag.ContinueOnError)
	gfs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	gfs.StringVar(&o.Path, "config", "", "settings file")
	gfs.Func("set", "section.key=value", func(s string) error { o.Sets = append(o.Sets, s); return nil })
	if len(os.Args) == 1 && inBundle() {
		// MegaVoice.app opened from Finder or Spotlight
		if err := start(); err != nil {
			fmt.Fprintln(os.Stderr, "megavoice:", err)
			os.Exit(1)
		}
		return
	}
	if err := gfs.Parse(os.Args[1:]); err != nil || gfs.NArg() < 1 {
		if err == nil {
			fmt.Fprint(os.Stderr, usage)
		}
		os.Exit(2)
	}
	cmd, args := gfs.Arg(0), gfs.Args()[1:]
	var err error
	switch cmd {
	case "serve":
		err = serve(o, args)
	case "start":
		err = start()
	case "status", "toggle", "cancel", "spike", "quit":
		err = call(ctl.Request{Cmd: cmd}, cmd == "status")
	case "restart":
		err = restart(args)
	case "paste":
		err = paste(args)
	case "takes":
		if len(args) > 0 && args[0] == "url" {
			err = takesCmd(args, os.Stdout)
		} else {
			err = takesList(args)
		}
	case "events":
		err = eventsCmd(args)
	case "resend":
		err = resendCmd(args)
	case "retranscribe":
		err = retranscribeCmd(args)
	case "record":
		err = record(o, args)
	case "mics":
		err = mics(o)
	case "mic":
		err = micRemote(o, args, os.Stdin, os.Stdout)
	case "transcribe":
		err = transcribe(o, args)
	case "script":
		err = scriptCheck(o, args)
	case "replay":
		err = replay(o, args)
	case "config":
		err = configCmd(o, args)
	case "panel":
		err = panel(o, args)
	case "version":
		fmt.Printf("build\t%s\n%s", app.BuildID(), audio.Version())
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "megavoice:", err)
		os.Exit(1)
	}
}

// exitRefused and exitNotRunning are the exits of restart or quit refused
// by the agent (EX_TEMPFAIL: try again) and of a command with no agent
// running (EX_UNAVAILABLE), which install-app.sh tells apart.
const (
	exitRefused    = exitCode(75)
	exitNotRunning = exitCode(69)
)

func call(req ctl.Request, isStatus bool) error {
	resp, err := ctl.Call(sockPath(), req, 10*time.Second)
	if errors.Is(err, ctl.ErrNotRunning) {
		fmt.Fprintln(os.Stderr, "megavoice:", err)
		return exitNotRunning
	}
	if err != nil {
		return err
	}
	if !resp.OK && (req.Cmd == "restart" || req.Cmd == "quit") {
		fmt.Fprintln(os.Stderr, "megavoice:", resp.Error)
		return exitRefused
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if len(resp.Data) > 0 {
		var v any
		_ = json.Unmarshal(resp.Data, &v)
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
	}
	if isStatus {
		var s status
		if json.Unmarshal(resp.Data, &s) == nil && (!s.Grants.Accessibility || !s.TapInstalled) {
			return exitCode(3)
		}
	}
	return nil
}

// restart asks the agent to restart; --when-idle sends {"when_idle": true}.
func restart(args []string) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	idle := fs.Bool("when-idle", false, "exit once nothing has been in flight and no key was pressed for 3 s")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if err == nil {
			fmt.Fprint(os.Stderr, usage)
		}
		return exitCode(2)
	}
	req := ctl.Request{Cmd: "restart"}
	if *idle {
		req.Args = json.RawMessage(`{"when_idle":true}`)
	}
	return call(req, false)
}

func paste(args []string) error {
	fs := flag.NewFlagSet("paste", flag.ContinueOnError)
	pid := fs.Int("activate", 0, "focus this pid's window before pasting")
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprint(os.Stderr, usage)
		return exitCode(2)
	}
	return call(ctl.Request{Cmd: "paste", Text: fs.Arg(0), PID: int32(*pid)}, false)
}

// agentLabel is the launchd label of the agent that runs serve: the app's
// bundle id (install-app.sh).
const agentLabel = "app.0xdao.megavoice"

func inBundle() bool {
	exe, _ := os.Executable()
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

// start asks launchd to run the agent; a running agent is left as it is.
func start() error {
	out, err := exec.Command("launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart gui/%d/%s: %v: %s", os.Getuid(), agentLabel, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mics lists this Mac's inputs, the one capture.mic records marked.
func mics(o app.LoadOpts) error {
	in, err := audio.Inputs()
	if err != nil {
		return err
	}
	c := app.Default().Capture
	if l, err := load(o); err == nil {
		c = l.Capture
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USE\tDEFAULT\tCHANNELS\tUID\tNAME")
	yn := map[bool]string{true: "Y", false: "-"}
	for _, d := range in {
		use := c.Source == "local" && (d.UID == c.Mic || c.Mic == "default" && d.Default)
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", yn[use], yn[d.Default], d.Channels, d.UID, d.Name)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	switch c.Source {
	case "ssh":
		fmt.Printf("in use: capture.source = %q (%s:%s)\n", c.Source, c.Host, c.Device)
	case "remote":
		fmt.Printf("in use: capture.source = %q (%s; megavoice mic list)\n", c.Source, c.Remote)
	}
	fmt.Println("pick one: megavoice config set capture.mic <UID> (or default); capture.source local; capture.mic_channel <n> (0 mixes)")
	return nil
}
