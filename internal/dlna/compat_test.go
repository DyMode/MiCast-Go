package dlna

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"micastgo/internal/playback"
)

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
	s.nextURI = "http://127.0.0.1:1/next.mp3"
	w = soap(t, s, "AVTransport", "Next", "")
	if w.Code != 200 || s.nextURI != "" || s.owner == "" || s.track.Title != "下一首" {
		t.Fatal(w.Body.String())
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
	if s.owner == "finished" || s.nextURI != "" {
		t.Fatal("finished DLNA session did not advance")
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
	if !s.muted || s.volume != 23 || s.beforeMute != 23 {
		t.Fatal("SetVolume cleared mute or lost requested restore volume")
	}
	w := soap(t, s, "RenderingControl", "SetMute", `<DesiredMute>0</DesiredMute>`)
	if w.Code != 200 || s.muted || s.volume != 23 {
		t.Fatal("unmute did not restore requested volume")
	}
}
