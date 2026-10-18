package receivers

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"micastgo/internal/app"
	"micastgo/internal/playback"
	"micastgo/internal/testsupport"
	"micastgo/internal/xiaomi"
)

func newManager(t *testing.T) (*Manager, *testsupport.Cloud, *httptest.Server, net.Listener) {
	t.Helper()
	a, cloud := testsupport.NewApp(t, "wav")
	cloud.DevicesList = []xiaomi.Device{{ID: "speaker", Name: "客厅", Hardware: "OH2"}, {ID: "second", Name: "卧室", Hardware: "OH2"}}
	if err := a.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { occupied.Close() })
	binary, _ := os.Executable()
	var manager *Manager
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { manager.ServeHTTP(w, r) }))
	t.Cleanup(web.Close)
	manager, err = New(context.Background(), a, Options{Host: "127.0.0.1", Base: web.URL, Binary: binary, CacheDir: t.TempDir(), Identity: "test-identity", AirplayAddress: occupied.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager, cloud, web, occupied
}
func TestIndependentPublicationSwitchesAndPortFallback(t *testing.T) {
	m, _, web, occupied := newManager(t)
	statuses := m.Status()
	if len(statuses) != 3 {
		t.Fatal(statuses)
	}
	seen := map[string]bool{}
	for _, status := range statuses {
		if status.Airplay != "ready" || status.DLNA != "ready" || seen[status.AirplayAddress] || status.AirplayAddress == occupied.Addr().String() {
			t.Fatal("port reuse or receiver failure", status)
		}
		seen[status.AirplayAddress] = true
		response, err := web.Client().Get(status.DescriptionURL)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(raw), status.Name) {
			t.Fatal("description routed incorrectly", string(raw))
		}
		if status.Kind == "speaker" && !strings.Contains(string(raw), "/receivers/") {
			t.Fatal("service URL escaped speaker route", string(raw))
		}
	}
	single, _ := m.Engine("speaker:speaker")
	_ = m.app.UpdateSpeaker(context.Background(), "speaker", app.SpeakerPatch{WholeHouse: boolPointer(false)})
	if err := m.app.UpdateSpeaker(context.Background(), "second", app.SpeakerPatch{WholeHouse: boolPointer(false)}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 2 {
		t.Fatal("whole-house publication was not removed")
	}
	if _, err := m.Engine("whole_house"); err == nil {
		t.Fatal("disabled whole-house entry still accepts casts")
	}
	if actual, _ := m.Engine("speaker:speaker"); actual != single {
		t.Fatal("whole-house switch restarted individual entry")
	}
	off := false
	if err := m.app.UpdateSpeaker(context.Background(), "speaker", app.SpeakerPatch{Joined: &off}); err != nil {
		t.Fatal(err)
	}
	_ = m.Sync()
	if len(m.Status()) != 1 {
		t.Fatal("speaker switch did not remove its entry")
	}
	if err := m.app.UpdateSpeaker(context.Background(), "second", app.SpeakerPatch{WholeHouse: boolPointer(true)}); err != nil {
		t.Fatal(err)
	}
	_ = m.Sync()
	if len(m.Status()) != 2 {
		t.Fatal("whole-house entry did not return")
	}
}

func TestConcurrentPCMStreamsRemainIndependentAndOldPullIsRevoked(t *testing.T) {
	m, cloud, web, _ := newManager(t)
	first, _ := m.Engine("speaker:speaker")
	second, _ := m.Engine("speaker:second")
	start := func(engine *playback.Engine, title string, fill byte) {
		t.Helper()
		reader, writer := io.Pipe()
		t.Cleanup(func() { writer.Close() })
		if _, err := engine.StartPCM(reader, playback.Track{Title: title}); err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = writer.Write(bytes.Repeat([]byte{fill}, 44100)) }()
	}
	wait := func() testsupport.Play {
		t.Helper()
		select {
		case play := <-cloud.Plays:
			return play
		case <-time.After(5 * time.Second):
			t.Fatal("speaker received no cast")
			return testsupport.Play{}
		}
	}
	start(first, "客厅歌曲", 11)
	one := wait()
	start(second, "卧室歌曲", 22)
	two := wait()
	if one.ID == two.ID || one.URL == two.URL || first.Status().Track.Title != "客厅歌曲" || second.Status().Track.Title != "卧室歌曲" {
		t.Fatal("independent sessions overwrote one another", one, two)
	}
	for _, test := range []struct {
		play testsupport.Play
		fill byte
	}{{one, 11}, {two, 22}} {
		response, err := web.Client().Get(test.play.URL)
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 44+512)
		_, err = io.ReadFull(response.Body, data)
		response.Body.Close()
		if err != nil || !bytes.Equal(data[44:], bytes.Repeat([]byte{test.fill}, 512)) {
			t.Fatal("wrong audio routed to speaker", test.play, err)
		}
	}
	whole, _ := m.Engine("whole_house")
	start(whole, "全屋歌曲", 33)
	_ = wait()
	_ = wait()
	response, err := web.Client().Head(one.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("superseded single-speaker URL remained valid", response.StatusCode)
	}
	first.Stop("")
	if !m.app.RouteOwns("whole_house", "speaker") || !m.app.RouteOwns("whole_house", "second") {
		t.Fatal("old sender stopped whole-house playback")
	}
}

func TestNoWholeHousePublicationWithoutMembers(t *testing.T) {
	m, _, _, _ := newManager(t)
	off, on := false, true
	for _, id := range []string{"speaker", "second"} {
		if err := m.app.UpdateSpeaker(context.Background(), id, app.SpeakerPatch{WholeHouse: &off}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 2 {
		t.Fatal("empty whole-house route stayed published or erased user preference")
	}
	if _, err := m.Engine("whole_house"); err == nil {
		t.Fatal("empty whole-house route accepts casts")
	}
	if err := m.app.UpdateSpeaker(context.Background(), "speaker", app.SpeakerPatch{WholeHouse: &on}); err != nil {
		t.Fatal(err)
	}
	_ = m.Sync()
	if len(m.Status()) != 3 {
		t.Fatal("whole-house route did not return with first member")
	}
	for _, status := range m.Status() {
		if status.Kind == "speaker" && strings.Contains(status.Name, "MiCast") {
			t.Fatal("speaker names contain product suffix")
		}
	}
}

func TestLogoutWithdrawsUnusableReceivers(t *testing.T) {
	m, _, _, _ := newManager(t)
	if err := m.app.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 0 {
		t.Fatal("logged-out account still publishes unusable entries")
	}
}

func boolPointer(v bool) *bool { return &v }

func TestRenamePreservesEngineAndDoesNotCommandSpeaker(t *testing.T) {
	m, cloud, _, _ := newManager(t)
	before, _ := m.Engine("whole_house")
	config := m.app.Config()
	if err := m.app.UpdateSettings(context.Background(), config.Mode, config.Selected, "Renamed", config.Format); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Engine("whole_house")
	if before != after {
		t.Fatal("rename replaced playback engine")
	}
	select {
	case p := <-cloud.Plays:
		t.Fatal("idle rename commanded playback", p)
	default:
	}
	for _, status := range m.Status() {
		if status.ID == "whole_house" && status.Name != "Renamed" {
			t.Fatal("discovery name not updated")
		}
	}
}
