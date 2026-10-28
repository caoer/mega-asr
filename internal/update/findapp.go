package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// findApp is the one *.app directly inside dir.
func findApp(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var apps []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			apps = append(apps, filepath.Join(dir, e.Name()))
		}
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("update: want one .app in the download, found %d", len(apps))
	}
	return apps[0], nil
}
