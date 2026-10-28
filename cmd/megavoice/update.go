package main

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/update"
)

// The update prompt's buttons, in order: Return is 更新, Esc 稍后.
const (
	choiceUpdate = iota // 更新
	choiceSkip          // 跳过此版本
	choiceLater         // 稍后
)

// releaseUI is what the update loop needs of the app.
type releaseUI struct {
	ask     func(rel *update.Release) int // the prompt; any answer but update or skip is 稍后
	flash   func(msg string)
	status  func(line string) // the menu's update line; "" clears it
	restart func()            // relaunch once idle: restart --when-idle
}

// releases is a staging build's update loop: a check at launch and every
// update.Interval, the prompt for a release it offers, the install and the
// relaunch. A dev build's (Channel "") makes no request at all.
type releases struct {
	u   *update.Updater
	app string // the bundle the running executable is in, which an update replaces
	ui  releaseUI

	mu      sync.Mutex
	busy    bool // a prompt or an install is under way
	revoked bool // the token stopped unlocking: said once
}

// releaseConfig is this build's updater: the -X values and its state dir.
func releaseConfig() update.Config {
	return update.Config{
		Channel:  app.Channel,
		FeedURL:  app.FeedURL,
		Token:    app.PageToken,
		Current:  app.Version,
		StateDir: filepath.Join(stateDir(), "update"),
	}
}

func newReleases(cfg update.Config, bundle string, ui releaseUI) (*releases, error) {
	u, err := update.New(cfg)
	if err != nil {
		return nil, err
	}
	return &releases{u: u, app: bundle, ui: ui}, nil
}

// run checks now and then every `every` until ctx ends; nothing on a dev build.
func (r *releases) run(ctx context.Context, every time.Duration) {
	if !r.u.Enabled() {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// check is one cycle; a cycle that finds a prompt or an install under way
// asks nothing.
func (r *releases) check(ctx context.Context) {
	rel, err := r.u.Check(ctx)
	if err != nil {
		r.failed("check", err)
		return
	}
	if rel == nil {
		return
	}
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return
	}
	r.busy = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.busy = false; r.mu.Unlock() }()

	switch r.ui.ask(rel) {
	case choiceUpdate:
		log.Printf("update: installing %s over %s", rel.Version, r.app)
		r.ui.status(l10n.F("正在下载 %s…", "Downloading %s…", rel.Version))
		if err := r.u.Apply(ctx, rel, r.app); err != nil {
			r.ui.status("")
			r.failed("install "+rel.Version, err)
			return
		}
		log.Printf("update: %s installed; restarting once nothing is in flight", rel.Version)
		r.ui.status(l10n.F("%s 已安装，空闲时自动重启", "%s installed; restarts by itself when idle", rel.Version))
		r.ui.restart()
	case choiceSkip:
		log.Printf("update: %s skipped", rel.Version)
		if err := r.u.Skip(rel.Version); err != nil {
			log.Printf("update: skip %s: %v", rel.Version, err)
		}
	default:
		log.Printf("update: %s later", rel.Version)
	}
}

// failed logs err and shows its line, if it has one; a revoked token is
// shown once, as it stops the updater until the next launch.
func (r *releases) failed(what string, err error) {
	log.Printf("update: %s: %v", what, err)
	msg := userMessage(err)
	if errors.Is(err, update.ErrRevoked) {
		r.mu.Lock()
		said := r.revoked
		r.revoked = true
		r.mu.Unlock()
		if said {
			return
		}
		r.ui.status(msg)
	}
	if msg != "" {
		r.ui.flash(msg)
	}
}

// userMessage is the line shown for an updater error, or "" when the user
// sees nothing (update.UserMessage's cases, in the user's language).
func userMessage(err error) string {
	switch {
	case errors.Is(err, update.ErrRevoked):
		return l10n.T("更新不可用，请从页面重新安装", "Updates are off for this copy; install again from the page")
	case errors.Is(err, update.ErrChecksum):
		return l10n.T("更新校验失败，下次再试", "The update failed its check; trying again later")
	case errors.Is(err, update.ErrNotWritable):
		return l10n.T("请从页面重新安装", "Install again from the page")
	}
	return ""
}

// releaseNotes is rel's notes in the user's language: the English notes in
// English when the feed carries them, else the Chinese.
func releaseNotes(rel *update.Release) string {
	if l10n.Current() == l10n.EN && rel.NotesEN != "" {
		return rel.NotesEN
	}
	return rel.Notes
}
