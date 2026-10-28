package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bundle runs scripts/bundle.sh on a scratch binary with CHANNEL set to
// channel and returns the bundle's Info.plist.
func bundle(t *testing.T, plist, channel string) (string, error) {
	return bundleNamed(t, plist, channel, "")
}

// bundleNamed is bundle with NAME set to name.
func bundleNamed(t *testing.T, plist, channel, name string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "megavoice")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "MegaVoice.app")
	cmd := exec.Command("sh", "../../scripts/bundle.sh", bin, app, plist)
	cmd.Env = append(os.Environ(), "CHANNEL="+channel, "VERSION=", "NAME="+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return string(out), err
	}
	b, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

var bundleID = regexp.MustCompile(`<key>CFBundleIdentifier</key>\s*<string>([^<]*)</string>`)

// A staging build is its own app to macOS: TCC keys Accessibility and Input
// Monitoring on the bundle id, so a shared id lets one build's grant prompt
// replace the other's. A dev build (no channel) keeps the id it has always had.
func TestBundleIDFollowsChannel(t *testing.T) {
	for _, c := range []struct{ plist, channel, want string }{
		{"../../packaging/Info.plist", "", "app.0xdao.megavoice"},
		{"../../packaging/Info.plist", "staging", "app.0xdao.megavoice.staging"},
		{"../../packaging/megameet.plist", "", "app.0xdao.megameet"},
		{"../../packaging/megameet.plist", "staging", "app.0xdao.megameet.staging"},
	} {
		got, err := bundle(t, c.plist, c.channel)
		if err != nil {
			t.Fatalf("%s channel %q: %v: %s", c.plist, c.channel, err, got)
		}
		m := bundleID.FindAllStringSubmatch(got, -1)
		if len(m) != 1 || m[0][1] != c.want {
			t.Errorf("%s channel %q: CFBundleIdentifier %v, want %s", c.plist, c.channel, m, c.want)
		}
		src, _ := os.ReadFile(c.plist)
		if rest := bundleID.ReplaceAllString(got, ""); rest != bundleID.ReplaceAllString(string(src), "") {
			t.Errorf("%s channel %q: the bundle changed more of the plist than its id", c.plist, c.channel)
		}
	}
}

func TestBundleRefusesABadChannel(t *testing.T) {
	out, err := bundle(t, "../../packaging/Info.plist", "Staging/x")
	if err == nil || !strings.Contains(out, "is not [a-z0-9-]") {
		t.Fatalf("CHANNEL Staging/x: err %v, output %q; want a refusal", err, out)
	}
}

var (
	bundleName    = regexp.MustCompile(`<key>CFBundleName</key>\s*<string>([^<]*)</string>`)
	bundleDisplay = regexp.MustCompile(`<key>CFBundleDisplayName</key>\s*<string>([^<]*)</string>`)
)

// NAME is the name a person sees: CFBundleName and, in each language's
// InfoPlist.strings, the display name. The plist's own CFBundleDisplayName
// stays MegaVoice, the bundle's file name, or Finder ignores the localized
// one. Nothing else in the plist changes.
func TestBundleName(t *testing.T) {
	for _, c := range []struct{ channel, name, want string }{
		{"staging", "", "Mega Voice"},
		{"staging", "Mega Voice", "Mega Voice"},
		{"", "Mega Voice Dev", "Mega Voice Dev"},
	} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "megavoice")
		os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
		app := filepath.Join(dir, "MegaVoice.app")
		cmd := exec.Command("sh", "../../scripts/bundle.sh", bin, app)
		cmd.Env = append(os.Environ(), "CHANNEL="+c.channel, "VERSION=", "NAME="+c.name)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("NAME %q: %v: %s", c.name, err, out)
		}
		b, _ := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
		got := string(b)
		if m := bundleName.FindAllStringSubmatch(got, -1); len(m) != 1 || m[0][1] != c.want {
			t.Errorf("NAME %q: CFBundleName %v, want %q", c.name, m, c.want)
		}
		if m := bundleDisplay.FindAllStringSubmatch(got, -1); len(m) != 1 || m[0][1] != "MegaVoice" {
			t.Errorf("NAME %q: CFBundleDisplayName %v, want MegaVoice (the file name)", c.name, m)
		}
		src, _ := os.ReadFile("../../packaging/Info.plist")
		strip := func(s string) string { return bundleID.ReplaceAllString(bundleName.ReplaceAllString(s, ""), "") }
		if strip(got) != strip(string(src)) {
			t.Errorf("NAME %q changed more of the plist than its id and name", c.name)
		}
		for _, lang := range []string{"en", "zh-Hans"} {
			s, err := os.ReadFile(filepath.Join(app, "Contents", "Resources", lang+".lproj", "InfoPlist.strings"))
			if err != nil {
				t.Fatalf("NAME %q: %v", c.name, err)
			}
			for _, k := range []string{"CFBundleName", "CFBundleDisplayName"} {
				if !strings.Contains(string(s), `"`+k+`" = "`+c.want+`";`) {
					t.Errorf("NAME %q: %s.lproj lacks %s = %q:\n%s", c.name, lang, k, c.want, s)
				}
			}
			if strings.Contains(string(s), "@NAME@") || !strings.Contains(string(s), "NSMicrophoneUsageDescription") {
				t.Errorf("NAME %q: %s.lproj:\n%s", c.name, lang, s)
			}
		}
	}
	if out, err := bundleNamed(t, "../../packaging/Info.plist", "", "Mega</string>"); err == nil || !strings.Contains(out, "is not [A-Za-z0-9 -]") {
		t.Fatalf("a NAME with markup: err %v, output %q; want a refusal", err, out)
	}
}
