package main

import (
	"os"
	"testing"

	"github.com/caoer/mega-asr/internal/app"
)

// TestMain pins the app's name: a build that passes its own appName runs
// these tests too (package.nix's check phase), and the menus they compare
// name the app.
func TestMain(m *testing.M) {
	app.AppName = "Mega Voice Dev"
	os.Exit(m.Run())
}
