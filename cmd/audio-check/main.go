// audio-check measures the development audio adapter without Xiaomi devices.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"micastgo/internal/audio"
)

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
func main() {
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "开发用 FFmpeg 路径")
	flag.Parse()
	pcm := make([]byte, 44100*2*2)
	for i := 0; i < 44100; i++ {
		sample := uint16(int16(math.Sin(float64(i)*440*2*math.Pi/44100) * 3000))
		binary.LittleEndian.PutUint16(pcm[i*4:], sample)
		binary.LittleEndian.PutUint16(pcm[i*4+2:], sample)
	}
	type result struct {
		Format     audio.Format `json:"format"`
		Bytes      int64        `json:"bytes"`
		DurationMS int64        `json:"duration_ms"`
		Error      string       `json:"error,omitempty"`
	}
	results := []result{}
	failed := false
	for _, format := range []audio.Format{audio.MP3, audio.FLAC, audio.WAV, audio.PCM} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		writer := &countWriter{}
		err := audio.Transcode(ctx, *ffmpeg, bytes.NewReader(pcm), writer, audio.Options{Format: format, SampleRate: 44100, Channels: 2, BitrateKbps: 192, RawPCM: true, InputRate: 44100, InputChannels: 2})
		cancel()
		r := result{Format: format, Bytes: writer.n, DurationMS: time.Since(start).Milliseconds()}
		if err != nil {
			r.Error = err.Error()
			failed = true
		}
		results = append(results, r)
	}
	encoded, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(encoded))
	if failed {
		os.Exit(1)
	}
}
