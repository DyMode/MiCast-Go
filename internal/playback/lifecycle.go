package playback

import "time"

func (s *session) signalLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

func (e *Engine) sourceLive(s *session) bool {
	if e.closing.Load() || s.ctx.Err() != nil || !e.isCurrent(s) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.desiredPaused && s.state != "error" && s.state != "stopped" && s.frames > 0 &&
		(s.protocol != "airplay" || time.Since(s.lastInput) < 2*time.Second)
}

func (e *Engine) waitRunnable(s *session) bool {
	for {
		if e.closing.Load() || s.ctx.Err() != nil {
			return false
		}
		s.mu.Lock()
		ready := !s.desiredPaused && (s.protocol != "airplay" || time.Since(s.lastInput) < 2*time.Second)
		changed := s.changed
		s.mu.Unlock()
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

// Readiness timeout counts active time only. Pausing preparation must neither
// start a speaker nor turn an intentionally paused source into a timeout.
func (e *Engine) waitReady(s *session, ready <-chan struct{}) bool {
	remaining := 20 * time.Second
	for {
		if !e.waitRunnableForPreparation(s) {
			return false
		}
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		timer := time.NewTimer(remaining)
		start := time.Now()
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return false
		case <-ready:
			timer.Stop()
			return true
		case <-changed:
			timer.Stop()
			remaining -= time.Since(start)
			if remaining <= 0 {
				e.fail(s, "音频源未产生可播放数据")
				return false
			}
		case <-timer.C:
			e.fail(s, "音频源未产生可播放数据")
			return false
		}
	}
}

func (e *Engine) waitRunnableForPreparation(s *session) bool {
	for {
		s.mu.Lock()
		paused, changed := s.desiredPaused, s.changed
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return false
		}
		if !paused {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-changed:
		}
	}
}

func (e *Engine) retire(s *session, reason string) {
	e.clearPrefetch(s.identity)
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.state, s.endReason, s.finished = "stopped", reason, false
	s.signalLocked()
	s.mu.Unlock()
	e.cancel(s)
	e.cleanupRoute(s)
	e.notifyChange()
}
