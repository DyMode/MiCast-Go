package dlna

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOriginalRendererCallbackForms(t *testing.T) {
	for _, raw := range []string{
		"<http://127.0.0.1:1234/notify>",
		"<https://127.0.0.1/notify>",
		"<http://127.0.0.1:1234/first><http://127.0.0.1:5678/second>",
	} {

		targets, err := callbackTargets(context.Background(), raw, "127.0.0.1")
		if err != nil || len(targets) == 0 {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"<http://127.0.0.2:1234/notify>", "<http://user:pass@127.0.0.1/notify>", "<http://127.0.0.1/notify#fragment>", "garbage<http://127.0.0.1/notify>", "<file:///test>"} {
		if _, err := callbackTargets(context.Background(), raw, "127.0.0.1"); err == nil {
			t.Fatal("unsafe callback accepted", raw)
		}
	}
}

func TestOriginalRendererCallbackFailoverAndCaseInsensitiveNT(t *testing.T) {
	s := testRenderer(t)
	received := make(chan string, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case received <- string(data):
		default:
		}
		w.WriteHeader(200)
	}))
	defer second.Close()
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("NT", "UPNP:EVENT")
	r.Header.Set("CALLBACK", "<"+first.URL+"/first><"+second.URL+"/second>")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("original-compatible subscription rejected", w.Code)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "STOPPED") {
			t.Fatal(body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second callback did not receive state")
	}
}
