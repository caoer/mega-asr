//go:build !darwin

package audio

import "context"

type micState struct{}

// Start fails off macOS.
func (m *Mic) Start(context.Context) (<-chan []int16, error) { return nil, ErrMicUnsupported }
func (m *Mic) Stop()                                         {}
func (m *Mic) Err() error                                    { return nil }
func (m *Mic) Note() string                                  { return "" }
func (m *Mic) Device() string                                { return "" }
func (m *Mic) Input() TakeInput                              { return TakeInput{Source: "local", UID: m.UID, Channel: m.Channel} }

// PrimeMic fails off macOS.
func PrimeMic() error { return ErrMicUnsupported }
