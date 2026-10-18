package playback

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"micastgo/internal/app"
	"micastgo/internal/audio"
	"micastgo/internal/store"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Track struct {
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Duration float64 `json:"duration"`
}
type Pull struct {
	Health       string    `json:"health,omitempty"`
	Bridges      uint64    `json:"bridges"`
	BufferedMS   int       `json:"buffered_ms"`
	HistorySkips uint64    `json:"history_skips"`
	Retries      int       `json:"retries"`
	Connections  int       `json:"connections"`
	Bytes        int64     `json:"bytes"`
	LastRead     time.Time `json:"last_read"`
	Completed    bool      `json:"completed"`
}
type Status struct {
	Activity             string          `json:"activity"`
	Equalizer            store.Equalizer `json:"equalizer"`
	Clipped              uint64          `json:"clipped_samples"`
	Events               []Event         `json:"events,omitempty"`
	VolumeMode           string          `json:"volume_mode"`
	InputDroppedBytes    int64           `json:"input_dropped_bytes"`
	EncodingDroppedBytes int64           `json:"encoding_dropped_bytes"`
	Finished             bool            `json:"finished"`
	SessionID            string          `json:"session_id,omitempty"`
	PipelineID           string          `json:"pipeline_id,omitempty"`
	TrackID              string          `json:"track_id,omitempty"`
	Protocol             string          `json:"protocol"`
	State                string          `json:"state"`
	Track                Track           `json:"track"`
	Position             float64         `json:"position"`
	Error                string          `json:"error,omitempty"`
	Streams              map[string]Pull `json:"streams"`
	Format               string          `json:"format"`
	EndReason            string          `json:"end_reason,omitempty"`
}

// A source is either a finite media file (DLNA/Web) or a continuous PCM
// stream (AirPlay). Protocol names are not used to decide playback behavior.
type sourceMode uint8

const (
	streamSource sourceMode = iota
	fileSource
)

type session struct {
	awaitMedia      bool
	mediaStarted    bool
	account         string
	equalizerConfig store.Equalizer
	equalizer       equalizer
	clipped         uint64
	prefetched      *prefetch
	events          []Event
	hardwareVolume  *int
	volumeMode      string
	targetGain      float64
	continuation    bool
	finished        bool
	id              string
	identity        string // Logical cast; id is the revocable media generation.
	trackID         string // Protocol-visible track generation, set by the adapter.
	mode            sourceMode
	mediaURL        string
	ctx             context.Context
	cancel          context.CancelFunc
	source          io.ReadCloser
	mu              sync.Mutex
	hubs            map[string]*hub
	queues          map[string]*PCMQueue
	pulls           map[string]Pull
	protocol        string
	track           Track
	state           string
	err             string
	started         time.Time
	position        float64
	frames          int64
	metadataSeq     uint64
	metadataCancel  context.CancelFunc
	pullReady       chan struct{}
	pullOnce        sync.Once
	playbackStart   time.Time
	displayRev      uint64
	sourceEnded     bool
	lastInput       time.Time
	pausedAt        time.Time
	desiredPaused   bool
	published       bool
	prepared        bool
	endReason       string
	changed         chan struct{}
}
type Engine struct {
	transport              http.RoundTripper // Optional media transport; production uses validated LAN dialing.
	prefetchMu             sync.Mutex
	prefetch               *prefetch
	requestedMuted         bool
	requestedVolume        *int // Protected by op; remembered before playback starts.
	closed                 bool // Protected by op.
	closing                atomic.Bool
	route                  string
	op                     sync.Mutex
	mu                     sync.RWMutex
	ctx                    context.Context
	app                    *app.App
	binary, base, cacheDir string
	current                *session
	secret                 []byte
	cacheMu                sync.Mutex
	cachePath, cacheURL    string
	volumeMu               sync.Mutex
	sequence               atomic.Uint64
	lastStart              atomic.Uint64
	wg                     sync.WaitGroup
	changes                chan struct{}
}

func New(ctx context.Context, a *app.App, binary, base, dir string, route ...string) (*Engine, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, errors.New("未找到音频运行时")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	routeID := ""
	if len(route) > 0 {
		routeID = route[0]
	}
	return &Engine{route: routeID, ctx: ctx, app: a, binary: binary, base: strings.TrimRight(base, "/"), cacheDir: dir, secret: secret, changes: make(chan struct{}, 1)}, nil
}
func (e *Engine) Status() Status {
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	out := Status{State: "stopped", Streams: map[string]Pull{}, Format: e.app.Config().Format}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Events = append([]Event(nil), s.events...)
	out.VolumeMode = s.volumeMode
	out.Equalizer = s.equalizerConfig
	out.Clipped = s.clipped
	out.Finished = s.finished
	out.Protocol = s.protocol
	out.SessionID = s.identity
	out.PipelineID = s.id
	out.TrackID = s.trackID
	out.State = s.state
	out.Activity = s.state
	if s.state != "paused" && s.state != "stopped" && s.state != "error" {
		switch {
		case s.awaitMedia && !s.mediaStarted:
			// AirPlay: the transport is open but no audible PCM has arrived.
			out.Activity = "connected"
		case s.mode == fileSource && s.state == "playing" && s.playbackStart.IsZero():
			// DLNA: the cloud accepted the command but the speaker has not
			// opened its stream yet. AirPlay never uses playbackStart.
			out.Activity = "transitioning"
		}
	}
	out.Track = s.track
	out.Error = s.err
	out.EndReason = s.endReason
	out.Position = s.positionAt(time.Now())
	if q, ok := s.source.(*PCMQueue); ok {
		out.InputDroppedBytes = q.DroppedBytes()
	}
	for _, q := range s.queues {
		out.EncodingDroppedBytes += q.DroppedBytes()
	}
	for id, v := range s.pulls {
		out.Streams[id] = v
	}
	return out
}
func (e *Engine) isCurrent(s *session) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.current == s
}
func (e *Engine) newSession(protocol string, track Track, position float64, identity ...string) *session {
	return e.newSessionFor(modeForProtocol(protocol), protocol, "", track, position, identity...)
}
func modeForProtocol(protocol string) sourceMode {
	if protocol == "dlna" || protocol == "web" {
		return fileSource
	}
	return streamSource
}
func (e *Engine) newSessionFor(mode sourceMode, protocol, trackID string, track Track, position float64, identity ...string) *session {
	if len(identity) == 0 || identity[0] == "" {
		e.clearPrefetch("")
	}
	b := make([]byte, 16)
	rand.Read(b)
	ctx, cancel := context.WithCancel(e.ctx)
	s := &session{id: hex.EncodeToString(b), ctx: ctx, cancel: cancel, hubs: map[string]*hub{}, queues: map[string]*PCMQueue{}, pulls: map[string]Pull{}, protocol: protocol, trackID: trackID, mode: mode, track: track, state: "transitioning", started: time.Now(), position: position, pullReady: make(chan struct{})}
	e.mu.Lock()
	old := e.current
	s.identity = s.id
	s.changed = make(chan struct{})
	if len(identity) > 0 && identity[0] != "" {
		s.identity = identity[0]
	}
	config := e.app.Config()
	s.account = config.Owner
	s.volumeMode = config.VolumeMode
	s.equalizerConfig = config.Equalizer
	if old != nil && old.identity == s.identity {
		s.volumeMode = old.volumeMode
		s.equalizerConfig = old.equalizerConfig
	}
	// An explicit mute belongs to its protocol cast, not a later takeover.
	e.volumeMu.Lock()
	if old != nil && old.protocol != protocol {
		e.requestedMuted = false
	}
	s.equalizer = newEqualizer(s.equalizerConfig)
	s.targetGain = 1
	if e.requestedVolume != nil {
		s.targetGain = volumeGain(*e.requestedVolume)
	}
	if e.requestedMuted {
		s.targetGain = 0
	}
	if e.requestedVolume != nil || e.requestedMuted {
		v := e.effectiveVolume()
		s.hardwareVolume = &v
	}
	e.volumeMu.Unlock()
	s.events = []Event{{At: time.Now(), Kind: "session_started"}}
	e.current = s
	e.mu.Unlock()
	if old != nil {
		e.cancel(old)
		old.mu.Lock()
		published := old.published
		old.mu.Unlock()
		if published {
			e.cleanupRoute(old)
		}
	}
	return s
}
func (e *Engine) cancel(s *session) {
	s.cancel()
	s.mu.Lock()
	if s.prefetched != nil {
		s.prefetched.cancel()
	}
	if s.source != nil {
		s.source.Close()
	}
	for _, q := range s.queues {
		q.Close()
	}
	for _, h := range s.hubs {
		h.Close()
	}
	s.mu.Unlock()
}
func (e *Engine) StartPCM(input io.ReadCloser, track Track, initialVolume ...int) (string, error) {
	receipt, err := e.StartStream(StartRequest{Track: track}, input, initialVolume...)
	return receipt.Identity, err
}

// StartPCMFor restarts an existing protocol transport only if it still owns
// the cast. Validation and replacement share the same operation lock.
func (e *Engine) StartPCMFor(owner string, input io.ReadCloser, track Track, initialVolume ...int) (string, error) {
	receipt, err := e.StartStream(StartRequest{Owner: owner, Seq: e.Sequence(), Continuation: true, Track: track, AwaitMedia: true}, input, initialVolume...)
	return receipt.Identity, err
}
func (e *Engine) prepare(s *session) {
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.watch(s) }()
	config := e.app.Config()
	e.ensureHub(s, config.Format)
	for _, speaker := range config.RouteTargets(e.route) {
		e.ensureHub(s, speaker.Format)
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		s.mu.Lock()
		h := s.hubs[config.Format]
		s.mu.Unlock()
		if !e.waitForMedia(s) {
			return
		}
		if !e.waitReady(s, h.ready) {
			return
		}
		s.mu.Lock()
		track := s.track
		s.mu.Unlock()
		audioID := e.match(s.ctx, track)
		for {
			if !e.waitRunnable(s) {
				return
			}
			e.op.Lock()
			if !e.isCurrent(s) || s.ctx.Err() != nil {
				e.op.Unlock()
				return
			}
			s.mu.Lock()
			if s.desiredPaused {
				s.mu.Unlock()
				e.op.Unlock()
				continue
			}
			s.published = true
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
			results, err := e.app.BeginGuardedRouteStream(ctx, e.route, "stream:"+s.id, audioID, func(sp store.Speaker) string { return e.url(s, sp) }, s.continuation, func() bool { return e.sourceLive(s) }, func() { e.retire(s, "targets_lost") }, app.StreamOptions{Background: func() bool { s.mu.Lock(); ended := s.sourceEnded; s.mu.Unlock(); return !ended && e.sourceLive(s) }, Volume: func() (int, bool) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.volumeMode == "linked" && s.hardwareVolume != nil {
					return *s.hardwareVolume, true
				}
				return 0, false
			}})
			if err != nil {
				cancel()
				e.op.Unlock()
				if s.ctx.Err() != nil {
					return
				}
				if errors.Is(err, app.ErrSuperseded) {
					continue
				}
				e.fail(s, err.Error())
				return
			}
			s.mu.Lock()
			pausedNow := s.desiredPaused
			s.mu.Unlock()
			if pausedNow {
				cancel()
				e.op.Unlock()
				continue
			}
			if s.ctx.Err() != nil {
				cancel()
				e.op.Unlock()
				return
			}
			if len(results) == 0 {
				cancel()
				e.op.Unlock()
				e.retire(s, "targets_lost")
				return
			}
			cancel()
			accepted := false
			for _, r := range results {
				if r == "accepted" {
					accepted = true
				}
			}
			s.mu.Lock()
			s.prepared = true
			if s.state == "transitioning" {
				if accepted {
					s.state = "playing"
				} else {
					s.err = "音箱尚未接受播放指令"
					s.state = "waiting_for_speakers"
				}
			}
			s.mu.Unlock()
			e.op.Unlock()
			if accepted {
				s.record("route_ready", "", 0)
			} else {
				s.record("route_waiting", "", 0)
			}
			return
		}
	}()
}
func (e *Engine) watch(s *session) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	recovery := map[string]*recoveryState{}
	var unavailableSince, observedAt time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			state, ended, started, lastInput := s.state, s.sourceEnded, s.started, s.lastInput
			pulls := make(map[string]Pull, len(s.pulls))
			for id, p := range s.pulls {
				pulls[id] = p
			}
			hubs := make(map[string]*hub, len(s.hubs))
			for f, h := range s.hubs {
				hubs[f] = h
			}
			s.mu.Unlock()
			if !ended && (state == "playing" || state == "paused" || state == "waiting_for_speakers") && now.Sub(observedAt) >= 30*time.Second {
				observedAt = now
				ctx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
				e.app.ObserveRoute(ctx, e.route, "stream:"+s.id)
				cancel()
				// Readback can wait on the network; recovery must use fresh flow data.
				s.mu.Lock()
				state, ended, started, lastInput = s.state, s.sourceEnded, s.started, s.lastInput
				clear(pulls)
				for id, p := range s.pulls {
					pulls[id] = p
				}
				clear(hubs)
				for format, h := range s.hubs {
					hubs[format] = h
				}
				s.mu.Unlock()
				now = time.Now()
			}
			if ended || (state != "playing" && state != "waiting_for_speakers") || now.Sub(started) < 6*time.Second {
				unavailableSince = time.Time{}
				continue
			}
			healthy := false
			ids := map[string]bool{}
			for _, target := range e.app.Config().RouteTargets(e.route) {
				if !e.app.RouteOwns(e.route, target.ID) {
					continue
				}
				p := pulls[target.ID]
				if !p.LastRead.IsZero() && now.Sub(p.LastRead) < 5*time.Second {
					healthy = true
				}
				r := recovery[target.ID]
				if r == nil {
					r = &recoveryState{}
					recovery[target.ID] = r
				}
				retry, recovered, health := r.check(now, p, retryHealthySource(now, lastInput, hubs[target.Format]))
				s.mu.Lock()
				current := s.pulls[target.ID]
				current.Health = health
				if retry {
					current.Retries++
				}
				s.pulls[target.ID] = current
				s.mu.Unlock()
				if recovered {
					s.record("recovery_confirmed", target.ID, 0)
				}
				if retry {
					ids[target.ID] = true
					s.record("recovery_requested", target.ID, r.attempts)
				}
			}
			if healthy {
				unavailableSince = time.Time{}
			} else if unavailableSince.IsZero() {
				unavailableSince = now
			} else if now.Sub(unavailableSince) > 90*time.Second {
				s.record("recovery_exhausted", "", 0)
				e.fail(s, "音箱持续无法拉取音频流")
				return
			}
			if len(ids) > 0 {
				e.op.Lock()
				if e.isCurrent(s) && s.ctx.Err() == nil {
					ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
					err := e.app.RetryRouteTargets(ctx, e.route, "stream:"+s.id, ids)
					cancel()
					if err != nil && s.ctx.Err() == nil {
						s.record("recovery_command_failed", "", 0)
					}
				}
				e.op.Unlock()
			}
		}
	}
}
func (e *Engine) match(ctx context.Context, t Track) string {
	if t.Title == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	id, _ := e.app.SearchTrack(ctx, t.Title, t.Artist)
	return id
}
func (e *Engine) ensureHub(s *session, format string) *hub {
	if !store.ValidFormat(format) {
		format = "mp3"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hubs[format]; h != nil {
		return h
	}
	h := newHub(format)
	s.hubs[format] = h
	q := NewPCMQueue(s.ctx)
	s.queues[format] = q
	if format == "wav" || format == "pcm" {
		return h
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer h.Close()
		defer q.Close()
		err := audio.Transcode(s.ctx, e.binary, q, h, audio.Options{Format: audio.Format(format), SampleRate: 44100, Channels: 2, BitrateKbps: 192, RawPCM: true, InputRate: 44100, InputChannels: 2})
		if err != nil && s.ctx.Err() == nil {
			e.fail(s, "音频编码失败（"+format+"）")
		}
	}()
	return h
}
func (e *Engine) consume(s *session, input io.Reader) {
	defer func() {
		s.mu.Lock()
		s.sourceEnded = true
		for _, q := range s.queues {
			q.Finish()
		}
		for f, h := range s.hubs {
			if f == "wav" || f == "pcm" {
				h.Close()
			}
		}
		s.mu.Unlock()
		if s.ctx.Err() == nil {
			if s.mode == fileSource {
				s.mu.Lock()
				waiting := s.playbackStart.IsZero()
				s.mu.Unlock()
				if waiting {
					select {
					case <-s.ctx.Done():
						return
					case <-s.pullReady:
					case <-time.After(25 * time.Second):
						e.fail(s, "音箱未连接本地音频流")
						return
					}
				}
			}
			if s.mode == fileSource {
				e.finishMedia(s)
			} else {
				select {
				case <-s.ctx.Done():
				case <-time.After(7 * time.Second):
					e.stop(s.id, true)
				}
			}
		}
	}()
	buffer := make([]byte, 4096)
	s.mu.Lock()
	gain := s.targetGain
	s.mu.Unlock()
	pending := []byte{}
	for {
		if s.mode == fileSource {
			s.mu.Lock()
			frames, start := s.frames, s.playbackStart
			s.mu.Unlock()
			if frames >= 88200 && start.IsZero() {
				select {
				case <-s.ctx.Done():
					return
				case <-s.pullReady:
				case <-time.After(25 * time.Second):
					e.fail(s, "音箱未连接本地音频流")
					return
				}
			}
			s.mu.Lock()
			start = s.playbackStart
			s.mu.Unlock()
			if !start.IsZero() {
				wait := time.Until(start.Add(time.Duration(float64(frames-88200) / 44100 * float64(time.Second))))
				if wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-s.ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}
		for {
			s.mu.Lock()
			paused := s.state == "paused" && s.mode == fileSource
			s.mu.Unlock()
			if !paused {
				break
			}
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		n, err := input.Read(buffer)
		data := append(pending, buffer[:n]...)
		aligned := len(data) / 4 * 4
		pending = append([]byte(nil), data[aligned:]...)
		data = data[:aligned]
		n = len(data)
		if n > 0 {
			s.mu.Lock()
			s.lastInput = time.Now()
			if s.awaitMedia && !s.mediaStarted {
				if !pcmHasContent(data) {
					s.mu.Unlock()
					if err != nil {
						return
					}
					continue
				}
				s.mediaStarted = true
			}
			s.signalLocked()
			s.frames += int64(n / 4)
			s.clipped += s.equalizer.process(data)
			if s.volumeMode != "linked" {
				applyGain(data, &gain, s.targetGain)
			}
			for format, h := range s.hubs {
				if format == "wav" || format == "pcm" {
					h.Write(data)
				} else if !s.queues[format].Push(data) {
					s.err = "音频处理积压，部分帧被丢弃"
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			if err != io.EOF && s.ctx.Err() == nil {
				e.fail(s, "音频输入已中断")
			}
			return
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

// EOF precedes audible completion. Follow the playback clock and pauses
// rather than imposing a fixed seven-second gap after every track.
func (e *Engine) finishMedia(s *session) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		drain := liveReserve
		for _, speaker := range e.app.Config().RouteTargets(e.route) {
			drain = max(drain, time.Duration(speaker.DelayMS)*time.Millisecond+liveReserve)
		}
		s.mu.Lock()
		ready := s.state != "paused" && s.state != "error" && !s.playbackStart.IsZero() &&
			time.Since(s.playbackStart) >= time.Duration(float64(s.frames)/44100*float64(time.Second))+drain
		ready = ready && !s.desiredPaused
		for _, p := range s.pulls {
			// A renderer may close at the media boundary before our handler
			// observes EOF. Once the source clock has drained, a historical
			// incomplete request must not turn natural EOF into a failure.
			if p.Connections > 0 {
				ready = false
			}
		}
		timedOut := !s.desiredPaused && s.state != "paused" && !s.playbackStart.IsZero() && time.Since(s.playbackStart) > time.Duration(float64(s.frames)/44100*float64(time.Second))+drain+15*time.Second
		s.mu.Unlock()
		if ready {
			e.stop(s.id, true)
			return
		}
		if timedOut {
			e.fail(s, "音箱未完成音频读取")
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (e *Engine) fail(s *session, message string) {
	s.mu.Lock()
	if s.ctx.Err() != nil || s.state == "stopped" || s.state == "error" {
		s.mu.Unlock()
		return
	}
	s.err = message
	s.state = "error"
	s.endReason = "failed"
	s.finished = false
	s.mu.Unlock()
	e.clearPrefetch(s.identity)
	s.record("session_failed", "", 0)
	e.cancel(s)
	e.notifyChange()
	// fail may run while op is held by prepare. Cleanup is generation-scoped
	// and tracked, so it cannot stop a newer source or outlive Engine.Close.
	e.cleanupRoute(s)
	slog.Warn("播放链路异常", "protocol", s.protocol, "reason", message)
}

func (e *Engine) cleanupRoute(s *session) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.op.Lock()
		defer e.op.Unlock()
		ctx, cancel := context.WithTimeout(e.ctx, 8*time.Second)
		defer cancel()
		_, _ = e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "stop", 0)
	}()
}
func (e *Engine) token(s *session, id string) string {
	m := hmac.New(sha256.New, e.secret)
	m.Write([]byte(s.id + ":" + id))
	return hex.EncodeToString(m.Sum(nil))[:32]
}
func (e *Engine) url(s *session, sp store.Speaker) string {
	e.ensureHub(s, sp.Format)
	s.mu.Lock()
	revision := s.displayRev
	s.mu.Unlock()
	return e.base + "/stream/" + s.id + "/" + sp.Format + "/" + e.token(s, sp.ID) + fmt.Sprintf("?rev=%d", revision)
}
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "stream" || (r.Method != "GET" && r.Method != "HEAD") {
		http.NotFound(w, r)
		return
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || parts[1] != s.id || s.ctx.Err() != nil {
		http.NotFound(w, r)
		return
	}
	var speaker store.Speaker
	found := false
	for _, sp := range e.app.Config().RouteTargets(e.route) {
		if hmac.Equal([]byte(parts[3]), []byte(e.token(s, sp.ID))) && parts[2] == sp.Format {
			speaker = sp
			found = true
			break
		}
	}
	if !found || !e.app.RouteOwns(e.route, speaker.ID) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	h := s.hubs[parts[2]]
	s.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	// Audio clients often send Range: bytes=0- for live streams. Ignore it rather
	// than claiming a finite length or byte-seek support.
	mime := map[string]string{"mp3": "audio/mpeg", "flac": "audio/flac", "wav": "audio/wav", "pcm": "audio/wav"}
	w.Header().Set("Content-Type", mime[parts[2]])
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org", "DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
	if r.Method == "HEAD" {
		return
	}
	select {
	case <-h.ready:
	case <-s.ctx.Done():
		http.Error(w, "stream ended", 410)
		return
	case <-r.Context().Done():
		return
	case <-time.After(20 * time.Second):
		http.Error(w, "audio not ready", 503)
		return
	}
	controller := http.NewResponseController(w)
	controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if parts[2] == "wav" || parts[2] == "pcm" {
		if _, err := w.Write(wavHeader()); err != nil {
			return
		}
	} else if parts[2] == "flac" {
		h.mu.Lock()
		header := append([]byte(nil), h.header...)
		h.mu.Unlock()
		if len(header) > 0 {
			if _, err := w.Write(header); err != nil {
				return
			}
		}
	}
	s.mu.Lock()
	firstPull := s.mode == fileSource && s.playbackStart.IsZero()
	if firstPull {
		s.playbackStart = time.Now()
		s.pullOnce.Do(func() { close(s.pullReady) })
	}
	p := s.pulls[speaker.ID]
	if s.state == "waiting_for_speakers" {
		s.state = "playing"
		s.err = ""
	}
	p.Connections++
	p.Completed = false
	s.pulls[speaker.ID] = p
	s.mu.Unlock()
	if firstPull {
		s.record("stream_pulled", speaker.ID, 0)
	}
	drained := false
	defer func() {
		s.mu.Lock()
		p := s.pulls[speaker.ID]
		p.Connections--
		p.Completed = p.Completed || drained
		s.pulls[speaker.ID] = p
		s.mu.Unlock()
	}()
	cursor := uint64(0)
	if firstPull {
		h.mu.Lock()
		if len(h.items) > 0 {
			cursor = h.items[0].seq - 1
		}
		h.mu.Unlock()
	}
	delay := time.Duration(speaker.DelayMS) * time.Millisecond
	line := &delayLine{}
	ended := false
	lastWrite := time.Now()
	formatRecorded := false
	for {
		allowed := false
		for _, sp := range e.app.Config().RouteTargets(e.route) {
			if sp.ID == speaker.ID && sp.Format == parts[2] {
				allowed = true
				delay = time.Duration(sp.DelayMS) * time.Millisecond
				break
			}
		}
		if !allowed || s.ctx.Err() != nil || !e.app.RouteOwns(e.route, speaker.ID) {
			return
		}
		line.configure(h.byteRate(), delay)
		item, send := line.pop(false, ended)
		bridged := false
		if !send {
			if ended {
				drained = true
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), bridgeGap)
			next, ok, err := h.next(ctx, cursor, 0, firstPull)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				if r.Context().Err() != nil {
					return
				}
				item, send = line.pop(true, false)
				bridged = send
				// An overestimated reserve may cost latency, never real audio.
				if !send && time.Since(lastWrite) >= time.Second {
					item, send = line.pop(false, true)
				}
				if !send {
					continue
				}
			} else if err != nil {
				return
			} else if !ok {
				ended = true
				continue
			} else {
				skipped := uint64(0)
				if cursor != 0 && next.seq > cursor+1 {
					skipped = next.seq - cursor - 1
				}
				cursor = next.seq
				skipped += line.push(next)
				if skipped > 0 {
					s.mu.Lock()
					p := s.pulls[speaker.ID]
					p.HistorySkips += skipped
					s.pulls[speaker.ID] = p
					s.mu.Unlock()
					slog.Warn("音箱取流落后，跳过过期音频并保持连接", "speaker", speaker.ID, "skipped_chunks", skipped)
				}
				continue
			}
		}
		controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		n, err := w.Write(item.data)
		if err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		lastWrite = time.Now()
		if !formatRecorded && n > 0 {
			e.app.RecordFormat(s.account, speaker.ID, parts[2])
			formatRecorded = true
		}
		s.mu.Lock()
		p := s.pulls[speaker.ID]
		if bridged {
			p.Bridges++
		}
		if line.rate > 0 {
			p.BufferedMS = int(float64(line.bytes) / line.rate * 1000)
		}
		p.Bytes += int64(n)
		p.LastRead = lastWrite
		s.pulls[speaker.ID] = p
		s.mu.Unlock()
	}

}

// Changes signals lifecycle updates; observers read a fresh Status snapshot.
func (e *Engine) Changes() <-chan struct{} { return e.changes }
func (e *Engine) notifyChange() {
	select {
	case e.changes <- struct{}{}:
	default:
	}
}
func (e *Engine) Stop(owner string) { e.stopSession(owner, false, true) }
func (e *Engine) stop(owner string, finished bool) {
	e.stopSession(owner, finished, false)
}
func (e *Engine) stopSession(owner string, finished, logical bool) {
	e.mu.RLock()
	intent := e.current
	e.mu.RUnlock()
	if intent != nil && (owner == "" || owner == intent.id || (logical && owner == intent.identity)) {
		intent.mu.Lock()
		prepared := intent.prepared
		intent.mu.Unlock()
		if !finished || prepared {
			e.app.SetRouteIntent(e.route, "stream:"+intent.id, true)
		}
	}
	if !finished {
		if intent != nil && (owner == "" || owner == intent.id || (logical && owner == intent.identity)) {
			e.clearPrefetch(intent.identity)
		}
		e.mu.RLock()
		selected := e.current
		e.mu.RUnlock()
		if selected != nil && (owner == "" || owner == selected.id || (logical && owner == selected.identity)) {
			if owner == "" {
				owner = selected.identity
			}
			e.cancel(selected)
		}
		if selected == nil {
			return
		}
	}
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || (owner != "" && owner != s.id && (!logical || owner != s.identity)) {
		return
	}
	s.mu.Lock()
	if finished && (s.ctx.Err() != nil || s.state == "error" || s.state == "paused" || s.desiredPaused) {
		s.mu.Unlock()
		return
	}
	if s.state == "stopped" {
		if !finished {
			s.finished = false
			s.endReason = "stopped"
		}
		s.mu.Unlock()
		if !finished {
			e.notifyChange()
		}
		return
	}
	s.finished = finished && s.ctx.Err() == nil
	if s.finished {
		s.endReason = "media_finished"
	} else {
		s.endReason = "stopped"
	}
	s.mu.Unlock()
	e.cancel(s)
	ctx, cancel := context.WithTimeout(e.ctx, 8*time.Second)
	defer cancel()
	_, _ = e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "stop", 0)
	s.mu.Lock()
	s.state = "stopped"
	s.mu.Unlock()
	e.notifyChange()
}

// A nonempty owner scopes protocol commands to their logical cast. Web
// controls omit it and select the current cast atomically under op.
func (e *Engine) Pause(ctx context.Context, owner ...string) error {
	e.mu.RLock()
	selected := e.current
	e.mu.RUnlock()
	if selected == nil || !ownsSession(selected, owner) || selected.ctx.Err() != nil {
		return nil
	}
	selected.mu.Lock()
	selected.desiredPaused = true
	selected.signalLocked()
	selected.mu.Unlock()
	e.app.SetRouteIntent(e.route, "stream:"+selected.id, true)
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.closing.Load() {
		return errors.New("投送入口已关闭")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil {
		return nil
	}
	if !ownsSession(s, owner) || s.ctx.Err() != nil {
		return nil
	}
	if s != selected {
		return nil
	}
	results, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "pause", 0)
	if err = commandError(results, err); err != nil {
		s.mu.Lock()
		s.desiredPaused = false
		s.signalLocked()
		s.mu.Unlock()
		e.app.SetRouteIntent(e.route, "stream:"+s.id, false)
		return err
	}
	s.mu.Lock()
	if s.state != "paused" {
		s.pausedAt = time.Now()
	}
	s.state = "paused"
	s.signalLocked()
	s.mu.Unlock()
	return err
}
func (e *Engine) Resume(ctx context.Context, owner ...string) error {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.closing.Load() {
		return errors.New("投送入口已关闭")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil {
		return errors.New("没有可恢复的播放")
	}
	if !ownsSession(s, owner) {
		return nil
	}
	s.mu.Lock()
	s.desiredPaused = false
	s.signalLocked()
	published := s.published
	s.mu.Unlock()
	e.app.SetRouteIntent(e.route, "stream:"+s.id, false)
	if s.mode == fileSource {
		s.mu.Lock()
		track, pos, trackID := s.track, s.positionAt(time.Now()), s.trackID
		s.mu.Unlock()
		_, err := e.startLocked(startSpec{mode: fileSource, protocol: s.protocol, trackID: trackID, owner: s.identity, uri: s.mediaURL, track: track, position: pos, continuation: true})
		return err
	}
	if s.ctx.Err() != nil {
		return errors.New("播放会话已结束")
	}
	if !published {
		s.mu.Lock()
		s.state = "transitioning"
		s.signalLocked()
		s.mu.Unlock()
		return nil
	}
	results, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "play", 0)
	if err = commandError(results, err); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	s.mu.Lock()
	s.state = "playing"
	s.signalLocked()
	s.mu.Unlock()
	return err
}
func (e *Engine) Metadata(owner string, track Track) {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.closing.Load() {
		return
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.identity != owner || s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	same := s.track.Title == track.Title && s.track.Artist == track.Artist
	s.track = track
	if same {
		s.mu.Unlock()
		return
	}
	s.metadataSeq++
	seq := s.metadataSeq
	if s.metadataCancel != nil {
		s.metadataCancel()
	}
	metadataCtx, cancelMetadata := context.WithCancel(s.ctx)
	s.metadataCancel = cancelMetadata
	s.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancelMetadata()
		id := e.match(metadataCtx, track)
		if id == "" {
			return
		}
		e.op.Lock()
		defer e.op.Unlock()
		if !e.isCurrent(s) || s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		valid := s.metadataSeq == seq && s.state == "playing" && !s.desiredPaused && !s.sourceEnded && (s.protocol != "airplay" || time.Since(s.lastInput) < time.Second)
		if valid {
			s.displayRev++
		}
		s.mu.Unlock()
		if valid {
			ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
			defer cancel()
			_ = e.app.RefreshRouteDisplay(ctx, e.route, id, "stream:"+s.id)
		}
	}()
}
func (e *Engine) Close() {
	e.closing.Store(true)
	e.clearPrefetch("")
	e.mu.RLock()
	selected := e.current
	e.mu.RUnlock()
	if selected != nil {
		e.cancel(selected)
		e.app.SetRouteIntent(e.route, "stream:"+selected.id, true)
	}
	e.op.Lock()
	e.closed = true
	e.op.Unlock()
	e.Stop("")
	e.wg.Wait()
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if e.cachePath != "" {
		os.Remove(e.cachePath)
	}
}
func (e *Engine) RuntimePath() string { return filepath.Base(e.binary) }
func (e *Engine) Seek(seconds float64, owner ...string) error {
	if seconds < 0 || seconds > 24*3600 {
		return errors.New("无效播放位置")
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.closing.Load() {
		return errors.New("投送入口已关闭")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.mode != fileSource {
		return errors.New("当前音源不支持定位")
	}
	if !ownsSession(s, owner) {
		return nil
	}
	s.mu.Lock()
	track, trackID := s.track, s.trackID
	s.mu.Unlock()
	_, err := e.startLocked(startSpec{mode: fileSource, protocol: s.protocol, trackID: trackID, owner: s.identity, uri: s.mediaURL, track: track, position: seconds, continuation: true})
	return err
}

func ownsSession(s *session, owner []string) bool {
	return len(owner) == 0 || (owner[0] != "" && owner[0] == s.identity)
}

func commandError(results map[string]string, err error) error {
	if err != nil {
		return err
	}
	var failures []error
	for _, result := range results {
		if result == "accepted" {
			return nil
		}
		failures = append(failures, errors.New(result))
	}
	return errors.Join(failures...)
}
func (e *Engine) String() string { return fmt.Sprintf("audio runtime: %s", filepath.Base(e.binary)) }
