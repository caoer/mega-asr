package audio

import "math"

// resampler converts a float32 stream at any input rate to Rate int16 with a
// Blackman-windowed sinc low-pass at 90 % of the output Nyquist. Two
// resamplers built for the same rate and fed the same frame counts return
// the same sample counts, which keeps a device's tracks frame-aligned.
type resampler struct {
	step  float64   // input samples per output sample
	half  int       // kernel half-width, input samples
	table []float64 // kernel at x = i/res input samples, 0 ≤ x ≤ half
	buf   []float32 // input not yet consumed, starting at buffer index 0
	pos   float64   // buffer position of the next output sample
}

const resampleRes = 256 // kernel table points per input sample

func newResampler(inRate float64) *resampler {
	step := inRate / Rate
	fc := 0.45 / math.Max(step, 1) // cutoff, cycles per input sample
	half := int(math.Ceil(16 / (2 * fc)))
	r := &resampler{step: step, half: half, table: make([]float64, half*resampleRes+2)}
	for i := range r.table {
		x := float64(i) / resampleRes
		h := 2 * fc
		if x > 0 {
			h = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		if w := x / float64(half); w < 1 { // Blackman, centred
			h *= 0.42 + 0.5*math.Cos(math.Pi*w) + 0.08*math.Cos(2*math.Pi*w)
		} else {
			h = 0
		}
		r.table[i] = h
	}
	r.buf = make([]float32, half) // leading silence: the first output is centred on input 0
	r.pos = float64(half)
	return r
}

func (r *resampler) kernel(x float64) float64 {
	x = math.Abs(x) * resampleRes
	i := int(x)
	if i+1 >= len(r.table) {
		return 0
	}
	f := x - float64(i)
	return r.table[i]*(1-f) + r.table[i+1]*f
}

// push feeds input samples and returns the output samples they complete.
func (r *resampler) push(in []float32) []int16 {
	r.buf = append(r.buf, in...)
	var out []int16
	for int(r.pos)+r.half < len(r.buf) {
		c := int(r.pos)
		var acc float64
		for j := c - r.half + 1; j <= c+r.half; j++ {
			acc += float64(r.buf[j]) * r.kernel(r.pos-float64(j))
		}
		out = append(out, toInt16(acc))
		r.pos += r.step
	}
	if drop := int(r.pos) - r.half; drop > 0 {
		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.pos -= float64(drop)
	}
	return out
}

func toInt16(v float64) int16 {
	v = math.Round(v * 32767)
	return int16(max(-32768, min(32767, v)))
}
