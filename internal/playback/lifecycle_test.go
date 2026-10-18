package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"micastgo/internal/testsupport"
)

func lifecycleEngine(t *testing.T) *Engine {
	t.Helper()
	a, _ := testsupport.NewApp(t, "wav")
	e := &Engine{ctx: context.Background(), app: a, cacheDir: t.TempDir(), changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	return e
}

func TestResumeAndSeekPreserveCastButRevokeOldGeneration(t *testing.T) {
	e := lifecycleEngine(t)
	first := e.newSession("dlna", Track{Title: "first"}, 0)
	first.mediaURL = "http://127.0.0.1:1/first.wav"
	first.state = "paused"
	// The invalid local origin is rejected without external network traffic.
	if err := e.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.op.Lock()
	second := e.current
	e.op.Unlock()
	if second == first || second.id == first.id || second.identity != first.identity || first.ctx.Err() == nil {
		t.Fatal("resume did not preserve cast and retire the old media generation")
	}
	if err := e.Seek(2, first.identity); err != nil {
		t.Fatal(err)
	}
	e.op.Lock()
	third := e.current
	e.op.Unlock()
	if third.id == second.id || third.identity != first.identity || third.position != 2 || third.mediaURL != first.mediaURL {
		t.Fatal("seek lost the cast or selected a different source")
	}
	// Completion from a retired decoder must not end its replacement, even
	// when the retired generation's id equals the logical cast identity.
	e.stop(first.id, true)
	if e.current != third || e.Status().Finished {
		t.Fatal("retired generation completed the active cast")
	}
}

func TestSupersededProtocolControlsCannotChangeCurrentCast(t *testing.T) {
	e := lifecycleEngine(t)
	old := e.newSession("airplay", Track{}, 0)
	current := e.newSession("dlna", Track{Title: "current"}, 0)
	current.state = "playing"
	ctx := context.Background()
	for _, err := range []error{e.Pause(ctx, old.identity), e.Resume(ctx, old.identity), e.Seek(10, old.identity), e.Volume(ctx, 80, old.identity)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.Stop(old.identity)
	e.Metadata(old.identity, Track{Title: "stale"})
	if _, err := e.StartPCMFor(old.identity, NewPCMQueue(ctx), Track{}); err == nil {
		t.Fatal("old transport restarted over newer media")
	}
	if e.current != current || e.Status().State != "playing" || e.Status().Track.Title != "current" || e.requestedVolume != nil {
		t.Fatal("old protocol changed a newer cast", e.Status())
	}
}

func TestFailureCancelsWorkersAndCannotBecomeNaturalCompletion(t *testing.T) {
	e := lifecycleEngine(t)
	s := e.newSession("dlna", Track{}, 0)
	reader, writer := io.Pipe()
	s.source = reader
	t.Cleanup(func() { writer.Close() })
	done := make(chan struct{})
	go func() { defer close(done); e.consume(s, reader) }()
	e.fail(s, "decoder failed")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed source worker remained blocked")
	}
	e.stop(s.id, true)
	if status := e.Status(); status.State != "error" || status.Finished || s.ctx.Err() == nil {
		t.Fatal("failure became natural completion", status)
	}
	select {
	case <-e.Changes():
	default:
		t.Fatal("missing lifecycle notification")
	}
}

func TestClosedEngineRejectsResumeAndSeek(t *testing.T) {
	e := lifecycleEngine(t)
	s := e.newSession("dlna", Track{}, 0)
	e.Close()
	if e.Resume(context.Background()) == nil || e.Seek(0) == nil || e.current != s {
		t.Fatal("closed engine accepted new media work")
	}
}

func TestPCMInitialVolumeBelongsToNewCast(t *testing.T) {
	e := lifecycleEngine(t)
	q := NewPCMQueue(e.ctx)
	id, err := e.StartPCM(q, Track{}, 23)
	if err != nil {
		t.Fatal(err)
	}
	if e.requestedVolume == nil || *e.requestedVolume != 23 {
		t.Fatal("initial AirPlay volume was lost before RECORD")
	}
	if err := e.Volume(context.Background(), 90, "old-connection"); err != nil {
		t.Fatal(err)
	}
	if *e.requestedVolume != 23 || e.Status().SessionID != id {
		t.Fatal("old connection changed initial volume")
	}
}

func TestAutomaticAdvanceRequiresCurrentNaturalCompletion(t *testing.T) {
	e := lifecycleEngine(t)
	first := e.newSession("dlna", Track{}, 0)
	raw := "http://127.0.0.1:1/next.wav"
	if _, err := e.AdvanceMedia(first.identity, raw, Track{}); err == nil {
		t.Fatal("unfinished media advanced")
	}
	e.stop(first.id, true)
	previous := e.Status().PipelineID
	id, err := e.AdvanceMedia(first.identity, raw, Track{Title: "next"})
	status := e.Status()
	if err != nil || id != first.identity || status.PipelineID == previous || status.Track.Title != "next" {
		t.Fatal("natural completion did not advance", id, err, status)
	}
	newCast := e.newSession("airplay", Track{}, 0)
	if _, err := e.AdvanceMedia(first.identity, raw, Track{}); err == nil || e.current != newCast {
		t.Fatal("delayed auto advance replaced newer AirPlay cast")
	}
}

func TestFiniteMediaAudioAndAutomaticAdvanceThroughHandler(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires FFmpeg")
	}
	a, cloud := testsupport.NewApp(t, "wav")
	e, err := New(context.Background(), a, ffmpeg, "http://renderer", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	pcm := make([]byte, 4410*4)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	header := wavHeader()
	binary.LittleEndian.PutUint32(header[4:], uint32(len(pcm)+36))
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	e.cacheURL = "http://192.168.1.20/cached.wav"
	e.cachePath = filepath.Join(t.TempDir(), "cached.wav")
	if err := os.WriteFile(e.cachePath, append(header, pcm...), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := e.PlayMedia(e.cacheURL, Track{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for track := 0; track < 2; track++ {
		var play testsupport.Play
		select {
		case play = <-cloud.Plays:
		case <-time.After(5 * time.Second):
			t.Fatal("no speaker playback command", e.Status())
		}
		// Exercise real decoding, output buffering and the HTTP handler without
		// relying on the test environment permitting loopback socket traffic.
		w := httptest.NewRecorder()
		if track == 0 {
			e.ServeHTTP(&delayedAudioRecorder{ResponseRecorder: w, delay: 120 * time.Millisecond}, httptest.NewRequest("GET", play.URL, nil))
		} else {
			e.ServeHTTP(w, httptest.NewRequest("GET", play.URL, nil))
		}
		if w.Code != 200 || w.Body.Len() != len(pcm)+44 || !bytes.Equal(w.Body.Bytes()[44:], pcm) {
			t.Fatal("audio beginning or tail changed", track, w.Code, w.Body.Len())
		}
		select {
		case <-e.Changes():
		case <-time.After(3 * time.Second):
			t.Fatal("no natural completion notification", e.Status())
		}
		if !e.Status().Finished {
			t.Fatal("decoder completion was not marked natural", e.Status())
		}
		if track == 0 {
			previous := e.Status().PipelineID
			id, err = e.AdvanceMedia(id, e.cacheURL, Track{})
			if err != nil || e.Status().PipelineID == previous {
				t.Fatal("next track did not start", id, err)
			}
		}
	}
}

type delayedAudioRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (w *delayedAudioRecorder) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseRecorder.Write(p)
}

func TestStopPreemptsPendingSpeakerStart(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	entered := make(chan struct{})
	cloud.BeforePlay = func(ctx context.Context, _ testsupport.Play) error { close(entered); <-ctx.Done(); return ctx.Err() }
	e := &Engine{ctx: context.Background(), app: a, changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	q := NewPCMQueue(e.ctx)
	_, err := e.StartPCM(q, Track{})
	if err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no pending playback command")
	}
	done := make(chan struct{})
	go func() { defer close(done); e.Stop("") }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop waited behind cloud startup")
	}
	if e.Status().State != "stopped" {
		t.Fatal(e.Status())
	}
}

func TestStopPreemptsPendingResume(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	e := &Engine{ctx: context.Background(), app: a, changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	q := NewPCMQueue(e.ctx)
	_, err := e.StartPCM(q, Track{})
	if err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case <-cloud.Plays:
	case <-time.After(2 * time.Second):
		t.Fatal("no playback")
	}
	if err := e.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	cloud.BeforeCommand = func(ctx context.Context, _ string, method string, args map[string]any) error {
		if method == "player_play_operation" && args["action"] == "play" {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	resumed := make(chan struct{})
	go func() { defer close(resumed); _ = e.Resume(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no pending resume")
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); e.Stop("") }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop waited behind resume")
	}
	<-resumed
	if e.Status().State != "stopped" {
		t.Fatal(e.Status())
	}
}

func TestPauseBeforeSourceReadyDoesNotWakeSpeaker(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	e := &Engine{ctx: context.Background(), app: a, changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	q := NewPCMQueue(e.ctx)
	_, err := e.StartPCM(q, Track{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case play := <-cloud.Plays:
		t.Fatal("paused preparation woke the speaker", play)
	case <-time.After(250 * time.Millisecond):
	}
	if e.Status().State != "paused" {
		t.Fatal(e.Status())
	}
	if err := e.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case <-cloud.Plays:
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not start prepared source", e.Status())
	}
}

func TestLastTargetTakeoverRetiresOldPipeline(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	e := &Engine{ctx: context.Background(), app: a, route: "speaker:speaker", changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	q := NewPCMQueue(e.ctx)
	_, err := e.StartPCM(q, Track{})
	if err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case <-cloud.Plays:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial source")
	}
	_, err = a.ControlRoute(context.Background(), "whole_house", "play_url", "http://new-source", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if status := e.Status(); status.State != "stopped" || status.EndReason != "targets_lost" || status.Finished {
		t.Fatal("old pipeline survived takeover", status)
	}
	if q.Push(make([]byte, 4)) {
		t.Fatal("retired input still accepted PCM")
	}
}
