package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDefaultRootIsDataHome(t *testing.T) {
	d := t.TempDir()
	t.Setenv("XDG_DATA_HOME", d)
	saved := DefaultASRRoot
	t.Cleanup(func() { DefaultASRRoot = saved })

	DefaultASRRoot = ""
	if got, want := Default().ASR.FunASR.Root, filepath.Join(d, "mega-asr", "funasr-root"); got != want {
		t.Fatalf("root with no baked default = %q, want %q (where a staging build's first launch fills it)", got, want)
	}
	DefaultASRRoot = "/nix/store/x-megavoice-asr-root"
	if got := Default().ASR.FunASR.Root; got != DefaultASRRoot {
		t.Fatalf("root with a baked default = %q, want %q", got, DefaultASRRoot)
	}
}

func TestBundleCLI(t *testing.T) {
	d := t.TempDir()
	macos := filepath.Join(d, "MegaVoice.app", "Contents", "MacOS")
	os.MkdirAll(macos, 0o755)
	exe := filepath.Join(macos, "megavoice")
	if got := BundleCLI(exe); got != "" {
		t.Fatalf("bundle without the CLI: %q, want none", got)
	}
	os.WriteFile(filepath.Join(macos, cliName), []byte("x"), 0o755)
	if got := BundleCLI(exe); got != filepath.Join(macos, cliName) {
		t.Fatalf("BundleCLI = %q", got)
	}
	if got := BundleCLI(filepath.Join(d, "bin", "megavoice")); got != "" {
		t.Fatalf("not in a bundle: %q, want none", got)
	}
}

func TestLinkCLIFollowsTheBundle(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "bin", cliName)
	for _, cli := range []string{"/Applications/MegaVoice.app/Contents/MacOS/" + cliName, "/Volumes/Downloads/MegaVoice.app/Contents/MacOS/" + cliName} {
		if err := LinkCLI(root, cli); err != nil {
			t.Fatal(err)
		}
		if to, err := os.Readlink(link); err != nil || to != cli {
			t.Fatalf("link = %q, %v; want %q", to, err, cli)
		}
	}
	// a root someone built keeps its own binary
	os.Remove(link)
	os.WriteFile(link, []byte("built"), 0o755)
	if err := LinkCLI(root, "/Applications/MegaVoice.app/Contents/MacOS/"+cliName); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(link); string(b) != "built" {
		t.Fatal("a regular file was replaced")
	}
}

func TestMissingModels(t *testing.T) {
	root := t.TempDir()
	if got := MissingModels(root); !slices.Equal(got, Models) {
		t.Fatalf("empty root: %v", got)
	}
	os.MkdirAll(filepath.Join(root, "models"), 0o755)
	os.WriteFile(filepath.Join(root, "models", Models[1]), []byte("gguf"), 0o644)
	os.WriteFile(filepath.Join(root, "models", Models[2]), nil, 0o644) // an empty file is missing
	if got, want := MissingModels(root), []string{Models[0], Models[2]}; !slices.Equal(got, want) {
		t.Fatalf("MissingModels = %v, want %v", got, want)
	}
}
