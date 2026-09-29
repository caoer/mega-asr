package audio

// Quality is one 20 ms block's signal quality, for the overlay: its level,
// the noise floor it is judged against, their difference, and whether any
// sample reached full scale.
type Quality struct {
	Level   float64 // block RMS, dBFS
	Floor   float64 // noise floor (10th percentile of the recent window), dBFS
	SNR     float64 // Level - Floor, dB
	Clipped bool    // a sample at |v| >= 32700
}
