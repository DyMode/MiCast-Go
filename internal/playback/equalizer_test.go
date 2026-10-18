package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"

	"micastgo/internal/app"
	"micastgo/internal/store"
)

func TestEqualizerBypassIsBitExactAndChannelsIndependent(t *testing.T) {
	data := []byte{0, 128, 255, 127, 0, 0, 0, 0, 17, 34, 51, 68}
	for _, config := range []store.Equalizer{{}, {Enabled: true}} {
		eq := newEqualizer(config)
		got := append([]byte(nil), data...)
		eq.process(got)
		if !bytes.Equal(got, data) {
			t.Fatal("flat EQ changed source")
		}
	}
	eq := newEqualizer(store.Equalizer{Enabled: true, Bass: 12, Mid: 12, Treble: 12})
	data = make([]byte, 44100*4)
	for i := 0; i < len(data); i += 4 {
		binary.LittleEndian.PutUint16(data[i:], 32767)
	}
	if clips := eq.process(data); clips != 0 {
		t.Fatal("headroom failed", clips)
	}
	for i := 2; i < len(data); i += 4 {
		if binary.LittleEndian.Uint16(data[i:]) != 0 {
			t.Fatal("left channel leaked into right")
		}
	}
}

func TestEqualizerBassResponseAndSessionSnapshot(t *testing.T) {
	rms := func(hz float64) float64 {
		eq := newEqualizer(store.Equalizer{Enabled: true, Bass: 12})
		data := make([]byte, 44100*4)
		for i := 0; i < 44100; i++ {
			v := int16(math.Round(10000 * math.Sin(2*math.Pi*hz*float64(i)/44100)))
			binary.LittleEndian.PutUint16(data[i*4:], uint16(v))
		}
		eq.process(data)
		sum := 0.0
		for i := 4410; i < 44100; i++ {
			v := float64(int16(binary.LittleEndian.Uint16(data[i*4:])))
			sum += v * v
		}
		return math.Sqrt(sum / (44100 - 4410))
	}
	if ratio := rms(120) / rms(8000); ratio < 3 || ratio > 5 {
		t.Fatal("incorrect bass response", ratio)
	}
	e := lifecycleEngine(t)
	first := e.newSession("dlna", Track{}, 0)
	config := store.Equalizer{Enabled: true, Bass: 3}
	if err := e.app.UpdateSettings(context.Background(), "whole_house", "", "MiCast Go", "wav", app.SettingsOptions{Equalizer: &config}); err != nil {
		t.Fatal(err)
	}
	resumed := e.newSession("dlna", Track{}, 1, first.identity)
	if resumed.equalizer.enabled {
		t.Fatal("settings changed current cast")
	}
	if next := e.newSession("airplay", Track{}, 0); !next.equalizer.enabled {
		t.Fatal("new cast did not enable EQ")
	}
}
