package playback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrSuperseded means a newer cast was accepted before this command ran.
var ErrSuperseded = errors.New("投送已被更新的命令替代")

// StartRequest is the protocol-neutral way to begin a cast. Seq comes from
// Engine.Sequence and orders starts across every protocol sharing this entry.
type StartRequest struct {
	Seq          uint64
	Protocol     string
	TrackID      string
	Owner        string
	URI          string
	Track        Track
	Position     float64
	Continuation bool
	AwaitMedia   bool
}

// Receipt identifies the logical cast and its first pipeline.
type Receipt struct {
	Identity   string
	PipelineID string
	TrackID    string
}

// Sequence allocates a process-wide command order for one receiver entry.
func (e *Engine) Sequence() uint64 { return e.sequence.Add(1) }

// acceptStart lets only a start at least as new as the last accepted one run.
// A stale queued start from another protocol can never take the speakers back.
func (e *Engine) acceptStart(seq uint64) bool {
	if seq == 0 {
		return true
	}
	for {
		last := e.lastStart.Load()
		if seq < last {
			return false
		}
		if e.lastStart.CompareAndSwap(last, seq) {
			return true
		}
	}
}

type startSpec struct {
	mode          sourceMode
	protocol      string
	trackID       string
	owner         string
	uri           string
	track         Track
	position      float64
	continuation  bool
	input         io.ReadCloser
	awaitMedia    bool
	initialVolume []int
	seq           uint64
}

func (e *Engine) StartFile(req StartRequest) (Receipt, error) {
	e.op.Lock()
	defer e.op.Unlock()
	if req.Protocol == "" {
		req.Protocol = "dlna"
	}
	return e.startLocked(startSpec{mode: fileSource, protocol: req.Protocol, trackID: req.TrackID, owner: req.Owner, uri: req.URI, track: req.Track, position: req.Position, continuation: req.Continuation, seq: req.Seq})
}

func (e *Engine) StartStream(req StartRequest, input io.ReadCloser, initialVolume ...int) (Receipt, error) {
	e.op.Lock()
	defer e.op.Unlock()
	if req.Protocol == "" {
		req.Protocol = "airplay"
	}
	receipt, err := e.startLocked(startSpec{mode: streamSource, protocol: req.Protocol, trackID: req.TrackID, owner: req.Owner, track: req.Track, input: input, awaitMedia: req.AwaitMedia, initialVolume: initialVolume, seq: req.Seq})
	if err != nil {
		input.Close()
	}
	return receipt, err
}

// Preempt stops the current pipeline without waiting on the cloud. Callers use
// it when a controller replaces media; the following Play establishes the new
// source, so a slow stop must never delay the protocol response.
func (e *Engine) Preempt(owner string) {
	if owner == "" {
		return
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.identity != owner {
		return
	}
	s.mu.Lock()
	if s.state == "stopped" {
		s.mu.Unlock()
		return
	}
	s.state, s.endReason, s.finished = "stopped", "preempted", false
	s.signalLocked()
	s.mu.Unlock()
	e.clearPrefetch(owner)
	e.cancel(s)
	e.stopRouteSource(s)
	e.notifyChange()
}

// stopRouteSource tells the cloud to stop this pipeline without taking the
// operation lock, so a new cast never waits behind a slow stop.
func (e *Engine) stopRouteSource(s *session) {
	if e.app == nil || e.closing.Load() {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
		defer cancel()
		_, _ = e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "stop", 0)
	}()
}

// Caller holds op.
func (e *Engine) startLocked(spec startSpec) (Receipt, error) {
	if spec.mode == fileSource {
		if !validMediaURL(spec.uri) || spec.position < 0 || spec.position > 86400 {
			return Receipt{}, errors.New("无效媒体地址或播放位置")
		}
	}
	if e.closed || e.closing.Load() {
		return Receipt{}, errors.New("投送入口已关闭")
	}
	if spec.owner != "" {
		e.mu.RLock()
		current := e.current
		e.mu.RUnlock()
		if current == nil || current.identity != spec.owner {
			return Receipt{}, errors.New("播放会话已被新的投放替代")
		}
	}
	if spec.seq != 0 && !e.acceptStart(spec.seq) {
		return Receipt{}, ErrSuperseded
	}
	var prefetched *prefetch
	if spec.mode == fileSource {
		prefetched = e.takePrefetch(spec.uri)
	}
	s := e.newSessionFor(spec.mode, spec.protocol, spec.trackID, spec.track, spec.position, spec.owner)
	s.mu.Lock()
	s.prefetched = prefetched
	s.mediaURL = spec.uri
	s.continuation = spec.continuation
	if spec.mode == streamSource {
		s.source = spec.input
		s.awaitMedia = spec.awaitMedia
	}
	s.mu.Unlock()
	if len(spec.initialVolume) > 0 {
		e.volumeMu.Lock()
		e.requestedVolume = new(int)
		*e.requestedVolume = max(0, min(100, spec.initialVolume[0]))
		e.requestedMuted = false
		v := *e.requestedVolume
		e.volumeMu.Unlock()
		s.mu.Lock()
		s.targetGain = volumeGain(v)
		s.hardwareVolume = &v
		s.mu.Unlock()
	}
	if spec.mode == streamSource {
		e.prepare(s)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.consume(s, spec.input)
		}()
	} else {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.decodeFile(s)
		}()
	}
	return Receipt{Identity: s.identity, PipelineID: s.id, TrackID: s.trackID}, nil
}

func (e *Engine) decodeFile(s *session) {
	s.mu.Lock()
	raw, position := s.mediaURL, s.position
	s.mu.Unlock()
	path, err := e.download(s, raw)
	if err != nil {
		if s.ctx.Err() == nil {
			e.fail(s, "媒体下载失败，请检查来源地址和文件大小")
		}
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	s.record("media_ready", "", 0)
	s.mu.Lock()
	needProbe := s.track.Duration <= 0
	s.mu.Unlock()
	if needProbe {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.probeDuration(s, path)
		}()
	}
	reader, writer := io.Pipe()
	s.mu.Lock()
	s.source = reader
	s.mu.Unlock()
	defer reader.Close()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-protocol_whitelist", "file,pipe", "-format_whitelist", "mp3,aac,flac,wav,mov,mp4,m4a,3gp,3g2,mj2,ogg"}
	if position > 0 {
		args = append(args, "-ss", strconv.FormatFloat(position, 'f', 3, 64))
	}
	args = append(args, "-i", path)
	args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-ar", "44100", "-ac", "2", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
	command := exec.CommandContext(s.ctx, e.binary, args...)
	command.Stdout = writer
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	if err = command.Start(); err != nil {
		writer.CloseWithError(err)
		e.fail(s, "无法启动媒体解码")
		return
	}
	e.prepare(s)
	done := make(chan struct{})
	go func() { defer close(done); e.consume(s, reader) }()
	err = command.Wait()
	if err != nil && s.ctx.Err() == nil {
		e.fail(s, "媒体解码失败或格式不支持")
	}
	writer.Close()
	<-done
}

// probeDuration reads the container header through the bundled FFmpeg. The
// build disables ffprobe, and this path does not decode audio.
func (e *Engine) probeDuration(s *session, path string) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	duration := probeFileDuration(ctx, e.binary, path)
	if duration <= 0 || !e.isCurrent(s) || s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.track.Duration <= 0 && s.state != "stopped" && s.state != "error" {
		s.track.Duration = duration
	}
	s.mu.Unlock()
	s.record("duration_probed", "", 0)
}

type limitedWriter struct {
	bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.Buffer.Len() >= w.limit {
		return len(p), nil
	}
	room := w.limit - w.Buffer.Len()
	if len(p) > room {
		w.Buffer.Write(p[:room])
		return len(p), nil
	}
	return w.Buffer.Write(p)
}

func probeFileDuration(ctx context.Context, binary, path string) float64 {
	log := &limitedWriter{limit: 64 * 1024}
	command := exec.CommandContext(ctx, binary, "-hide_banner", "-nostdin", "-i", path)
	command.Stdout = io.Discard
	command.Stderr = log
	_ = command.Run()
	return parseFFmpegDuration(log.String())
}

func parseFFmpegDuration(output string) float64 {
	for _, line := range strings.Split(output, "\n") {
		_, rest, ok := strings.Cut(line, "Duration:")
		if !ok {
			continue
		}
		value := strings.TrimSpace(strings.SplitN(rest, ",", 2)[0])
		if value == "" || strings.HasPrefix(value, "N/A") {
			return 0
		}
		parts := strings.Split(value, ":")
		if len(parts) != 3 {
			return 0
		}
		hours, err1 := strconv.Atoi(parts[0])
		minutes, err2 := strconv.Atoi(parts[1])
		seconds, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil || hours < 0 || minutes < 0 || minutes > 59 || seconds < 0 || seconds >= 60 {
			return 0
		}
		return float64(hours*3600+minutes*60) + seconds
	}
	return 0
}
