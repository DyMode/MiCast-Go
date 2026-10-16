package app

import (
	"context"
	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
	"testing"
)

func TestIndependentRoutesAndLatestCastOwnership(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	start := func(route, source string) {
		t.Helper()
		result, err := a.ControlRoute(ctx, route, "play_url", source, "", 0)
		if err != nil || len(result) == 0 {
			t.Fatal(result, err)
		}
	}
	start("speaker:one", "one-source")
	start("speaker:two", "two-source")
	if !a.RouteOwns("speaker:one", "one") || !a.RouteOwns("speaker:two", "two") {
		t.Fatal("independent routes interfered")
	}
	start("whole_house", "whole-source")
	if !a.RouteOwns("speaker:one", "one") || !a.RouteOwns("whole_house", "two") {
		t.Fatal("whole house ignored independent membership")
	}
	countOne, countTwo := len(f.calls["one"]), len(f.calls["two"])
	_ = a.RetryRouteTargets(ctx, "speaker:two", "two-source", map[string]bool{"two": true})
	_ = a.RefreshRouteDisplay(ctx, "speaker:two", "different-song")
	_, _ = a.ControlRoute(ctx, "speaker:two", "pause", "", "", 0)
	_, _ = a.ControlRoute(ctx, "speaker:two", "stop", "", "", 0)
	if len(f.calls["one"]) != countOne || len(f.calls["two"]) != countTwo {
		t.Fatal("old session reclaimed/stopped another route", f.calls)
	}
	// Whole-house membership changes must not stop a single-speaker cast.
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &on}); err != nil {
		t.Fatal(err)
	}
	if !a.RouteOwns("speaker:one", "one") || len(f.calls["one"]) != countOne {
		t.Fatal("membership change stole individual playback")
	}
	if err := a.UpdateSpeaker(ctx, "two", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	if !a.RouteOwns("speaker:one", "one") || a.RouteOwns("whole_house", "two") {
		t.Fatal("whole-house shutdown affected independent ownership")
	}
	if len(f.calls["one"]) != countOne || len(f.calls["two"]) != countTwo+1 {
		t.Fatal("wrong speaker stopped", f.calls)
	}
	start("speaker:two", "new-two-source")
	if !a.RouteOwns("speaker:two", "two") {
		t.Fatal("fresh cast did not reset superseded state")
	}
}

func TestWholeHouseTransportNeverStopsNewIndividualCast(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	_, _ = a.ControlRoute(ctx, "whole_house", "play_url", "whole", "", 0)
	_, _ = a.ControlRoute(ctx, "speaker:one", "play_url", "solo", "", 0)
	count := len(f.calls["one"])
	_, _ = a.ControlRoute(ctx, "whole_house", "pause", "", "", 0)
	_, _ = a.ControlRoute(ctx, "whole_house", "stop", "", "", 0)
	if len(f.calls["one"]) != count || !a.RouteOwns("speaker:one", "one") {
		t.Fatal("whole-house controls affected newer individual cast")
	}
}

func TestClosedEngineCannotControlRecreatedEndpoint(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	_, _ = a.ControlRoute(ctx, "speaker:one", "play_url", "old-session", "", 0)
	_, _ = a.ControlRoute(ctx, "speaker:one", "play_url", "new-session", "", 0)
	count := len(f.calls["one"])
	_, _ = a.ControlRouteSource(ctx, "speaker:one", "old-session", "stop", 0)
	_ = a.RefreshRouteDisplay(ctx, "speaker:one", "old-song", "old-session")
	if len(f.calls["one"]) != count || !a.RouteOwns("speaker:one", "one") {
		t.Fatal("closed engine affected recreated route")
	}
}

func TestQueueContinuationDoesNotReclaimIndividualSpeaker(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	_, _ = a.ControlRoute(ctx, "whole_house", "play_url", "whole-old", "", 0)
	_, _ = a.ControlRoute(ctx, "speaker:one", "play_url", "individual", "", 0)
	_, _ = a.ControlRouteSource(ctx, "whole_house", "whole-old", "stop", 0)
	result, err := a.BeginRouteStream(ctx, "whole_house", "whole-next", "", func(s store.Speaker) string { return "http://local/" + s.ID }, true)
	if err != nil || len(result) != 1 || result["two"] != "accepted" || !a.RouteOwns("speaker:one", "one") {
		t.Fatal("queued next song reclaimed newer individual cast", result, err)
	}
}
