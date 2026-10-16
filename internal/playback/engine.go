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
	"time"
)

type Track struct {
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Duration float64 `json:"duration"`
}
type Pull struct {
	Bridges      uint64    `json:"bridges"`
	BufferedMS   int       `json:"buffered_ms"`
	HistorySkips uint64    `json:"history_skips"`
	Retries      int       `json:"retries"`
	Connections  int       `json:"connections"`
	Bytes        int64     `json:"bytes"`
	LastRead     time.Time `json:"last_read"`
}
type Status struct {
	InputDroppedBytes    int64           `json:"input_dropped_bytes"`
	EncodingDroppedBytes int64           `json:"encoding_dropped_bytes"`
	Finished             bool            `json:"finished"`
	SessionID            string          `json:"session_id,omitempty"`
	Protocol             string          `json:"protocol"`
	State                string          `json:"state"`
	Track                Track           `json:"track"`
	Position             float64         `json:"position"`
	Error                string          `json:"error,omitempty"`
	Streams              map[string]Pull `json:"streams"`
	Format               string          `json:"format"`
}
type session struct {
	continuation  bool
	finished      bool
	id            string
	ctx           context.Context
	cancel        context.CancelFunc
	source        io.ReadCloser
	mu            sync.Mutex
	hubs          map[string]*hub
	queues        map[string]*PCMQueue
	pulls         map[string]Pull
	protocol      string
	track         Track
	state         string
	err           string
	started       time.Time
	position      float64
	frames        int64
	metadataSeq   uint64
	pullReady     chan struct{}
	pullOnce      sync.Once
	playbackStart time.Time
	displayRev    uint64
	sourceEnded   bool
	lastInput     time.Time
	pausedAt      time.Time
}
type Engine struct {
	requestedVolume        *int // Protected by op; remembered before playback starts.
	closed                 bool // Protected by op.
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
	wg                     sync.WaitGroup
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
	return &Engine{route: routeID, ctx: ctx, app: a, binary: binary, base: strings.TrimRight(base, "/"), cacheDir: dir, secret: secret}, nil
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
	out.Finished = s.finished
	out.Protocol = s.protocol
	out.SessionID = s.id
	out.State = s.state
	out.Track = s.track
	out.Error = s.err
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
func (e *Engine) newSession(protocol string, track Track, position float64) *session {
	b := make([]byte, 16)
	rand.Read(b)
	ctx, cancel := context.WithCancel(e.ctx)
	s := &session{id: hex.EncodeToString(b), ctx: ctx, cancel: cancel, hubs: map[string]*hub{}, queues: map[string]*PCMQueue{}, pulls: map[string]Pull{}, protocol: protocol, track: track, state: "transitioning", started: time.Now(), position: position, pullReady: make(chan struct{})}
	e.mu.Lock()
	old := e.current
	e.current = s
	e.mu.Unlock()
	if old != nil {
		e.cancel(old)
	}
	return s
}
func (e *Engine) cancel(s *session) {
	s.cancel()
	s.mu.Lock()
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
func (e *Engine) StartPCM(input io.ReadCloser, track Track) (string, error) {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		input.Close()
		return "", errors.New("投送入口已关闭")
	}
	s := e.newSession("airplay", track, 0)
	s.source = input
	e.prepare(s)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.consume(s, input) }()
	return s.id, nil
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
		select {
		case <-s.ctx.Done():
			return
		case <-h.ready:
		case <-time.After(20 * time.Second):
			e.fail(s, "音频源未产生可播放数据")
			return
		}
		e.op.Lock()
		defer e.op.Unlock()
		if !e.isCurrent(s) || s.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		s.mu.Lock()
		track := s.track
		s.mu.Unlock()
		audioID := e.match(ctx, track)
		results, err := e.app.BeginRouteStream(ctx, e.route, "stream:"+s.id, audioID, func(sp store.Speaker) string { return e.url(s, sp) }, s.continuation)
		if err != nil {
			e.fail(s, err.Error())
			return
		}
		if e.requestedVolume != nil {
			_, _ = e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "volume", *e.requestedVolume)
		}
		accepted := false
		for _, r := range results {
			if r == "accepted" {
				accepted = true
			}
		}
		s.mu.Lock()
		if s.state == "transitioning" {
			if accepted {
				s.state = "playing"
			} else {
				s.err = "音箱尚未接受播放指令"
				s.state = "waiting_for_speakers"
			}
		}
		s.mu.Unlock()
	}()
}
func (e *Engine) watch(s *session) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	attempts := map[string]int{}
	last := map[string]time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			state := s.state
			ended := s.sourceEnded
			pulls := map[string]Pull{}
			for id, p := range s.pulls {
				pulls[id] = p
			}
			started := s.started
			lastInput := s.lastInput
			hubs := make(map[string]*hub, len(s.hubs))
			for format, h := range s.hubs {
				hubs[format] = h
			}
			s.mu.Unlock()
			if ended || (state != "playing" && state != "waiting_for_speakers") || time.Since(started) < 6*time.Second {
				continue
			}
			ids := map[string]bool{}
			for _, target := range e.app.Config().RouteTargets(e.route) {
				p := pulls[target.ID]
				if time.Since(p.LastRead) < 5*time.Second {
					attempts[target.ID] = 0
					continue
				}
				// A source/encoder hole is not a failed speaker. Reissuing the
				// cloud play command during the hole causes a reconnect/refill cycle.
				if !retryHealthySource(time.Now(), lastInput, hubs[target.Format]) {
					continue
				}
				if attempts[target.ID] >= 3 || time.Since(last[target.ID]) < time.Duration(3*(1<<attempts[target.ID]))*time.Second {
					continue
				}
				ids[target.ID] = true
				s.mu.Lock()
				p = s.pulls[target.ID]
				p.Retries++
				s.pulls[target.ID] = p
				s.mu.Unlock()
				slog.Warn("音箱取流停滞，重试播放", "speaker", target.ID, "retry", p.Retries)
				attempts[target.ID]++
				last[target.ID] = time.Now()
			}
			if len(ids) > 0 {
				e.op.Lock()
				if e.isCurrent(s) && s.ctx.Err() == nil {
					ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
					_ = e.app.RetryRouteTargets(ctx, e.route, "stream:"+s.id, ids)
					cancel()
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
			if s.protocol == "dlna" {
				s.mu.Lock()
				waiting := s.playbackStart.IsZero()
				s.mu.Unlock()
				if waiting {
					select {
					case <-s.ctx.Done():
						return
					case <-s.pullReady:
					case <-time.After(25 * time.Second):
						return
					}
				}
			}
			timer := time.NewTimer(7 * time.Second)
			defer timer.Stop()
			select {
			case <-s.ctx.Done():
			case <-timer.C:
				e.stop(s.id, true)
			}
		}
	}()
	buffer := make([]byte, 4096)
	for {
		if s.protocol == "dlna" {
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
			paused := s.state == "paused" && s.protocol == "dlna"
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
		if n > 0 {
			s.mu.Lock()
			s.lastInput = time.Now()
			s.frames += int64(n / 4)
			for format, h := range s.hubs {
				if format == "wav" || format == "pcm" {
					h.Write(buffer[:n])
				} else if !s.queues[format].Push(buffer[:n]) {
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
func (e *Engine) fail(s *session, message string) {
	s.mu.Lock()
	s.err = message
	s.state = "error"
	s.mu.Unlock()
	slog.Warn("播放链路异常", "protocol", s.protocol, "reason", message)
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
	firstPull := s.protocol == "dlna" && s.playbackStart.IsZero()
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
	s.pulls[speaker.ID] = p
	s.mu.Unlock()
	defer func() { s.mu.Lock(); p := s.pulls[speaker.ID]; p.Connections--; s.pulls[speaker.ID] = p; s.mu.Unlock() }()
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
func (e *Engine) Stop(owner string) { e.stop(owner, false) }
func (e *Engine) stop(owner string, finished bool) {
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || (owner != "" && owner != s.id) {
		return
	}
	s.mu.Lock()
	if s.state == "stopped" {
		if !finished {
			s.finished = false
		}
		s.mu.Unlock()
		return
	}
	s.finished = finished && s.ctx.Err() == nil
	s.mu.Unlock()
	e.cancel(s)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, _ = e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "stop", 0)
	s.mu.Lock()
	s.state = "stopped"
	s.mu.Unlock()
}
func (e *Engine) Pause(ctx context.Context) error {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return errors.New("投送入口已关闭")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil {
		return nil
	}
	_, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "pause", 0)
	s.mu.Lock()
	if s.state != "paused" {
		s.pausedAt = time.Now()
	}
	s.state = "paused"
	s.mu.Unlock()
	return err
}
func (e *Engine) Resume(ctx context.Context) error {
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil {
		return errors.New("没有可恢复的播放")
	}
	if s.protocol == "dlna" {
		s.mu.Lock()
		track, pos := s.track, s.positionAt(time.Now())
		s.mu.Unlock()
		e.cacheMu.Lock()
		raw := e.cacheURL
		e.cacheMu.Unlock()
		_, err := e.PlayMedia(raw, track, pos, true)
		return err
	}
	e.op.Lock()
	defer e.op.Unlock()
	_, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "play", 0)
	s.mu.Lock()
	s.state = "playing"
	s.mu.Unlock()
	return err
}
func (e *Engine) Volume(ctx context.Context, value int) error {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return errors.New("投送入口已关闭")
	}
	e.requestedVolume = new(int)
	*e.requestedVolume = value
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil {
		return nil
	}
	_, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "volume", value)
	return err
}
func (e *Engine) Metadata(owner string, track Track) {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.id != owner {
		return
	}
	s.mu.Lock()
	if s.track.Title == track.Title && s.track.Artist == track.Artist {
		s.mu.Unlock()
		return
	}
	s.track = track
	s.metadataSeq++
	seq := s.metadataSeq
	s.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		id := e.match(s.ctx, track)
		if id == "" {
			return
		}
		e.op.Lock()
		defer e.op.Unlock()
		if !e.isCurrent(s) || s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		valid := s.metadataSeq == seq && s.state == "playing"
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
func (e *Engine) Seek(seconds float64) error {
	if seconds < 0 || seconds > 24*3600 {
		return errors.New("无效播放位置")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.protocol != "dlna" {
		return errors.New("当前音源不支持定位")
	}
	s.mu.Lock()
	track := s.track
	s.mu.Unlock()
	e.cacheMu.Lock()
	raw := e.cacheURL
	e.cacheMu.Unlock()
	_, err := e.PlayMedia(raw, track, seconds, true)
	return err
}
func (e *Engine) String() string { return fmt.Sprintf("audio runtime: %s", filepath.Base(e.binary)) }
