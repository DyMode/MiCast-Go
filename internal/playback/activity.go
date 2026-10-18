package playback

import "encoding/binary"

// Only leading transport silence is suppressed. Once media starts, musical
// silence and a muted phone must neither pause playback nor reset its position.
func pcmHasContent(data []byte) bool {
	for i := 0; i+1 < len(data); i += 2 {
		v := int16(binary.LittleEndian.Uint16(data[i:]))
		if v >= 8 || v <= -8 {
			return true
		}
	}
	return false
}

func (e *Engine) waitForMedia(s *session) bool {
	for {
		s.mu.Lock()
		ready, changed := !s.awaitMedia || s.mediaStarted, s.changed
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return false
		}
		if ready {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-changed:
		}
	}
}
