package dlna

import (
	"context"
	"encoding/xml"
	"io"
	"micastgo/internal/playback"
	"micastgo/internal/testsupport"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func testRenderer(t *testing.T) *Server {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	a, _ := testsupport.NewApp(t, "mp3")
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := playback.New(ctx, a, ffmpeg, "http://192.168.1.2:42310", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(ctx, engine, "MiCast Go", "http://192.168.1.2:42310", "test")
	t.Cleanup(func() { s.Close(); cancel(); engine.Close() })
	return s
}
func soap(t *testing.T, s *Server, service, action, arguments string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` + action + ` xmlns:u="` + serviceTypes[service] + `">` + arguments + `</u:` + action + `></s:Body></s:Envelope>`
	r := httptest.NewRequest("POST", "http://renderer/dlna/"+service+"/control", strings.NewReader(body))
	r.Header.Set("SOAPAction", `"`+serviceTypes[service]+"#"+action+`"`)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestDescriptionAndServiceDocumentsAreValidXML(t *testing.T) {
	s := testRenderer(t)
	for _, path := range []string{"description.xml", "AVTransport/scpd.xml", "RenderingControl/scpd.xml", "ConnectionManager/scpd.xml"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "http://renderer/dlna/"+path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		decoder := xml.NewDecoder(w.Body)
		for {
			_, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(path, err)
			}
		}
	}
}
func TestTransportURIAndMetadata(t *testing.T) {
	s := testRenderer(t)
	didl := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item><dc:title>歌曲</dc:title><upnp:artist>歌手</upnp:artist><res duration="00:03:12">http://192.168.1.3/song.mp3</res></item></DIDL-Lite>`
	w := soap(t, s, "AVTransport", "SetAVTransportURI", "<InstanceID>0</InstanceID><CurrentURI>http://192.168.1.3/song.mp3</CurrentURI><CurrentURIMetaData>"+escape(didl)+"</CurrentURIMetaData>")
	if w.Code != 200 || s.track.Title != "歌曲" || s.track.Artist != "歌手" || s.track.Duration != 192 {
		t.Fatal(w.Code, w.Body.String(), s.track)
	}
	w = soap(t, s, "AVTransport", "GetMediaInfo", "<InstanceID>0</InstanceID>")
	if !strings.Contains(w.Body.String(), "00:03:12") {
		t.Fatal(w.Body.String())
	}
	w = soap(t, s, "AVTransport", "GetTransportInfo", "<InstanceID>1</InstanceID>")
	if w.Code != 500 || !strings.Contains(w.Body.String(), "718") {
		t.Fatal("invalid InstanceID accepted")
	}
}
func TestInvalidSOAPAndMediaAddressesRejected(t *testing.T) {
	s := testRenderer(t)
	for _, raw := range []string{"file:///etc/passwd", "http://127.0.0.1:42310/api/status", "http://169.254.169.254/latest/meta-data"} {
		w := soap(t, s, "AVTransport", "SetAVTransportURI", "<CurrentURI>"+raw+"</CurrentURI>")
		if w.Code != 500 {
			t.Fatal("unsafe URI accepted", raw)
		}
	}
	w := soap(t, s, "AVTransport", "UnknownAction", "")
	if w.Code != 500 {
		t.Fatal("unknown action accepted")
	}
}
func TestEventsCannotCallDifferentPeer(t *testing.T) {
	s := testRenderer(t)
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "192.168.1.50:30000"
	r.Header.Set("NT", "upnp:event")
	r.Header.Set("CALLBACK", "<http://127.0.0.1:1234/internal>")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 412 {
		t.Fatal("callback peer not enforced")
	}
}
func TestEventInitialNotifyAndUnsubscribe(t *testing.T) {
	s := testRenderer(t)
	received := make(chan *http.Request, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "NOTIFY" {
			select {
			case received <- r.Clone(r.Context()):
			default:
			}
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer callback.Close()
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "127.0.0.1:30000"
	r.Header.Set("NT", "upnp:event")
	r.Header.Set("CALLBACK", "<"+callback.URL+"/notify>")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("SID") == "" {
		t.Fatal(w.Code)
	}
	select {
	case notify := <-received:
		if notify.Header.Get("SEQ") != "0" {
			t.Fatal("initial sequence wrong")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing initial notification")
	}
	r = httptest.NewRequest("UNSUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "127.0.0.1:30000"
	r.Header.Set("SID", w.Header().Get("SID"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
