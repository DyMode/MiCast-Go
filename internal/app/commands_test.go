package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
)

type gatedCloud struct {
	*fakeCloud
	entered chan struct{}
	once    sync.Once
}

func (f *gatedCloud) PlayURL(ctx context.Context, t xiaomi.Tokens, id, hardware, raw, audioID string) error {
	if raw == "blocked" {
		f.once.Do(func() { close(f.entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	return f.fakeCloud.PlayURL(ctx, t, id, hardware, raw, audioID)
}

func TestIdleMaintenanceNeverCommandsSpeakers(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	volume, muted := 17, true
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{Volume: &volume, Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateSettings(ctx, "whole_house", "", "Renamed", "wav"); err != nil {
		t.Fatal(err)
	}
	_ = a.Refresh(ctx, true)
	_ = a.RetryRouteTargets(ctx, "speaker:one", "", map[string]bool{"one": true})
	_ = a.RefreshRouteDisplay(ctx, "speaker:one", "metadata")
	_, _ = a.ControlRoute(ctx, "speaker:one", "stop", "", "", 0)
	if len(f.calls) != 0 {
		t.Fatal("idle maintenance woke a speaker", f.calls)
	}
	_, err := a.BeginGuardedRouteStream(ctx, "speaker:one", "cast", "", func(store.Speaker) string { return "http://audio" }, false, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := append([]string(nil), f.calls["one"]...)
	f.mu.Unlock()
	if len(calls) != 2 || calls[0] != "player_set_volume" || calls[1] != "play" {
		t.Fatal("saved idle volume was not applied on actual playback", calls)
	}
}

func TestNetworkWaitIsIndependentAndStopCancelsPendingStart(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	gated := &gatedCloud{fakeCloud: f, entered: make(chan struct{})}
	a.cloud = gated
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	first := make(chan struct{})
	go func() { defer close(first); _, _ = a.ControlRoute(work, "speaker:one", "play_url", "blocked", "", 0) }()
	select {
	case <-gated.entered:
	case <-time.After(time.Second):
		t.Fatal("no pending start")
	}
	second := make(chan struct{})
	go func() { defer close(second); _, _ = a.ControlRoute(ctx, "speaker:two", "play_url", "free", "", 0) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("one speaker blocked a different route")
	}
	stop := make(chan struct{})
	go func() { defer close(stop); _, _ = a.ControlRouteSource(ctx, "speaker:one", "blocked", "stop", 0) }()
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel pending start")
	}
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("old command worker did not exit")
	}
	if a.RouteOwns("speaker:one", "one") || !a.RouteOwns("speaker:two", "two") {
		t.Fatal("stop crossed routes")
	}
}

func TestSourceGuardPreventsBackgroundRevival(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	live := true
	_, err := a.BeginGuardedRouteStream(ctx, "speaker:one", "cast", "", func(store.Speaker) string { return "http://audio" }, false, func() bool { return live }, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.calls["one"])
	live = false
	_ = a.RetryRouteTargets(ctx, "speaker:one", "cast", map[string]bool{"one": true})
	_ = a.RefreshRouteDisplay(ctx, "speaker:one", "new cover", "cast")
	_ = a.UpdateSettings(ctx, "whole_house", "", "renamed", "wav")
	if len(f.calls["one"]) != before {
		t.Fatal("inactive source was replayed", f.calls)
	}
}

func TestEndedSourceCannotBeRestartedByMaintenance(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	background := true
	_, err := a.BeginGuardedRouteStream(ctx, "speaker:one", "cast", "", func(store.Speaker) string { return "http://audio" }, false, func() bool { return true }, nil, StreamOptions{Background: func() bool { return background }})
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.calls["one"])
	background = false
	_ = a.RetryRouteTargets(ctx, "speaker:one", "cast", map[string]bool{"one": true})
	_ = a.RefreshRouteDisplay(ctx, "speaker:one", "cover", "cast")
	format := "wav"
	_ = a.UpdateSpeaker(ctx, "one", SpeakerPatch{Format: &format})
	if len(f.calls["one"]) != before {
		t.Fatal("EOF source was replayed", f.calls)
	}
}

func TestExpiredAccountStillReleasesLocalRoute(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	_, _ = a.ControlRoute(ctx, "speaker:one", "play_url", "cast", "", 0)
	before := len(f.calls["one"])
	a.mu.Lock()
	a.status = "expired"
	a.mu.Unlock()
	_, err := a.ControlRouteSource(ctx, "speaker:one", "cast", "stop", 0)
	if err != nil || a.RouteOwns("speaker:one", "one") || a.routes["speaker:one"].source != "" {
		t.Fatal("expired route retained ownership", err)
	}
	if len(f.calls["one"]) != before {
		t.Fatal("expired account sent a cloud command")
	}
}
