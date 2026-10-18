package playback

import (
	"context"
	"micastgo/internal/testsupport"
	"testing"
	"time"
)

func TestPlayingActivityIsNotGatedByTheDLNAClock(t *testing.T) {
	e := lifecycleEngine(t)
	airplay := e.newSession("airplay", Track{Title: "airplay"}, 0)
	airplay.mu.Lock()
	airplay.awaitMedia = true
	airplay.mediaStarted = true
	airplay.state = "playing"
	airplay.mu.Unlock()
	if got := e.Status().Activity; got != "playing" {
		t.Fatal("AirPlay playback reported as preparing", got)
	}
	dlna := e.newSession("dlna", Track{Title: "dlna"}, 0)
	dlna.mu.Lock()
	dlna.state = "playing"
	dlna.mu.Unlock()
	if got := e.Status().Activity; got != "transitioning" {
		t.Fatal("DLNA must wait for the speaker pull", got)
	}
}

func TestAirPlayWaitsForMediaAndKeepsPlayingThroughSilence(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	e := &Engine{ctx: context.Background(), app: a, changes: make(chan struct{}, 1)}
	t.Cleanup(e.Close)
	q := NewPCMQueue(e.ctx)
	owner, err := e.StartPCMFor("", q, Track{})
	if err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	select {
	case <-cloud.Plays:
		t.Fatal("idle transport started speaker")
	case <-time.After(150 * time.Millisecond):
	}
	if s := e.Status(); s.Activity != "connected" || s.Position != 0 || s.Error != "" {
		t.Fatal(s)
	}
	data := make([]byte, 4096)
	data[0] = 32
	q.Push(data)
	select {
	case <-cloud.Plays:
	case <-time.After(time.Second):
		t.Fatal("media did not start speaker")
	}
	e.op.Lock()
	s := e.current
	s.mu.Lock()
	s.playbackStart = time.Now()
	frames := s.frames
	s.mu.Unlock()
	e.op.Unlock()
	if e.Status().Activity != "playing" {
		t.Fatal(e.Status())
	}
	if err := e.Volume(context.Background(), 0, owner); err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		advanced := s.frames > frames
		s.mu.Unlock()
		if advanced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("silence was discarded after media started")
		}
		time.Sleep(time.Millisecond)
	}
	if e.Status().Activity != "playing" {
		t.Fatal(e.Status())
	}
}
