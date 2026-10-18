package playback

import "time"

// These rules follow MiCast's stream_server delay line: retain 250ms, spend
// that margin after a 150ms arrival gap, cap lag at four seconds, and never
// slice encoder output or pace a second time by nominal bitrate.
const bridgeGap = 150 * time.Millisecond

// Caller holds s.mu. Decoded look-ahead is buffered audio, not played position.
func (s *session) positionAt(now time.Time) float64 {
	seconds := float64(s.frames) / 44100
	if s.mode == fileSource {
		if s.playbackStart.IsZero() {
			return s.position
		}
		if s.state == "paused" && !s.pausedAt.IsZero() {
			now = s.pausedAt
		}
		seconds = min(seconds, max(0, now.Sub(s.playbackStart).Seconds()))
	}
	return s.position + seconds
}

type delayLine struct {
	items   []chunk
	bytes   int
	rate    float64
	hold    int
	reserve int
}

func (d *delayLine) configure(rate float64, delay time.Duration) {
	if rate <= 0 {
		return
	}
	// An observed FLAC rate is adopted while filling, then held steady.
	if d.rate == 0 || d.bytes < d.reserve {
		d.rate = rate
	}
	hold := int(d.rate * delay.Seconds())
	if hold < d.hold {
		remaining := d.hold - hold
		for len(d.items) > 0 && remaining > 0 {
			item := d.take()
			remaining -= len(item.data)
		}
	}
	d.hold = hold
	d.reserve = d.hold + int(d.rate*liveReserve.Seconds())
}

func (d *delayLine) take() chunk {
	item := d.items[0]
	d.items[0] = chunk{}
	d.items = d.items[1:]
	d.bytes -= len(item.data)
	return item
}

func (d *delayLine) push(item chunk) uint64 {
	d.items = append(d.items, item)
	d.bytes += len(item.data)
	var skipped uint64
	if d.rate > 0 {
		ceiling := d.reserve + int(d.rate*4)
		for len(d.items) > 1 && d.bytes > ceiling && d.bytes-len(d.items[0].data) >= d.reserve {
			d.take()
			skipped++
		}
	}
	return skipped
}

func (d *delayLine) pop(gap, finished bool) (chunk, bool) {
	if len(d.items) == 0 {
		return chunk{}, false
	}
	floor := d.reserve
	if gap {
		floor = d.hold
	}
	if !finished && d.rate > 0 && d.bytes-len(d.items[0].data) < floor {
		return chunk{}, false
	}
	return d.take(), true
}

func (h *hub) byteRate() float64 {
	switch h.format {
	case "mp3":
		return 192000 / 8
	case "wav", "pcm":
		return 44100 * 4
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Rate is bytes over a real window, excluding the first sample's bytes.
	// Adjacent muxer writes may arrive microseconds apart; their instantaneous
	// rate would exaggerate the FLAC reserve by orders of magnitude.
	if len(h.items) < 20 {
		return 0
	}
	last := h.items[len(h.items)-1].at
	first := len(h.items) - 1
	bytes := 0
	for first > 0 {
		bytes += len(h.items[first].data)
		first--
		if last.Sub(h.items[first].at) >= time.Second {
			break
		}
	}
	span := last.Sub(h.items[first].at).Seconds()
	if span < 0.5 {
		return 0
	}
	rate := float64(bytes) / span
	if rate < 8000 {
		return 0
	}
	return rate
}
