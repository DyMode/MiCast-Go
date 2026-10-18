package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
)

type observingCloud struct {
	*fakeCloud
	reads atomic.Int32
	read  func(context.Context) (xiaomi.PlayerStatus, error)
}

func (f *observingCloud) PlayerStatus(ctx context.Context, _ xiaomi.Tokens, _ string) (xiaomi.PlayerStatus, error) {
	f.reads.Add(1)
	return f.read(ctx)
}

func TestObservationIsActiveOnlyAndDoesNotOverwriteSavedVolume(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	if err := a.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	reader := &observingCloud{fakeCloud: f, read: func(context.Context) (xiaomi.PlayerStatus, error) {
		v := 71
		return xiaomi.PlayerStatus{Volume: &v, State: "paused"}, nil
	}}
	a.cloud = reader
	a.ObserveRoute(context.Background(), "speaker:one", "")
	if reader.reads.Load() != 0 {
		t.Fatal("idle status query")
	}
	saved := a.Config().Speakers["one"].Volume
	if _, err := a.BeginRouteStream(context.Background(), "speaker:one", "cast", "", func(store.Speaker) string { return "http://audio" }); err != nil {
		t.Fatal(err)
	}
	a.ObserveRoute(context.Background(), "speaker:one", "cast")
	devices := a.Snapshot().Devices
	if len(devices) != 1 || devices[0].Observed == nil || *devices[0].Observed.Volume != 71 || devices[0].Observed.State != "paused" {
		t.Fatal(devices)
	}
	if a.Config().Speakers["one"].Volume != saved {
		t.Fatal("readback overwrote baseline")
	}
	if !a.routes["speaker:one"].runnable() {
		t.Fatal("observed pause changed playback intent")
	}
}

func TestLateObservationCannotCommitAfterTakeover(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	_ = a.Refresh(context.Background(), true)
	entered, release := make(chan struct{}), make(chan struct{})
	a.cloud = &observingCloud{fakeCloud: f, read: func(context.Context) (xiaomi.PlayerStatus, error) {
		close(entered)
		<-release
		v := 90
		return xiaomi.PlayerStatus{Volume: &v}, nil
	}}
	_, err := a.BeginRouteStream(context.Background(), "speaker:one", "old", "", func(store.Speaker) string { return "http://audio" })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); a.ObserveRoute(context.Background(), "speaker:one", "old") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read not started")
	}
	started := make(chan struct{})
	go func() {
		defer close(started)
		_, _ = a.BeginRouteStream(context.Background(), "speaker:one", "new", "", func(store.Speaker) string { return "http://new" })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		a.op.Lock()
		changed := a.routes["speaker:one"].source == "new"
		a.op.Unlock()
		if changed {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("takeover blocked on readback")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	<-started
	if a.Snapshot().Devices[0].Observed != nil {
		t.Fatal("stale observation committed")
	}
}
