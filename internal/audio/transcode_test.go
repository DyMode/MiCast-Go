package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
	"time"
)

func TestRejectInvalidFormatsBeforeStartingProcess(t *testing.T) {
	for _, o := range []Options{{Format: "video", SampleRate: 44100, Channels: 2}, {Format: MP3, SampleRate: 123, Channels: 2}, {Format: MP3, SampleRate: 44100, Channels: 2, BitrateKbps: 999}} {
		if _, err := o.Arguments(); err == nil {
			t.Fatal("invalid options accepted", o)
		}
	}
}
func TestFFmpegRoundTripFormats(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("integration test requires development FFmpeg")
	}
	pcm := make([]byte, 44100*4/2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(i%3000))
	}
	for _, format := range []Format{MP3, FLAC, WAV, PCM} {
		t.Run(string(format), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var encoded bytes.Buffer
			err := Transcode(ctx, ffmpeg, bytes.NewReader(pcm), &encoded, Options{Format: format, SampleRate: 44100, Channels: 2, BitrateKbps: 192, RawPCM: true, InputRate: 44100, InputChannels: 2})
			if err != nil || encoded.Len() == 0 {
				t.Fatal("encode failed", err)
			}
			args := []string{"-hide_banner", "-loglevel", "error"}
			if format == PCM {
				args = append(args, "-f", "s16le", "-ar", "44100", "-ac", "2")
			}
			args = append(args, "-i", "pipe:0", "-f", "s16le", "-acodec", "pcm_s16le", "pipe:1")
			cmd := exec.CommandContext(ctx, ffmpeg, args...)
			cmd.Stdin = bytes.NewReader(encoded.Bytes())
			decoded, err := cmd.Output()
			if err != nil {
				t.Fatal("decode failed", err)
			}
			if format != MP3 && !bytes.Equal(decoded, pcm) {
				t.Fatalf("lossless PCM mismatch: got %d want %d", len(decoded), len(pcm))
			}
			if format == MP3 && (len(decoded) < len(pcm) || len(decoded) > len(pcm)+44100/2) {
				t.Fatal("unexpected decoded MP3 duration", len(decoded))
			}
		})
	}
}
