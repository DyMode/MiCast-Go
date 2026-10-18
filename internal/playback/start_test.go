package playback

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStartSequenceRejectsOlderCrossProtocolCast(t *testing.T) {
	e := lifecycleEngine(t)
	raw := "http://192.168.1.20/sequence.wav"
	e.cacheURL = raw
	e.cachePath = filepath.Join(t.TempDir(), "sequence.wav")
	if err := os.WriteFile(e.cachePath, wavHeader(), 0600); err != nil {
		t.Fatal(err)
	}
	older, newer := e.Sequence(), e.Sequence()
	if _, err := e.StartFile(StartRequest{Seq: newer, Protocol: "dlna", URI: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartFile(StartRequest{Seq: older, Protocol: "web", URI: raw}); !errors.Is(err, ErrSuperseded) {
		t.Fatal("older cast took over a newer one", err)
	}
	if _, err := e.StartFile(StartRequest{Seq: e.Sequence(), Protocol: "web", URI: raw}); err != nil {
		t.Fatal("newer cast rejected", err)
	}
}

func TestFFmpegDurationParser(t *testing.T) {
	for _, test := range []struct {
		output string
		want   float64
	}{
		{"  Duration: 00:03:12.34, start: 0.000000, bitrate: 320 kb/s\n", 192.34},
		{"Duration: 01:00:00.00, start: 0.000000", 3600},
		{"Duration: N/A, bitrate: N/A", 0},
		{"no duration here", 0},
	} {
		if got := parseFFmpegDuration(test.output); got != test.want {
			t.Fatalf("%q: %v, want %v", test.output, got, test.want)
		}
	}
}
