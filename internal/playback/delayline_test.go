package playback

import (
	"context"
	"testing"
	"time"
)

func TestDelayLineBurstsAndGapBridgesNeverLoseOrDuplicateAudio(t *testing.T) {
	d := &delayLine{}
	d.configure(24000, 0)
	var received []uint64
	for cycle := 0; cycle < 1000; cycle++ {
		for j := 0; j < 2; j++ {
			seq := uint64(cycle*2 + j + 1)
			if dropped := d.push(chunk{seq: seq, data: make([]byte, 600)}); dropped != 0 {
				t.Fatal("healthy source dropped audio", dropped)
			}
		}
		for {
			item, ok := d.pop(false, false)
			if !ok {
				break
			}
			received = append(received, item.seq)
		}
		if cycle%7 == 0 {
			// A source gap spends the jitter margin without waiting for a new
			// chunk and without sacrificing the alignment hold.
			if item, ok := d.pop(true, false); ok {
				received = append(received, item.seq)
			}
		}
	}
	for {
		item, ok := d.pop(false, true)
		if !ok {
			break
		}
		received = append(received, item.seq)
	}
	if len(received) != 2000 {
		t.Fatal("unnecessary audio loss", len(received))
	}
	for i, seq := range received {
		if seq != uint64(i+1) {
			t.Fatal("lost, split or reordered output", i, seq)
		}
	}
}

func TestDelayLineBridgeRespectsManualAlignmentAndLagIsBounded(t *testing.T) {
	d := &delayLine{}
	d.configure(24000, 100*time.Millisecond)
	for i := 0; i < 4; i++ {
		d.push(chunk{seq: uint64(i + 1), data: make([]byte, 600)})
	}
	if _, ok := d.pop(true, false); ok {
		t.Fatal("bridge consumed the alignment hold")
	}
	d.push(chunk{seq: 5, data: make([]byte, 600)})
	if _, ok := d.pop(true, false); !ok || d.bytes != 2400 {
		t.Fatal("bridge did not spend only jitter margin", d.bytes)
	}
	var dropped uint64
	for i := 0; i < 1000; i++ {
		dropped += d.push(chunk{seq: uint64(i + 6), data: make([]byte, 600)})
	}
	if dropped == 0 || d.bytes > d.reserve+24000*4 {
		t.Fatal("slow client backlog is unbounded", d.bytes, dropped)
	}
	if d.bytes < d.reserve {
		t.Fatal("lag trim destroyed reserve")
	}
}

func TestFLACRateUsesWindowInsteadOfAdjacentMuxerWrites(t *testing.T) {
	h := newHub("flac")
	start := time.Now()
	for i := 0; i < 100; i++ {
		h.items = append(h.items, chunk{seq: uint64(i + 1), at: start.Add(time.Duration(i/2)*50*time.Millisecond + time.Duration(i%2)*time.Microsecond), data: make([]byte, 6000)})
	}
	rate := h.byteRate()
	if rate < 210000 || rate > 270000 {
		t.Fatal("bursty encoder inflated rate estimate", rate)
	}
}

func TestFiniteFirstPullKeepsBeginningAndSlowLiveCursorTrimsWithoutReconnect(t *testing.T) {
	h := newHub("wav")
	now := time.Now()
	for i := 0; i < 100; i++ {
		h.items = append(h.items, chunk{seq: uint64(i + 1), at: now.Add(time.Duration(i-99) * 100 * time.Millisecond), data: []byte{1, 2, 3, 4}})
	}
	first, ok, err := h.next(context.Background(), 0, 0, true)
	if err != nil || !ok || first.seq != 1 {
		t.Fatal("finite media intro was skipped", first.seq, err)
	}
	live, ok, err := h.next(context.Background(), 1, 0)
	if err != nil || !ok || now.Sub(live.at) > 4*time.Second+liveReserve {
		t.Fatal("slow live client did not trim lag", live.seq, err)
	}
}

func TestSpeakerRetryRequiresFlowingSourceAndEncoder(t *testing.T) {
	now := time.Now()
	h := newHub("mp3")
	h.items = []chunk{{at: now}}
	if retryHealthySource(now, now.Add(-3*time.Second), h) {
		t.Fatal("source hole must not trigger cloud replay")
	}
	h.items[0].at = now.Add(-3 * time.Second)
	if retryHealthySource(now, now, h) {
		t.Fatal("encoder hole must not trigger cloud replay")
	}
	h.items[0].at = now
	if !retryHealthySource(now, now, h) {
		t.Fatal("healthy audio must allow a stalled speaker to recover")
	}
}

func TestDecodedLookAheadIsNotSkippedWhenResumingFiniteMedia(t *testing.T) {
	now := time.Now()
	s := &session{protocol: "dlna", position: 10, frames: 7 * 44100, playbackStart: now.Add(-5 * time.Second)}
	if got := s.positionAt(now); got != 15 {
		t.Fatal("decoded look-ahead counted as played", got)
	}
	s.state = "paused"
	s.pausedAt = now
	if got := s.positionAt(now.Add(time.Hour)); got != 15 {
		t.Fatal("pause advanced playback position", got)
	}
}
