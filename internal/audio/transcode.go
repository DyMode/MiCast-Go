// Package audio adapts the bundled audio-only FFmpeg runtime.
// Development can also use a compatible system FFmpeg.
package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

type Format string

const (
	MP3  Format = "mp3"
	FLAC Format = "flac"
	WAV  Format = "wav"
	PCM  Format = "pcm"
)

type Options struct {
	Format        Format
	SampleRate    int
	Channels      int
	BitrateKbps   int
	RawPCM        bool
	InputRate     int
	InputChannels int
}

func (o Options) Arguments() ([]string, error) {
	if o.SampleRate != 44100 && o.SampleRate != 48000 {
		return nil, errors.New("output sample rate must be 44100 or 48000")
	}
	if o.Channels != 1 && o.Channels != 2 {
		return nil, errors.New("output channels must be 1 or 2")
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1"}
	args = append(args, "-probesize", "32", "-analyzeduration", "0")
	if o.RawPCM {
		if (o.InputRate != 44100 && o.InputRate != 48000) || (o.InputChannels != 1 && o.InputChannels != 2) {
			return nil, errors.New("invalid PCM input format")
		}
		args = append(args, "-f", "s16le", "-ar", fmt.Sprint(o.InputRate), "-ac", fmt.Sprint(o.InputChannels))
	}
	args = append(args, "-i", "pipe:0", "-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1", "-ar", fmt.Sprint(o.SampleRate), "-ac", fmt.Sprint(o.Channels))
	switch o.Format {
	case MP3:
		if o.BitrateKbps != 128 && o.BitrateKbps != 192 && o.BitrateKbps != 320 {
			return nil, errors.New("invalid MP3 bitrate")
		}
		args = append(args, "-c:a", "libmp3lame", "-b:a", fmt.Sprint(o.BitrateKbps)+"k", "-write_xing", "0", "-id3v2_version", "0", "-f", "mp3")
	case FLAC:
		args = append(args, "-c:a", "flac", "-sample_fmt", "s16", "-f", "flac")
	case WAV:
		args = append(args, "-c:a", "pcm_s16le", "-f", "wav")
	case PCM:
		args = append(args, "-c:a", "pcm_s16le", "-f", "s16le")
	default:
		return nil, errors.New("unsupported output format")
	}
	return append(args, "-flush_packets", "1", "pipe:1"), nil
}

// Transcode reads a finite file or bounded PCM reader; cancellation terminates
// the child process. No shell or user-provided FFmpeg arguments are used.
func Transcode(ctx context.Context, binary string, input io.Reader, output io.Writer, options Options) error {
	args, err := options.Arguments()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = input
	command.Stdout = output
	// Do not expose FFmpeg diagnostics: they can contain media URL credentials.
	command.Stderr = io.Discard
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("音频转换失败，请检查输入格式和音频运行时")
	}
	return nil
}
