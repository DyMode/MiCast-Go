package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
)

type fakeCloud struct {
	exchangeCalls, deviceCalls, failDevices int
	deviceErr                               error
	mu                                      sync.Mutex
	devices                                 []xiaomi.Device
	calls                                   map[string][]string
	failure                                 map[string]bool
	renewErr                                error
	pollGate                                chan struct{}
	pollStarted                             chan struct{}
}

func (f *fakeCloud) StartQR(context.Context) (xiaomi.QR, error) {
	return xiaomi.QR{LoginURL: "https://account.xiaomi.com/qr", PollURL: "https://account.xiaomi.com/poll"}, nil
}
func (f *fakeCloud) PollQR(ctx context.Context, q xiaomi.QR) (string, *xiaomi.Tokens, xiaomi.QR, error) {
	if f.pollStarted != nil {
		close(f.pollStarted)
		select {
		case <-f.pollGate:
		case <-ctx.Done():
			return "", nil, q, ctx.Err()
		}
	}
	return "confirmed", &xiaomi.Tokens{UserID: "u", ServiceToken: "s"}, q, nil
}
func (f *fakeCloud) Exchange(_ context.Context, t xiaomi.Tokens) (xiaomi.Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchangeCalls++
	if f.renewErr != nil {
		return t, f.renewErr
	}
	t.ServiceToken = "renewed-service"
	t.RefreshedAt = time.Now()
	return t, nil
}
func (f *fakeCloud) Devices(context.Context, xiaomi.Tokens) ([]xiaomi.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deviceCalls++
	if f.failDevices > 0 {
		f.failDevices--
		return nil, f.deviceErr
	}
	return f.devices, nil
}
func (f *fakeCloud) Command(_ context.Context, _ xiaomi.Tokens, id, method string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string][]string{}
	}
	f.calls[id] = append(f.calls[id], method)
	if f.failure[id] {
		return errors.New("offline")
	}
	return nil
}
func (f *fakeCloud) PlayURL(ctx context.Context, t xiaomi.Tokens, id, hardware, raw, audioID string) error {
	return f.Command(ctx, t, id, "play", nil)
}
func newApp(t *testing.T, f *fakeCloud) *App {
	t.Helper()
	dir := t.TempDir()
	config, err := store.NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := store.NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(config, vault, f)
	if err != nil {
		t.Fatal(err)
	}
	a.tokens = xiaomi.Tokens{UserID: "u", ServiceToken: "s", PassToken: "p", RefreshedAt: time.Now()}
	return a
}
func TestDiscoveryPreservesIndependentSwitches(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Name: "客厅", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	s := a.Snapshot().Devices[0]
	if !s.Joined || s.WholeHouse {
		t.Fatal("switches overwritten")
	}
}
func TestWholeHouseFailureIsolatedAndOffStopsOnlyOwnedSpeaker(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}, failure: map[string]bool{"two": true}}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	results, err := a.Control(ctx, "play_url", "http://local/audio.mp3", "", 0)
	if err != nil || results["one"] != "accepted" || results["two"] == "accepted" {
		t.Fatal(results, err)
	}
	off := false
	if err = a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["one"]) != 2 || len(f.calls["two"]) != 1 {
		t.Fatal("unexpected cross-speaker stop", f.calls)
	}
}
func TestLogoutInvalidatesInFlightQR(t *testing.T) {
	f := &fakeCloud{pollGate: make(chan struct{}), pollStarted: make(chan struct{})}
	a := newApp(t, f)
	qr, err := a.StartQR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() { s, _ := a.PollQR(context.Background(), qr.ID); done <- s }()
	<-f.pollStarted
	if err = a.Logout(); err != nil {
		t.Fatal(err)
	}
	close(f.pollGate)
	if <-done != "expired" || a.Snapshot().Account.UserID != "" {
		t.Fatal("logout resurrected")
	}
}
func TestNetworkRenewFailureKeepsCredentials(t *testing.T) {
	f := &fakeCloud{renewErr: errors.New("network unavailable")}
	a := newApp(t, f)
	a.tokens.RefreshedAt = time.Time{}
	if a.Renew(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	if a.Snapshot().Account.Status != "unstable" || a.tokens.PassToken != "p" {
		t.Fatal("network failure expired credentials")
	}
}

func TestWholeHouseRejoinDoesNotRestartOtherSpeakers(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}, {ID: "two", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Control(ctx, "play_url", "http://local/audio.mp3", "", 0); err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &on}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["one"]) != 3 || len(f.calls["two"]) != 1 {
		t.Fatal("unaffected speaker restarted", f.calls)
	}
	if err := a.UpdateSettings(ctx, "single", "one", "MiCast Go", "mp3"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["two"]) != 2 {
		t.Fatal("old target not stopped")
	}
}
func TestRenewRejectionSurvivesDeviceRefresh(t *testing.T) {
	f := &fakeCloud{renewErr: xiaomi.ErrRejected}
	a := newApp(t, f)
	a.tokens.RefreshedAt = time.Time{}
	if err := a.Renew(context.Background()); !errors.Is(err, xiaomi.ErrRejected) {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if a.Snapshot().Account.Status != "expired" {
		t.Fatal("expired account reactivated")
	}
}

func TestRetryCannotReviveOldSessionOrDisabledSpeaker(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	a.Control(ctx, "play_url", "old", "", 0)
	a.Control(ctx, "play_url", "new", "", 0)
	before := len(f.calls["one"])
	if err := a.RetryTargets(ctx, "old", map[string]bool{"one": true}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["one"]) != before {
		t.Fatal("old session was revived")
	}
	off := false
	if err := a.UpdateSpeaker(ctx, "one", SpeakerPatch{WholeHouse: &off}); err != nil {
		t.Fatal(err)
	}
	before = len(f.calls["one"])
	if err := a.RetryTargets(ctx, "new", map[string]bool{"one": true}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["one"]) != before {
		t.Fatal("disabled speaker was revived")
	}
}

func TestCloudRequestsAreCachedAndRenewalBacksOff(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}, renewErr: errors.New("temporary network failure")}
	a := newApp(t, f)
	ctx := context.Background()
	if err := a.Refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		_ = a.Snapshot()
		_ = a.Refresh(ctx, false)
		_ = a.Renew(ctx)
	}
	if f.deviceCalls != 1 || f.exchangeCalls != 0 {
		t.Fatal("status polling caused cloud requests", f.deviceCalls, f.exchangeCalls)
	}
	a.tokens.RefreshedAt = time.Now().Add(-8 * 24 * time.Hour)
	if err := a.Renew(ctx); err == nil {
		t.Fatal("expected transient renewal failure")
	}
	for range 30 {
		_ = a.Renew(ctx)
	}
	if f.exchangeCalls != 1 || time.Until(a.nextRenew) < 11*time.Hour || a.tokens.PassToken != "p" {
		t.Fatal("renewal failed to retain credentials/back off")
	}
}
func TestStoredAccountHealsOnceAfterUpgradeAndSurvivesRestart(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}, failDevices: 1, deviceErr: errors.New("old service token")}
	a := newApp(t, f)
	if err := a.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if f.exchangeCalls != 1 || f.deviceCalls != 2 || a.tokens.ServiceToken != "renewed-service" {
		t.Fatal("service login did not heal once")
	}
	restored, err := New(a.config, a.vault, f)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Account.UserID != "u" || restored.tokens.ServiceToken != "renewed-service" {
		t.Fatal("restart lost stored account")
	}
}
func TestDefinitiveRejectionPersistsAndAllowsEntryCleanup(t *testing.T) {
	f := &fakeCloud{devices: []xiaomi.Device{{ID: "one", Hardware: "OH2"}}, renewErr: xiaomi.ErrRejected}
	a := newApp(t, f)
	ctx := context.Background()
	_ = a.Refresh(ctx, true)
	if err := a.ClearEntries(); err == nil {
		t.Fatal("cleared connected account entries")
	}
	f.failDevices = 1
	f.deviceErr = errors.New("service login rejected")
	if err := a.Refresh(ctx, true); !errors.Is(err, xiaomi.ErrRejected) {
		t.Fatal(err)
	}
	restored, err := New(a.config, a.vault, f)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Account.Status != "expired" {
		t.Fatal("restart forgot confirmed expiry")
	}
	calls := f.exchangeCalls
	_ = restored.Renew(ctx)
	_ = restored.Refresh(ctx, true)
	if f.exchangeCalls != calls {
		t.Fatal("expired login kept requesting renewal")
	}
	if err := restored.ClearEntries(); err != nil || len(restored.Snapshot().Devices) != 0 {
		t.Fatal("could not clear expired entries", err)
	}
}
func TestTransientFailureKeepsAccountAndRecoveryIsRateLimited(t *testing.T) {
	f := &fakeCloud{failDevices: 5, deviceErr: errors.New("temporary device-list error"), renewErr: errors.New("DNS unavailable")}
	a := newApp(t, f)
	ctx := context.Background()
	for range 5 {
		_ = a.Refresh(ctx, true)
	}
	if f.exchangeCalls != 1 || a.tokens.UserID != "u" || a.tokens.Rejected || a.Snapshot().Account.Status == "expired" {
		t.Fatal("transient failure expired credentials or caused exchange storm")
	}
}
