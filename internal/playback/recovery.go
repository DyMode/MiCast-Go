package playback

import "time"

type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Speaker string    `json:"speaker,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
}

func (s *session) record(kind, speaker string, attempt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{time.Now(), kind, speaker, attempt})
	if len(s.events) > 48 {
		s.events = append([]Event(nil), s.events[len(s.events)-48:]...)
	}
}

type recoveryState struct {
	suspect, attempted time.Time
	baseline           int64
	attempts           int
	awaiting           bool
}

// A cloud acceptance is not recovery. Only renewed output advances the state.
func (r *recoveryState) check(now time.Time, p Pull, sourceHealthy bool) (retry, recovered bool, health string) {
	flowing := !p.LastRead.IsZero() && now.Sub(p.LastRead) < 5*time.Second
	if r.awaiting && p.Bytes > r.baseline && p.LastRead.After(r.attempted) {
		*r = recoveryState{}
		return false, true, "flowing"
	}
	if flowing && !r.awaiting {
		r.suspect = time.Time{}
		return false, false, "flowing"
	}
	if r.awaiting && now.Sub(r.attempted) < 8*time.Second {
		return false, false, "recovering"
	}
	if r.suspect.IsZero() {
		r.suspect = now
	}
	if !sourceHealthy {
		return false, false, "source_quiet"
	}
	if r.attempts >= 3 {
		return false, false, "unavailable"
	}
	cooldown := time.Duration(15*(1<<max(0, r.attempts-1))) * time.Second
	if now.Sub(r.suspect) < 6*time.Second || (!r.attempted.IsZero() && now.Sub(r.attempted) < cooldown) {
		return false, false, "observing"
	}
	r.attempts++
	r.attempted, r.baseline, r.awaiting = now, p.Bytes, true
	return true, false, "recovering"
}
