package dlna

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProtocolDiagnosticsBoundedAndRedacted(t *testing.T) {
	s := testRenderer(t)
	soap(t, s, "AVTransport", "SetAVTransportURI", `<CurrentURI>http://192.168.1.20/private?secret=token</CurrentURI><CurrentURIMetaData>private-title</CurrentURIMetaData>`)
	for range 140 {
		soap(t, s, "AVTransport", "GetPositionInfo", "")
	}
	info := s.Diagnostics()
	// Progress polling is sampled, so the meaningful command survives.
	if len(info.Events) != 2 || info.Events[0].Action != "SetAVTransportURI" || info.Events[0].Meta != "present" || info.Events[1].Action != "GetPositionInfo" {
		t.Fatal(info.Events)
	}
	if info.Events[1].Duration != "00:00:00" || info.State != "STOPPED" {
		t.Fatal(info)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"192.168.1.20", "secret=token", "private-title"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("diagnostic leaked", secret)
		}
	}
	// Mutations are never sampled and the trace stays bounded.
	for range 140 {
		s.recordProtocol("AVTransport", "Play", 200, nil)
	}
	if got := len(s.Diagnostics().Events); got != 128 {
		t.Fatal("trace not bounded", got)
	}
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "192.168.1.50:50000"
	r.Header.Set("NT", "upnp:event")
	r.Header.Set("CALLBACK", "<http://192.168.1.60/private>")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	info = s.Diagnostics()
	event := info.Events[len(info.Events)-1]
	if event.Action != "SUBSCRIBE" || event.Code != 412 {
		t.Fatal("rejected subscription not captured", event)
	}
}

func TestProgressPollingDoesNotEraseCommands(t *testing.T) {
	s := testRenderer(t)
	s.recordProtocol("AVTransport", "SetAVTransportURI", 200, map[string]string{"_meta": "empty", "_duration": "00:00:00"})
	for range 20 {
		s.recordProtocol("AVTransport", "GetPositionInfo", 200, map[string]string{"CurrentTransportState": "PLAYING", "RelTime": "00:00:01", "TrackDuration": "00:03:00"})
	}
	events := s.Diagnostics().Events
	if len(events) != 2 || events[0].Action != "SetAVTransportURI" || events[0].Meta != "empty" {
		t.Fatal(events)
	}
	// A real state change is always retained, even inside the sample window.
	s.recordProtocol("AVTransport", "GetPositionInfo", 200, map[string]string{"CurrentTransportState": "STOPPED", "RelTime": "00:03:00", "TrackDuration": "00:03:00"})
	if got := len(s.Diagnostics().Events); got != 3 {
		t.Fatal("state change dropped by polling sample", got)
	}
}

func TestNotificationDiagnosticsContainDeliveredState(t *testing.T) {
	s := testRenderer(t)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer callback.Close()
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("NT", "upnp:event")
	r.Header.Set("CALLBACK", "<"+callback.URL+"/notify>")
	s.ServeHTTP(httptest.NewRecorder(), r)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range s.Diagnostics().Events {
			if event.Action == "NOTIFY" {
				if event.Code != 200 || event.State != "STOPPED" || event.Sequence == nil || *event.Sequence != 0 {
					t.Fatal(event)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("missing notification diagnostic")
}
