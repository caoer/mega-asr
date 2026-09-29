// Package asr turns a 16 kHz mono WAV into text.
package asr

import "context"

// Transcriber transcribes one clip; empty text means no speech. whole decodes
// the clip as one window; otherwise FSMN-VAD cuts it into segments first.
type Transcriber interface {
	Transcribe(ctx context.Context, wav string, whole bool) (string, error)
}
