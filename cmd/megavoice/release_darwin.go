//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/mac"
	"github.com/caoer/mega-asr/internal/update"
)

// A staging build (app.Channel set) is the MegaVoice.app friends install
// from the release page: opened from Finder or at login, never a launchd
// agent. It carries the engine CLI, downloads its models at first launch,
// offers to open at login, and updates itself from the page's feed. A dev
// build (Channel "") does none of this.

// setLanguage picks the language of what this process shows: a release
// build speaks Chinese when the Mac's first preferred language is Chinese
// and English otherwise; a dev build keeps its strings as written.
func setLanguage() {
	if app.Channel == "" {
		return
	}
	l10n.Set(l10n.FromTag(mac.PreferredLanguage()))
}

// bundleOf is the .app that holds exe, or "" when exe is not in one.
func bundleOf(exe string) string {
	i := strings.Index(exe, ".app/Contents/MacOS/")
	if i < 0 {
		return ""
	}
	return exe[:i+len(".app")]
}

// logToFile sends this process's stdout and stderr — the log, a panic — to
// ~/Library/Logs/megavoice.log, where launchd puts the agent's in a dev install.
func logToFile() {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Library", "Logs", "megavoice.log")
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	syscall.Dup2(int(f.Fd()), 1)
	syscall.Dup2(int(f.Fd()), 2)
}

// relaunching is exit for a staging build that launchd does not keep alive:
// a restart (exit 75) opens the app again once this process is gone. Under
// the launchd agent, its KeepAlive does that, so exit is returned as it is.
func relaunching(exit func(int)) func(int) {
	if app.Channel == "" || os.Getenv("XPC_SERVICE_NAME") == agentLabel {
		return exit
	}
	return func(code int) {
		exe, _ := os.Executable()
		if b := bundleOf(exe); code == 75 && b != "" {
			cmd := exec.Command("/bin/sh", "-c", `while kill -0 "$1" 2>/dev/null; do sleep 0.2; done; exec /usr/bin/open "$2"`,
				"sh", strconv.Itoa(os.Getpid()), b)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				log.Printf("serve: restart: reopening %s: %v", b, err)
			} else {
				log.Printf("serve: restart: %s opens again once pid %d has exited", b, os.Getpid())
			}
		}
		exit(code)
	}
}

// startReleases runs a staging build's first run and update loop.
func (s *server) startReleases() {
	if app.Channel == "" {
		return
	}
	exe, _ := os.Executable()
	log.Printf("serve: channel %s %s, feed %s", app.Channel, app.Version, app.FeedURL)
	r, err := newReleases(releaseConfig(), bundleOf(exe), releaseUI{
		ask:    askUpdate,
		flash:  s.ctrl.UI.Flash,
		status: s.setReleaseLine,
		restart: func() {
			s.cmds.handle(ctl.Request{Cmd: "restart", Args: json.RawMessage(`{"when_idle":true}`)})
		},
	})
	if err != nil {
		log.Printf("update: %v; this build does not update itself", err)
		return
	}
	s.releases = r
	go s.firstRun(exe)
	go r.run(context.Background(), update.Interval)
}

func (s *server) setReleaseLine(line string) { s.releaseLine.Store(&line) }

func (s *server) releaseLineNow() string {
	if p := s.releaseLine.Load(); p != nil {
		return *p
	}
	return ""
}

// askUpdate is the update prompt: 更新 / 跳过此版本 / 稍后.
func askUpdate(rel *update.Release) int {
	info := l10n.F("新版本 %s（现在是 %s）。", "Version %s is out (you have %s).", rel.Version, app.Version)
	if notes := releaseNotes(rel); notes != "" {
		info += "\n\n" + notes
	}
	info += l10n.F("\n\n更新后 %s 会在空闲时自动重启。", "\n\nAfter the update, %s restarts by itself when idle.", app.Name())
	ch := make(chan int, 1)
	mac.Later(func() {
		ch <- mac.Choose(l10n.F("%s 有新版本", "A new version of %s is available", app.Name()), info,
			l10n.T("更新", "Update"), l10n.T("跳过此版本", "Skip This Version"), l10n.T("稍后", "Later"))
	})
	return <-ch
}

// firstRun points the engine root at the bundle's CLI, offers the login
// item once, and downloads the models the root lacks, retrying until they
// are in; the menu shows the progress.
func (s *server) firstRun(exe string) {
	root := s.cfg.ASR.FunASR.Root
	if cli := app.BundleCLI(exe); cli != "" {
		if err := app.LinkCLI(root, cli); err != nil {
			log.Printf("firstrun: %v", err)
		}
	}
	s.offerLoginItem()
	missing := app.MissingModels(root)
	if len(missing) == 0 {
		return
	}
	log.Printf("firstrun: downloading %s into %s", strings.Join(missing, ", "), root)
	for wait := time.Minute; ; wait = min(2*wait, 30*time.Minute) {
		last := -1
		err := fetchModels(context.Background(), s.releases.u, root, missing, func(done, total int64) {
			if pct := int(done * 100 / max(total, 1)); pct != last {
				last = pct
				s.setReleaseLine(l10n.F("正在下载语音模型 %d%%", "Downloading the speech models %d%%", pct))
			}
		})
		if err == nil {
			break
		}
		log.Printf("firstrun: models: %v; again in %v", err, wait)
		if errors.Is(err, update.ErrRevoked) {
			s.setReleaseLine(userMessage(err))
			return
		}
		s.setReleaseLine(l10n.T("语音模型下载中断，稍后自动继续", "Model download interrupted; it resumes by itself"))
		time.Sleep(wait)
		missing = app.MissingModels(root)
	}
	log.Printf("firstrun: models in %s", root)
	s.setReleaseLine("")
	s.ctrl.UI.Flash(l10n.T("语音模型已就绪", "Speech models ready"))
	if fb, ok := s.ctrl.ASR.(*app.Fallback); ok {
		fb.Warm(stateDir())
	}
}

// offerLoginItem asks once whether MegaVoice opens at login.
func (s *server) offerLoginItem() {
	mark := filepath.Join(stateDir(), "login-item-offered")
	if _, err := os.Stat(mark); err == nil || mac.LoginItemOn() {
		return
	}
	ch := make(chan int, 1)
	mac.Later(func() {
		ch <- mac.Choose(l10n.F("登录时自动打开 %s？", "Open %s at login?", app.Name()),
			l10n.F("%[1]s 在菜单栏里待命。开机登录时自动打开，就能随时用。", "%[1]s waits in the menu bar. Open it at login and it is always ready.", app.Name()),
			l10n.T("自动打开", "Open at Login"), l10n.T("不用了", "Not Now"))
	})
	if <-ch == 0 {
		if err := mac.SetLoginItem(true); err != nil {
			log.Printf("firstrun: %v", err)
		} else {
			log.Printf("firstrun: opens at login")
		}
	}
	os.WriteFile(mark, nil, 0o600)
}

// updateReply answers the control socket's update: a check now, whose
// prompt, if any, the agent shows.
func (s *server) updateReply() ctl.Response {
	if s.releases == nil {
		return ctl.Fail(errors.New("this build does not update itself (channel " + app.ChannelName() + ")"))
	}
	go s.releases.check(context.Background())
	return ctl.Reply(map[string]string{"state": "checking", "channel": app.Channel, "version": app.Version})
}

// updateCmd is `megavoice update`: the running agent checks the feed now.
func updateCmd(args []string) error {
	if len(args) > 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitCode(2)
	}
	if app.Channel == "" {
		fmt.Println("channel dev: this build never checks for updates")
		return nil
	}
	return call(ctl.Request{Cmd: "update"}, false)
}
