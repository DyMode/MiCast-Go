package dlna

import (
	"errors"
	"strconv"

	"micastgo/internal/playback"
)

// Media is the protocol-visible description of one track.
type Media struct {
	URI      string
	Metadata string
}

// Snapshot is the immutable protocol state used by every query and event.
type Snapshot struct {
	Current  Media
	Next     Media
	HasMedia bool
	HasNext  bool
	State    string
	Status   string
	Actions  string
	Position float64
	Duration float64
}

type jobKind uint8

const (
	jobStart jobKind = iota
	jobAdvance
	jobResume
	jobPause
	jobSeek
	jobPrefetch
)

type job struct {
	kind         jobKind
	cast, track  uint64
	seq          uint64
	owner        string
	uri          string
	info         playback.Track
	position     float64
	continuation bool
}

func (j job) trackID() string { return strconv.FormatUint(j.track, 10) }

func (s *Server) enqueue(j job) bool {
	select {
	case s.jobs <- j:
		return true
	default:
		s.stateMu.Lock()
		s.lastError = "投送队列已满"
		s.stateMu.Unlock()
		return false
	}
}

// submit keeps pendingStarts balanced when a queue is saturated.
func (s *Server) submit(j job) {
	if s.enqueue(j) {
		return
	}
	if j.kind == jobStart || j.kind == jobAdvance {
		s.stateMu.Lock()
		if j.cast == s.cast && j.track == s.trackID {
			if s.pendingStarts > 0 {
				s.pendingStarts--
			}
			s.desired = "STOPPED"
		}
		s.stateMu.Unlock()
	}
}

func (s *Server) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case j := <-s.jobs:
			if s.ctx.Err() != nil {
				return
			}
			s.execute(j)
		}
	}
}

func (s *Server) execute(j job) {
	// A queued command from an older cast must not create a pipeline.
	s.stateMu.Lock()
	current := j.cast == s.cast && (j.kind == jobPrefetch || j.track == s.trackID)
	if !current && (j.kind == jobStart || j.kind == jobAdvance) && s.pendingStarts > 0 {
		s.pendingStarts--
	}
	s.stateMu.Unlock()
	if !current {
		return
	}
	switch j.kind {
	case jobStart:
		status := s.engine.Status()
		// A repeated Play must not reopen a pipeline that is already starting.
		if status.Protocol == "dlna" && status.TrackID == j.trackID() &&
			(j.owner == "" || status.SessionID == j.owner) &&
			(status.State == "playing" || status.State == "transitioning" || status.State == "waiting_for_speakers") {
			s.commitStart(j, playback.Receipt{Identity: status.SessionID, PipelineID: status.PipelineID, TrackID: status.TrackID}, nil)
			return
		}
		receipt, err := s.engine.StartFile(playback.StartRequest{Seq: j.seq, Protocol: "dlna", TrackID: j.trackID(), Owner: j.owner, URI: j.uri, Track: j.info, Position: j.position, Continuation: j.continuation})
		s.commitStart(j, receipt, err)
	case jobAdvance:
		receipt, err := s.engine.AdvanceFile(playback.StartRequest{Seq: j.seq, Protocol: "dlna", TrackID: j.trackID(), Owner: j.owner, URI: j.uri, Track: j.info, Continuation: true})
		s.commitStart(j, receipt, err)
	case jobResume:
		s.commitControl(j, s.engine.Resume(s.ctx, j.owner))
	case jobPause:
		s.commitControl(j, s.engine.Pause(s.ctx, j.owner))
	case jobSeek:
		s.commitControl(j, s.engine.Seek(j.position, j.owner))
	case jobPrefetch:
		s.engine.PrefetchNext(j.owner, j.uri)
	}
}

func (s *Server) commitStart(j job, receipt playback.Receipt, err error) {
	s.stateMu.Lock()
	current := j.cast == s.cast && j.track == s.trackID
	if s.pendingStarts > 0 {
		s.pendingStarts--
	}
	nextURI := ""
	if current {
		if err == nil {
			s.owner = receipt.Identity
			s.engineTrackID = j.trackID()
			s.lastError = ""
			nextURI = s.nextURI
		} else if errors.Is(err, playback.ErrSuperseded) {
			// Another protocol won the same entry. Converge without retrying.
			s.owner, s.engineTrackID = "", ""
			s.desired, s.lastError = "STOPPED", ""
		} else {
			s.owner, s.engineTrackID = "", ""
			s.desired = "STOPPED"
			s.lastError = err.Error()
		}
	}
	s.stateMu.Unlock()
	if nextURI != "" {
		s.enqueue(job{kind: jobPrefetch, cast: j.cast, owner: receipt.Identity, uri: nextURI})
	}
	if !current && err == nil {
		// The cast moved on while this pipeline was starting. Stop it, but
		// never touch a newer pipeline that already adopted the identity.
		s.stateMu.Lock()
		active := s.owner == receipt.Identity && receipt.Identity != ""
		s.stateMu.Unlock()
		if !active {
			s.engine.Preempt(receipt.Identity)
		}
	}
}

func (s *Server) commitControl(j job, err error) {
	if err == nil {
		return
	}
	s.stateMu.Lock()
	if j.cast == s.cast && j.track == s.trackID {
		s.lastError = err.Error()
	}
	s.stateMu.Unlock()
}

func (s *Server) snapshotLocked(status playback.Status) Snapshot {
	state := s.stateLocked(status)
	duration := s.durationLocked(status)
	position := s.positionLocked(status)
	if state == "PLAYING" && duration > 0 {
		position = min(position, max(0, duration-1))
	}
	statusText := "OK"
	if s.lastError != "" || (s.owner != "" && status.SessionID == s.owner && status.State == "error") {
		statusText = "ERROR_OCCURRED"
	}
	return Snapshot{
		Current: Media{
			URI:      s.uri,
			Metadata: s.metadata,
		},
		Next: Media{
			URI:      s.nextURI,
			Metadata: s.nextMetadata,
		},
		HasMedia: s.uri != "",
		HasNext:  s.nextURI != "",
		State:    state,
		Status:   statusText,
		Actions:  s.actionsLocked(state),
		Position: position,
		Duration: duration,
	}
}

func (s *Server) stateLocked(status playback.Status) string {
	if s.owner == "" {
		if s.desired == "PLAYING" && s.uri != "" {
			return "TRANSITIONING"
		}
		return "STOPPED"
	}
	if status.SessionID != s.owner || status.Protocol != "dlna" {
		if s.pendingStarts > 0 {
			return "TRANSITIONING"
		}
		return "STOPPED"
	}
	switch status.State {
	case "playing":
		return "PLAYING"
	case "paused":
		return "PAUSED_PLAYBACK"
	case "transitioning", "waiting_for_speakers":
		return "TRANSITIONING"
	default:
		return "STOPPED"
	}
}

// Position is only taken from a pipeline carrying this track's generation.
func (s *Server) positionLocked(status playback.Status) float64 {
	if s.owner != "" && status.SessionID == s.owner && status.Protocol == "dlna" {
		if s.engineTrackID == "" || status.TrackID == "" || status.TrackID == s.engineTrackID {
			return status.Position
		}
	}
	return s.positionBase
}

func (s *Server) durationLocked(status playback.Status) float64 {
	if s.owner != "" && status.SessionID == s.owner && status.Protocol == "dlna" && status.Track.Duration > 0 {
		return status.Track.Duration
	}
	return s.track.Duration
}

func (s *Server) actionsLocked(state string) string {
	if s.uri == "" {
		return "Play"
	}
	actions := "Play,Seek"
	switch state {
	case "PLAYING":
		actions = "Pause,Stop,Seek"
	case "PAUSED_PLAYBACK":
		actions = "Play,Stop,Seek"
	case "TRANSITIONING":
		actions = "Stop,Seek"
	}
	if s.nextURI != "" {
		actions += ",Next"
	}
	return actions
}

// advanceLocked promotes the queued track after a natural end. It never
// publishes a synthetic STOPPED and never waits on the engine.
func (s *Server) advanceLocked(status playback.Status) {
	s.stateMu.Lock()
	// Another protocol or a Web cast took the engine. Converge to STOPPED and
	// drop the stale continuation instead of trying to win the entry back.
	if s.pendingStarts == 0 && status.State != "" && status.State != "stopped" && status.State != "error" &&
		(status.SessionID != s.owner || (status.SessionID != "" && status.Protocol != "dlna")) {
		if s.owner != "" || s.nextURI != "" {
			s.cast++
			s.owner, s.engineTrackID = "", ""
			s.desired = "STOPPED"
			s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
			s.pendingStarts = 0
		}
		s.stateMu.Unlock()
		return
	}
	if status.SessionID == s.owner && status.Protocol == "dlna" && !status.Finished &&
		(status.EndReason == "stopped" || status.EndReason == "failed" || status.EndReason == "targets_lost") {
		s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
		s.stateMu.Unlock()
		return
	}
	if !status.Finished || status.State != "stopped" || status.Protocol != "dlna" || status.SessionID != s.owner || s.nextURI == "" {
		s.stateMu.Unlock()
		return
	}
	owner := s.owner
	uri, meta, track := s.nextURI, s.nextMetadata, s.nextTrack
	s.trackID++
	s.uri, s.metadata, s.track = uri, meta, track
	s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
	s.owner, s.engineTrackID = "", ""
	s.desired = "PLAYING"
	s.positionBase = 0
	s.pendingStarts++
	j := job{kind: jobAdvance, cast: s.cast, track: s.trackID, seq: s.engine.Sequence(), owner: owner, uri: uri, info: track, continuation: true}
	s.stateMu.Unlock()
	s.submit(j)
}

func (s *Server) snapshot() Snapshot {
	status := s.engine.Status()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.snapshotLocked(status)
}
