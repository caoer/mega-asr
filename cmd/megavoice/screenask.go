package main

import (
	"errors"
	"log"

	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/refine"
)

// askScreen is what the menu does once it has written arg to key: switching
// the screen source on (its text or its picture) asks for the Screen
// Recording grant while it is off. A take never asks (refine.ScreenSource),
// so a source switched on by `config set` stays without the screen until
// the menu's switch or `megavoice screen` asks.
func askScreen(d refine.Display, key, arg string) error {
	if key != "refine.context.screen" || arg != `"`+refine.ScreenOCR+`"` && arg != `"`+refine.ScreenImage+`"` {
		return nil
	}
	return askGrant(d)
}

// askGrant asks for the Screen Recording grant when it is off: the
// system's prompt the first time. It returns what to tell the user while
// the grant stays off.
func askGrant(d refine.Display) error {
	if d.Access() {
		return nil
	}
	granted := d.Request()
	log.Printf("screen: Screen Recording asked for: granted %v", granted)
	if granted {
		return nil
	}
	return errors.New(l10n.T("请在系统设置里允许 Mega Voice 录制屏幕，然后重启它；在那之前转译不带屏幕",
		"Allow Screen Recording for Mega Voice in System Settings, then restart it"))
}
