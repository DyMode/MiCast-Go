package dlna

import (
	"context"
	"encoding/binary"
	"micastgo/internal/playback"
	"micastgo/internal/testsupport"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A slow cloud or media start must not delay SOAP replies or progress polls.
func TestSlowPlaybackDoesNotBlockTransportQueries(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	addresses, _ := net.InterfaceAddrs()
	var host string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() {
			host = ip.String()
			break
		}
	}
	if host == "" {
		t.Skip("requires a LAN interface")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 44100*4)
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
	engine, err := playback.New(context.Background(), a, ffmpeg, "http://renderer", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(context.Background(), engine, "MiCast Go", "http://renderer", "latency-test")
	t.Cleanup(func() { s.Close(); engine.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	var once bool
	cloud.BeforePlay = func(ctx context.Context, _ testsupport.Play) error {
		if !once {
			once = true
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := media.URL + "/first.wav"
	start := time.Now()
	if w := soap(t, s, "AVTransport", "SetAVTransportURI", "<CurrentURI>"+first+"</CurrentURI>"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := soap(t, s, "AVTransport", "Play", "<Speed>1</Speed>"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("SetAVTransportURI + Play waited on playback: %v", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no playback attempt", engine.Status())
	}

	// Replace media while the first pipeline is still opening. Both the
	// replacement and every query must answer without waiting for the cloud.
	second := media.URL + "/second.wav"
	start = time.Now()
	for _, command := range []struct{ action, args string }{
		{"SetAVTransportURI", "<CurrentURI>" + second + "</CurrentURI>"},
		{"GetTransportInfo", ""},
		{"GetPositionInfo", ""},
		{"GetMediaInfo", ""},
		{"GetCurrentTransportActions", ""},
	} {
		if w := soap(t, s, "AVTransport", command.action, command.args); w.Code != 200 {
			t.Fatal(command.action, w.Body.String())
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("transport queries or replacement waited on playback: %v", elapsed)
	}
	close(release)
}

func TestMetadataDurationFollowsSelectedResource(t *testing.T) {
	raw := `<DIDL-Lite><item><dc:title>song</dc:title><res duration="00:01:00">http://media.example/low.mp3</res><res duration="00:02:30">http://media.example/high.flac</res></item></DIDL-Lite>`
	track := metadata(raw, "http://media.example/high.flac")
	if track.Title != "song" || track.Duration != 150 {
		t.Fatal("duration taken from the wrong resource", track)
	}
	track = metadata(raw, "http://media.example/unknown.mp3")
	if track.Duration != 60 {
		t.Fatal("fallback duration not chosen", track)
	}
}

func TestTransportSnapshotKeepsPositionWithCurrentTrack(t *testing.T) {
	s := &Server{uri: "http://media.example/song.mp3", owner: "cast", engineTrackID: "7", trackID: 7, desired: "PLAYING"}
	status := playback.Status{SessionID: "cast", PipelineID: "pipe", TrackID: "7", Protocol: "dlna", State: "playing", Position: 42}
	s.stateMu.Lock()
	snap := s.snapshotLocked(status)
	s.stateMu.Unlock()
	if snap.State != "PLAYING" || snap.Position != 42 {
		t.Fatal(snap)
	}
	// A late position from an old track must not leak into the new one.
	status.TrackID = "6"
	status.Position = 99
	s.stateMu.Lock()
	snap = s.snapshotLocked(status)
	s.stateMu.Unlock()
	if snap.Position == 99 {
		t.Fatal("stale pipeline position leaked", snap)
	}
}

func TestEventContainsCompleteAVTransportSnapshot(t *testing.T) {
	s := testRenderer(t)
	s.stateMu.Lock()
	s.uri, s.nextURI, s.desired = "http://media.example/song.mp3", "http://media.example/next.mp3", "PLAYING"
	s.stateMu.Unlock()
	body := s.eventBody("AVTransport")
	for _, name := range []string{"CurrentTrack", "NumberOfTracks", "CurrentTransportActions", "CurrentTrackDuration", "CurrentMediaDuration"} {
		if !strings.Contains(body, name) {
			t.Fatal("missing LastChange field", name)
		}
	}
}
