package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Models are the files a FunASR root's models/ holds (the bytes
// runtime/funasr-root.nix pins and the repo's annexed models/ carry). A
// staging build downloads them into its root at first launch.
var Models = []string{"funasr-encoder-f16.gguf", "qwen3-0.6b-q8_0.gguf", "fsmn-vad.gguf"}

// cliName is the engine binary a root's bin/ holds.
const cliName = "llama-funasr-cli"

// BundleCLI is the engine CLI a staging MegaVoice.app carries beside its
// executable exe (Contents/MacOS/), or "" when exe is not in a bundle that
// has one.
func BundleCLI(exe string) string {
	if !strings.Contains(exe, ".app/Contents/MacOS/") {
		return ""
	}
	cli := filepath.Join(filepath.Dir(exe), cliName)
	if fi, err := os.Stat(cli); err != nil || fi.IsDir() {
		return ""
	}
	return cli
}

// LinkCLI makes root/bin/llama-funasr-cli a symlink to cli, the bundle's:
// the app may have moved or been swapped by an update, so a link that points
// elsewhere is replaced. A regular file there — a root someone built — is
// left alone.
func LinkCLI(root, cli string) error {
	link := filepath.Join(root, "bin", cliName)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if to, _ := os.Readlink(link); to == cli {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(cli, link); err != nil {
		return fmt.Errorf("linking the engine: %w", err)
	}
	return nil
}

// MissingModels lists the Models absent from root/models, in order.
func MissingModels(root string) []string {
	var out []string
	for _, m := range Models {
		if fi, err := os.Stat(filepath.Join(root, "models", m)); err != nil || fi.Size() == 0 {
			out = append(out, m)
		}
	}
	return out
}
