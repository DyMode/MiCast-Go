package playback

import (
	"encoding/binary"
	"math"

	"micastgo/internal/store"
)

type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
}

type equalizer struct {
	enabled bool
	preamp  float64
	bands   [3]biquad
}

func newEqualizer(config store.Equalizer) equalizer {
	e := equalizer{enabled: config.Enabled && (config.Bass != 0 || config.Mid != 0 || config.Treble != 0), preamp: 1}
	if !e.enabled {
		return e
	}
	boost := max(0, config.Bass) + max(0, config.Mid) + max(0, config.Treble)
	e.preamp = math.Pow(10, -(boost+1)/20)
	for i, band := range []struct{ hz, gain float64 }{{120, config.Bass}, {1000, config.Mid}, {8000, config.Treble}} {
		a := math.Pow(10, band.gain/40)
		w := 2 * math.Pi * band.hz / 44100
		alpha := math.Sin(w) / (2 * .7071067811865476)
		a0 := 1 + alpha/a
		e.bands[i] = biquad{b0: (1 + alpha*a) / a0, b1: -2 * math.Cos(w) / a0, b2: (1 - alpha*a) / a0, a1: -2 * math.Cos(w) / a0, a2: (1 - alpha/a) / a0}
	}
	return e
}

func (e *equalizer) process(data []byte) uint64 {
	if !e.enabled {
		return 0
	}
	var clipped uint64
	for i := 0; i+3 < len(data); i += 4 {
		for channel := 0; channel < 2; channel++ {
			offset := i + channel*2
			x := float64(int16(binary.LittleEndian.Uint16(data[offset:]))) * e.preamp
			for j := range e.bands {
				b := &e.bands[j]
				y := b.b0*x + b.z1[channel]
				b.z1[channel] = b.b1*x - b.a1*y + b.z2[channel]
				b.z2[channel] = b.b2*x - b.a2*y
				x = y
			}
			if x > 32767 || x < -32768 {
				clipped++
			}
			x = math.Round(max(-32768, min(32767, x)))
			binary.LittleEndian.PutUint16(data[offset:], uint16(int16(x)))
		}
	}
	return clipped
}
