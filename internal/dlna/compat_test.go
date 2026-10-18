package dlna

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"micastgo/internal/playback"
)

func TestStopRevokesQueueAndAllowsControllerReplacement(t *testing.T) {
	s := testRenderer(t)
	s.uri = "http://192.168.1.20/first.mp3"
	s.nextURI, s.nextMetadata = "http://192.168.1.20/stale.mp3", "stale"
	if w := soap(t, s, "AVTransport", "Stop", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.nextURI != "" || s.nextMetadata != "" || s.uri == "" {
		t.Fatal("stop must revoke continuation while preserving explicit replay")
	}
	// A controller that waits for STOPPED can provide the next current URI
	// later, without having used SetNextAVTransportURI or a subscription.
	w := soap(t, s, "AVTransport", "SetAVTransportURI", `<CurrentURI>http://192.168.1.20/later.mp3</CurrentURI>`)
	if w.Code != 200 || s.uri != "http://192.168.1.20/later.mp3" {
		t.Fatal(w.Body.String())
	}
	if w = soap(t, s, "AVTransport", "Next", ""); w.Code != 500 {
		t.Fatal("stopped queue was resurrected")
	}
}

func TestTransportLifecycleMapping(t *testing.T) {
	s := &Server{owner: "current"}
	for _, tc := range []struct{ state, want string }{
		{"transitioning", "TRANSITIONING"}, {"waiting_for_speakers", "TRANSITIONING"},
		{"playing", "PLAYING"}, {"paused", "PAUSED_PLAYBACK"},
		{"stopped", "STOPPED"}, {"error", "STOPPED"},
	} {
		status := playback.Status{SessionID: "current", Protocol: "dlna", State: tc.state}
		if got := s.transportState(status); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.state, got, tc.want)
		}
		status.Protocol = "airplay"
		if s.transportState(status) != "STOPPED" {
			t.Fatal("AirPlay leaked into DLNA transport")
		}
	}
}

func TestWebResumeAndSeekRetainDLNATransportOwnership(t *testing.T) {
	s := testRenderer(t)
	// This source is rejected locally; no external network is required to
	// verify the control identity used by the web and DLNA entry points.
	raw := "http://127.0.0.1:1/track.wav"
	id, err := s.engine.PlayMedia(raw, playback.Track{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.owner, s.uri = id, raw
	s.mu.Unlock()
	if err := s.engine.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.engine.Seek(3); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	owner := s.owner
	s.mu.Unlock()
	if status := s.engine.Status(); owner != status.SessionID {
		t.Fatal("web transport commands detached DLNA ownership", owner, status)
	}
	// A protocol Stop must still address the resumed media generation.
	w := soap(t, s, "AVTransport", "Stop", "")
	if w.Code != 200 || s.engine.Status().State != "stopped" {
		t.Fatal("DLNA could not stop the resumed cast", w.Body.String())
	}
}

func TestMiCastQueueAndEmptyURICompatibility(t *testing.T) {
	s := testRenderer(t)
	w := soap(t, s, "AVTransport", "SetAVTransportURI", `<CurrentURI>http://192.168.1.20/first.mp3</CurrentURI>`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = soap(t, s, "AVTransport", "SetNextAVTransportURI", `<NextURI>http://192.168.1.20/next.mp3</NextURI><NextURIMetaData>&lt;item&gt;&lt;title&gt;下一首&lt;/title&gt;&lt;/item&gt;</NextURIMetaData>`)
	if w.Code != 200 || !strings.Contains(s.nextMetadata, "下一首") {
		t.Fatal(w.Body.String())
	}
	w = soap(t, s, "AVTransport", "GetMediaInfo", "")
	if !strings.Contains(w.Body.String(), "http://192.168.1.20/next.mp3") {
		t.Fatal(w.Body.String())
	}
	w = soap(t, s, "AVTransport", "SetAVTransportURI", `<CurrentURI> </CurrentURI>`)
	if w.Code != 500 || s.uri != "http://192.168.1.20/first.mp3" || s.nextURI == "" {
		t.Fatal("empty URI erased transport/queue")
	}
	// A local-only rejected download allows testing session creation without LAN traffic.
	s.stateMu.Lock()
	s.nextURI = "http://127.0.0.1:1/next.mp3"
	s.stateMu.Unlock()
	w = soap(t, s, "AVTransport", "Next", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.stateMu.Lock()
		owner, next, title := s.owner, s.nextURI, s.track.Title
		s.stateMu.Unlock()
		if next == "" && owner != "" && title == "下一首" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Next did not start the queued track", owner, next, title)
		}
		time.Sleep(5 * time.Millisecond)
	}
	w = soap(t, s, "AVTransport", "SetNextAVTransportURI", `<NextURI>http://192.168.1.20/another.mp3</NextURI>`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = soap(t, s, "AVTransport", "SetAVTransportURI", `<CurrentURI>http://192.168.1.20/replacement.mp3</CurrentURI>`)
	if w.Code != 200 || s.nextURI != "" {
		t.Fatal("replacement preserved stale queue")
	}
}

func TestAutomaticNextIsLimitedToFinishedDLNASession(t *testing.T) {
	s := testRenderer(t)
	s.owner = "finished"
	s.nextURI = "http://127.0.0.1:1/next.mp3"
	for _, status := range []playback.Status{{Finished: true, SessionID: "finished", Protocol: "airplay"}, {Finished: false, SessionID: "finished", Protocol: "dlna"}, {Finished: true, SessionID: "old", Protocol: "dlna"}} {
		s.advanceLocked(status)
		if s.owner != "finished" || s.nextURI == "" {
			t.Fatal("stale session advanced queue")
		}
	}
	s.advanceLocked(playback.Status{Finished: true, SessionID: "finished", Protocol: "dlna"})
	if s.owner != "finished" || s.nextURI == "" {
		t.Fatal("unverified completion advanced the engine's current session")
	}
}

func TestSOAPOutputOrderAndEventFieldsMatchSCPD(t *testing.T) {
	s := testRenderer(t)
	w := soap(t, s, "AVTransport", "GetTransportInfo", "")
	xml := w.Body.String()
	if !(strings.Index(xml, "<CurrentTransportState>") < strings.Index(xml, "<CurrentTransportStatus>") && strings.Index(xml, "<CurrentTransportStatus>") < strings.Index(xml, "<CurrentSpeed>")) {
		t.Fatal("SOAP output argument order does not match SCPD", xml)
	}
	s.uri = "http://192.168.1.20/current.mp3"
	s.nextURI = "http://192.168.1.20/next.mp3"
	event := s.eventBody("AVTransport")
	for _, name := range []string{"AVTransportURI", "AVTransportURIMetaData", "CurrentTrackDuration", "CurrentMediaDuration", "NextAVTransportURI", "NextAVTransportURIMetaData"} {
		if !strings.Contains(event, name) {
			t.Fatal("missing LastChange field", name)
		}
	}
	if !strings.Contains(s.eventBody("ConnectionManager"), "SinkProtocolInfo") {
		t.Fatal("missing protocol event")
	}
}

func TestSubscriptionTimeoutAndDefaultCallbackPort(t *testing.T) {
	s := testRenderer(t)
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "192.168.1.50:50000"
	r.Header.Set("NT", "upnp:event")
	r.Header.Set("CALLBACK", "<http://192.168.1.50/events>")
	r.Header.Set("TIMEOUT", "Second-60")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("TIMEOUT") != "Second-60" {
		t.Fatal(w.Code, w.Header())
	}
	// Remove it before the event worker sends to the synthetic peer.
	s.mu.Lock()
	s.subscriptions = map[string]*subscription{}
	s.mu.Unlock()
}

func TestSharedSSDPRepliesForEveryReceiver(t *testing.T) {
	first, second := testRenderer(t), testRenderer(t)
	second.uuid = "uuid:second"
	second.base = "http://192.168.1.2:42310/receivers/second"
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	d := &Discovery{conn: conn, servers: map[*Server]bool{first: true, second: true}}
	d.respond("M-SEARCH * HTTP/1.1\r\nMAN: \"ssdp:discover\"\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n", peer.LocalAddr().(*net.UDPAddr))
	seen := map[string]bool{}
	buf := make([]byte, 8192)
	for range 2 {
		peer.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		text := string(buf[:n])
		if from.Port != conn.LocalAddr().(*net.UDPAddr).Port || !strings.Contains(text, "BOOTID.UPNP.ORG:") {
			t.Fatal(text)
		}
		seen[strings.Split(strings.Split(text, "LOCATION: ")[1], "\r\n")[0]] = true
	}
	if len(seen) != 2 || !seen[second.base+"/dlna/description.xml"] {
		t.Fatal("single discovery listener did not isolate receiver locations", seen)
	}
}

func TestVolumeWhileMutedDoesNotUnmute(t *testing.T) {
	s := testRenderer(t)
	for _, step := range []struct{ action, args string }{{"SetMute", `<DesiredMute>1</DesiredMute>`}, {"SetVolume", `<DesiredVolume>23</DesiredVolume>`}} {
		w := soap(t, s, "RenderingControl", step.action, step.args)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	volume, muted := s.engine.VolumeState()
	if !muted || volume != 23 {
		t.Fatal("SetVolume cleared mute or lost requested restore volume")
	}
	w := soap(t, s, "RenderingControl", "SetMute", `<DesiredMute>0</DesiredMute>`)
	volume, muted = s.engine.VolumeState()
	if w.Code != 200 || muted || volume != 23 {
		t.Fatal("unmute did not restore requested volume")
	}
}

func TestTerminalEngineStopClearsQueueButStaleStopDoesNot(t *testing.T) {
	s := testRenderer(t)
	s.owner = "current"
	s.nextURI = "http://media/next"
	s.advanceLocked(playback.Status{SessionID: "old", Protocol: "dlna", EndReason: "stopped"})
	if s.nextURI == "" {
		t.Fatal("stale stop cleared current queue")
	}
	s.advanceLocked(playback.Status{SessionID: "current", Protocol: "dlna", EndReason: "stopped"})
	if s.nextURI != "" {
		t.Fatal("engine stop retained queued continuation")
	}
}

func TestOriginalRendererPositionCountersAndServiceContract(t *testing.T) {
	s := testRenderer(t)
	w := soap(t, s, "AVTransport", "GetPositionInfo", "")
	for _, field := range []string{"<RelCount>0</RelCount>", "<AbsCount>0</AbsCount>"} {
		if !strings.Contains(w.Body.String(), field) {
			t.Fatal("position differs from the reference contract", w.Body.String())
		}
	}
	for _, value := range []string{"STOPPED", "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING", "NO_MEDIA_PRESENT", "OK", "ERROR_OCCURRED", "REL_TIME"} {
		if !strings.Contains(scpd("AVTransport"), "<allowedValue>"+value+"</allowedValue>") {
			t.Fatal("reference state declaration missing", value)
		}
	}
	if !strings.Contains(scpd("AVTransport"), "<name>AVTransportURI</name><dataType>uri</dataType>") {
		t.Fatal("URI type differs from original")
	}
	if !strings.Contains(scpd("RenderingControl"), "<allowedValue>Master</allowedValue>") {
		t.Fatal("Master channel declaration missing")
	}
}
