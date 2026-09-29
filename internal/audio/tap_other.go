//go:build !darwin

package audio

import "context"

type tapState struct{}

func (t *Tap) start(context.Context, int) (<-chan []int16, error) { return nil, ErrTapUnsupported }
func (t *Tap) stop()                                              {}
func (t *Tap) err() error                                         { return ErrTapUnsupported }

// Info is the zero TapInfo off macOS.
func (t *Tap) Info() TapInfo { return TapInfo{} }

// Apps lists Core Audio client processes; off macOS there are none.
func Apps() ([]App, error) { return nil, ErrTapUnsupported }

// Inputs lists input devices; off macOS there are none.
func Inputs() ([]Input, error) { return nil, ErrTapUnsupported }
