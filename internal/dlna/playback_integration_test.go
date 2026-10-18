package dlna

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"micastgo/internal/playback"
	"micastgo/internal/testsupport"
)

// Exercise the controller-facing sequence against real decoding and speaker
// pulls: subscribe, set URI, play, poll progress, queue next, observe advance.
func TestControllerSeesPlayingProgressAndQueuedAdvance(t *testing.T) {
	binaryPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var host string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() {
			host = ip.String()
			break
		}
	}
	if host == "" {
		t.Skip("requires a LAN interface for validated media downloads")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 44100*4*2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(i*31%16000))
	}
	header := make([]byte, 44)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(len(pcm)+36))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 2)
	binary.LittleEndian.PutUint32(header[24:], 44100)
	binary.LittleEndian.PutUint32(header[28:], 176400)
	binary.LittleEndian.PutUint16(header[32:], 4)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	media := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(header); w.Write(pcm) }))
	media.Listener = listener
	media.Start()
	defer media.Close()
	a, cloud := testsupport.NewApp(t, "wav")
	e, err := playback.New(context.Background(), a, binaryPath, "http://renderer", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(context.Background(), e, "MiCast Go", "http://renderer", "controller-test")
	defer e.Close()
	defer s.Close()
	notifications := make(chan string, 16)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case notifications <- string(data):
		default:
		}
		w.WriteHeader(200)
	}))
	defer callback.Close()
	request := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("NT", "upnp:event")
	request.Header.Set("CALLBACK", "<"+callback.URL+"/notify>")
	s.ServeHTTP(httptest.NewRecorder(), request)
	raw := media.URL + "/first.wav"
	metadata := `<item><title>first</title><res duration="00:00:02">` + raw + `</res></item>`
	for _, command := range []struct{ action, args string }{
		{"SetAVTransportURI", "<CurrentURI>" + raw + "</CurrentURI><CurrentURIMetaData>" + escape(metadata) + "</CurrentURIMetaData>"},
		{"Play", "<Speed>1</Speed>"},
	} {
		if result := soap(t, s, "AVTransport", command.action, command.args); result.Code != 200 {
			t.Fatal(result.Body.String())
		}
	}
	pull := func() <-chan struct{} {
		t.Helper()
		var raw string
		select {
		case play := <-cloud.Plays:
			raw = play.URL
		case <-time.After(8 * time.Second):
			t.Fatal("no speaker command", e.Status())
		}
		done := make(chan struct{})
		go func() { defer close(done); e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", raw, nil)) }()
		return done
	}
	firstDone := pull()
	deadline := time.Now().Add(3 * time.Second)
	playing := false
	for time.Now().Before(deadline) {
		result := soap(t, s, "AVTransport", "GetTransportInfo", "")
		if strings.Contains(result.Body.String(), "<CurrentTransportState>PLAYING</CurrentTransportState>") {
			playing = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !playing {
		t.Fatal("controller did not receive PLAYING", e.Status())
	}
	sawNotification := false
	for !sawNotification {
		select {
		case body := <-notifications:
			sawNotification = strings.Contains(body, "PLAYING") && !strings.Contains(body, "PAUSED_PLAYBACK")
		case <-time.After(3 * time.Second):
			t.Fatal("controller did not receive PLAYING notification")
		}
	}
	time.Sleep(1100 * time.Millisecond)
	position := soap(t, s, "AVTransport", "GetPositionInfo", "").Body.String()
	if !strings.Contains(position, "<RelTime>00:00:01</RelTime>") || !strings.Contains(position, "<TrackDuration>00:00:02</TrackDuration>") {
		t.Fatal("controller progress stalled", position)
	}
	// Progress is returned by GetPositionInfo and is not injected into the
	// LastChange notification payload.
	s.mu.Lock()
	event := s.eventBody("AVTransport")
	s.mu.Unlock()
	if strings.Contains(event, "RelativeTimePosition") || strings.Contains(event, "AbsoluteTimePosition") {
		t.Fatal("position fields added to original event contract")
	}
	next := media.URL + "/second.wav"
	result := soap(t, s, "AVTransport", "SetNextAVTransportURI", fmt.Sprintf("<NextURI>%s</NextURI><NextURIMetaData>%s</NextURIMetaData>", next, escape(`<item><title>second</title><res duration="00:00:02">`+next+`</res></item>`)))
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	secondDone := pull()
	waitPromoted(t, s, next)
	// Automatic promotion is one transaction: the controller must see the new
	// track without an artificial STOPPED for the old one.
	assertNoTrackStopped(t, notifications, raw)
	time.Sleep(1100 * time.Millisecond)
	position = soap(t, s, "AVTransport", "GetPositionInfo", "").Body.String()
	if !strings.Contains(position, "<RelTime>00:00:01</RelTime>") || !strings.Contains(position, "<TrackDuration>00:00:02</TrackDuration>") {
		t.Fatal("second track progress stalled", position)
	}
	third := media.URL + "/third.wav"
	result = soap(t, s, "AVTransport", "SetNextAVTransportURI", fmt.Sprintf("<NextURI>%s</NextURI>", third))
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	thirdDone := pull()
	waitPromoted(t, s, third)
	assertNoTrackStopped(t, notifications, next)
	// The third item has no DIDL metadata, so its duration must come from the
	// downloaded media rather than the previous track.
	deadline = time.Now().Add(5 * time.Second)
	for {
		position = soap(t, s, "AVTransport", "GetPositionInfo", "").Body.String()
		if strings.Contains(position, "<TrackDuration>00:00:02</TrackDuration>") && strings.Contains(position, "<RelTime>00:00:01</RelTime>") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("third track duration or progress missing", position)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, done := range []<-chan struct{}{firstDone, secondDone, thirdDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("speaker stream did not finish")
		}
	}
}

func waitPromoted(t *testing.T, s *Server, uri string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info := soap(t, s, "AVTransport", "GetMediaInfo", "").Body.String()
		if strings.Contains(info, "<CurrentURI>"+uri+"</CurrentURI>") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("queued track not promoted", uri)
}

func assertNoTrackStopped(t *testing.T, notifications <-chan string, uri string) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	for {
		select {
		case body := <-notifications:
			if strings.Contains(body, "STOPPED") && strings.Contains(body, uri) {
				t.Fatal("automatic advance published STOPPED for the previous track")
			}
		default:
			return
		}
	}
}
