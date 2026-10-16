package playback

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const liveReserve = 250 * time.Millisecond

func retryHealthySource(now, lastInput time.Time, h *hub) bool {
	if h == nil || lastInput.IsZero() || now.Sub(lastInput) > time.Second {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.items) > 0 && now.Sub(h.items[len(h.items)-1].at) <= 2*time.Second
}

type chunk struct {
	seq  uint64
	at   time.Time
	data []byte
}
type hub struct {
	mu            sync.Mutex
	items         []chunk
	bytes         int
	seq           uint64
	changed       chan struct{}
	done          bool
	header        []byte
	headerPending []byte
	format        string
	ready         chan struct{}
	once          sync.Once
}

func newHub(format string) *hub {
	return &hub{format: format, changed: make(chan struct{}), ready: make(chan struct{})}
}
func (h *hub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return 0, io.ErrClosedPipe
	}
	data := append([]byte(nil), p...)
	// FLAC headers end after the last metadata block. Retain those headers for
	// late joiners; audio frames are kept in the bounded rolling history.
	if h.format == "flac" && len(h.header) == 0 {
		h.headerPending = append(h.headerPending, data...)
		data = h.headerPending
		if len(data) > 65536 {
			return 0, io.ErrShortBuffer
		}
		if len(data) < 4 {
			return len(p), nil
		}
		if string(data[:4]) != "fLaC" {
			return 0, io.ErrUnexpectedEOF
		}
		end := 4
		for end+4 <= len(data) {
			n := int(data[end+1])<<16 | int(data[end+2])<<8 | int(data[end+3])
			last := data[end]&128 != 0
			end += 4 + n
			if end > len(data) {
				break
			}
			if last {
				h.header = append([]byte(nil), data[:end]...)
				data = data[end:]
				h.headerPending = nil
				break
			}
		}
		if len(h.header) == 0 {
			return len(p), nil
		}
	}
	if len(data) > 0 {
		h.seq++
		h.items = append(h.items, chunk{h.seq, time.Now(), data})
		h.bytes += len(data)
		for len(h.items) > 1 && (h.bytes > 4*1024*1024 || time.Since(h.items[0].at) > 8*time.Second) {
			h.bytes -= len(h.items[0].data)
			h.items[0].data = nil
			h.items = h.items[1:]
		}
		h.once.Do(func() { close(h.ready) })
	}
	close(h.changed)
	h.changed = make(chan struct{})
	return len(p), nil
}
func (h *hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.done {
		h.done = true
		close(h.changed)
	}
}
func (h *hub) next(ctx context.Context, cursor uint64, delay time.Duration, fromStart ...bool) (chunk, bool, error) {
	for {
		h.mu.Lock()
		var found chunk
		ok := false
		if cursor == 0 && len(h.items) > 0 && !(len(fromStart) > 0 && fromStart[0]) {
			cutoff := time.Now().Add(-delay - liveReserve)
			i := len(h.items) - 1
			for i > 0 && h.items[i].at.After(cutoff) {
				i--
			}
			found = h.items[i]
			ok = true
		} else {
			for _, item := range h.items {
				if item.seq > cursor {
					found = item
					ok = true
					break
				}
			}
		}
		if ok && cursor != 0 && len(h.items) > 0 {
			cutoff := h.items[len(h.items)-1].at.Add(-4*time.Second - delay - liveReserve)
			if found.at.Before(cutoff) {
				for _, item := range h.items {
					if !item.at.Before(cutoff) {
						found = item
						break
					}
				}
			}
		}
		changed, done := h.changed, h.done
		h.mu.Unlock()
		if ok {
			return found, true, nil
		}
		if done {
			return chunk{}, false, nil
		}
		select {
		case <-ctx.Done():
			return chunk{}, false, ctx.Err()
		case <-changed:
		}
	}
}
func wavHeader() []byte {
	b := make([]byte, 44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 0xffffffff)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], 44100)
	binary.LittleEndian.PutUint32(b[28:], 176400)
	binary.LittleEndian.PutUint16(b[32:], 4)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 0xffffffff)
	return b
}

// pcmQueue never blocks RTP socket processing. Overflow reports an error so a
// stalled pipeline cannot accumulate unbounded live audio.
type PCMQueue struct {
	ctx        context.Context
	cancel     context.CancelFunc
	data       chan []byte
	rest       []byte
	once       sync.Once
	finishOnce sync.Once
	dropped    atomic.Int64
}

func NewPCMQueue(parent context.Context) *PCMQueue {
	ctx, cancel := context.WithCancel(parent)
	return &PCMQueue{ctx: ctx, cancel: cancel, data: make(chan []byte, 256)}
}
func (q *PCMQueue) Push(p []byte) bool {
	select {
	case <-q.ctx.Done():
		return false
	default:
	}
	select {
	case q.data <- append([]byte(nil), p...):
		return true
	default:
		if q.dropped.Add(int64(len(p))) == int64(len(p)) {
			slog.Warn("音频队列积压，开始丢弃 PCM", "dropped_bytes", len(p))
		}
		return false
	}
}
func (q *PCMQueue) Read(p []byte) (int, error) {
	if len(q.rest) == 0 {
		select {
		case <-q.ctx.Done():
			return 0, io.EOF
		case data, ok := <-q.data:
			if !ok {
				return 0, io.EOF
			}
			q.rest = data
		}
	}
	n := copy(p, q.rest)
	q.rest = q.rest[n:]
	return n, nil
}
func (q *PCMQueue) Finish()      { q.finishOnce.Do(func() { close(q.data) }) }
func (q *PCMQueue) Close() error { q.once.Do(q.cancel); return nil }

func (q *PCMQueue) DroppedBytes() int64 { return q.dropped.Load() }
