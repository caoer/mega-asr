//go:build !darwin

package main

import "github.com/caoer/mega-asr/internal/refine"

// display: there is no screen to capture off macOS; the screen source
// stays off.
func display() refine.Display { return nil }
