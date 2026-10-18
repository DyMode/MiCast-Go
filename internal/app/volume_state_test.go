package app

import (
	"context"
	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
	"testing"
)

func TestCurrentVolumeSurvivesResumeAndStop(t *testing.T) {
	ctx := context.Background()
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	saved := a.Config().Speakers["one"].Volume
	_, err := a.BeginGuardedRouteStream(ctx, "speaker:one", "cast", "", func(store.Speaker) string { return "http://audio" }, false, nil, nil, StreamOptions{Volume: func() (int, bool) { return 70, true }})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		d := a.Snapshot().Devices[0]
		if d.CurrentVolume == nil || *d.CurrentVolume != want {
			t.Fatalf("current volume=%v, want %d", d.CurrentVolume, want)
		}
	}
	check(70)
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{Volume: &saved}); err != nil {
		t.Fatal(err)
	}
	check(saved)
	control := func(action string, v int) {
		t.Helper()
		if _, err := a.ControlRouteSource(ctx, "speaker:one", "cast", action, v); err != nil {
			t.Fatal(err)
		}
	}
	control("pause", 0)
	control("play", 0)
	check(saved)
	control("pause", 0)
	control("volume", 42)
	control("play", 0)
	check(42)
	control("stop", 0)
	check(42)
	if a.Config().Speakers["one"].Volume != saved {
		t.Fatal("runtime volume overwrote saved baseline")
	}
}
