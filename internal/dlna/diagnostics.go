package dlna

import (
	"net/http"
	"time"
)

// Only protocol state is retained, never media URLs, metadata or callbacks.
type ProtocolEvent struct {
	At       time.Time `json:"at"`
	Service  string    `json:"service"`
	Action   string    `json:"action"`
	Code     int       `json:"code"`
	State    string    `json:"state"`
	Position string    `json:"position,omitempty"`
	Duration string    `json:"duration,omitempty"`
	Meta     string    `json:"meta,omitempty"`
	Sequence *uint32   `json:"sequence,omitempty"`
}

type Diagnostics struct {
	State         string          `json:"state"`
	Position      float64         `json:"position"`
	Subscriptions int             `json:"subscriptions"`
	Queued        bool            `json:"queued"`
	Events        []ProtocolEvent `json:"events"`
}

func (s *Server) Diagnostics() Diagnostics {
	status := s.engine.Status()
	s.mu.Lock()
	subscriptions := len(s.subscriptions)
	trace := append([]ProtocolEvent(nil), s.trace...)
	s.mu.Unlock()
	s.stateMu.Lock()
	snap := s.snapshotLocked(status)
	s.stateMu.Unlock()
	return Diagnostics{State: snap.State, Position: snap.Position, Subscriptions: subscriptions, Queued: snap.HasNext, Events: trace}
}

func (s *Server) appendProtocol(event ProtocolEvent) {
	s.trace = append(s.trace, event)
	if len(s.trace) > 128 {
		s.trace = append([]ProtocolEvent(nil), s.trace[len(s.trace)-128:]...)
	}
}

func (s *Server) recordProtocol(service, action string, code int, response map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := response["CurrentTransportState"]
	if state == "" {
		state = s.transportState(s.engine.Status())
	}
	duration := response["TrackDuration"]
	if value := response["_duration"]; value != "" {
		duration = value
	}
	now := time.Now()
	// A controller polls progress every second. Keeping every poll would
	// rotate the bounded trace within two minutes and erase the commands that
	// matter (SetURI/Next/Play/Stop). Keep mutations and state/duration
	// changes, and sample the rest at most once per five seconds.
	if isReadOnlyAction(action) {
		if !s.pollAt.IsZero() && now.Sub(s.pollAt) < 5*time.Second && state == s.pollState && duration == s.pollDuration {
			return
		}
		s.pollAt, s.pollState, s.pollDuration = now, state, duration
	}
	s.appendProtocol(ProtocolEvent{At: now, Service: service, Action: action, Code: code, State: state, Position: response["RelTime"], Duration: duration, Meta: response["_meta"]})
}

func isReadOnlyAction(action string) bool {
	switch action {
	case "GetPositionInfo", "GetTransportInfo", "GetMediaInfo", "GetCurrentTransportActions",
		"GetVolume", "GetMute", "GetProtocolInfo", "GetDeviceCapabilities",
		"GetTransportSettings", "GetCurrentConnectionIDs", "GetCurrentConnectionInfo":
		return true
	default:
		return false
	}
}

type subscriptionResponse struct {
	http.ResponseWriter
	code int
}

func (w *subscriptionResponse) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *subscriptionResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
