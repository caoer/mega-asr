//go:build !darwin

package update

import (
	"context"
	"errors"
)

func osVersion() string { return "" }

func extractDefault(context.Context, string, string) (string, error) {
	return "", errors.New("update: no default unpacker off macOS; set Config.Extract")
}
