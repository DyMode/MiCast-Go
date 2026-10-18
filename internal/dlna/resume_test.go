package dlna

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func subscribeForTest(t *testing.T, s *Server, callback, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("SUBSCRIBE", "http://renderer/dlna/AVTransport/event", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	if sid != "" {
		r.Header.Set("SID", sid)
	} else {
		r.Header.Set("NT", "upnp:event")
		r.Header.Set("CALLBACK", "<"+callback+"/notify>")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("subscription failed", w.Code)
	}
	return w
}

func TestWakeRenewalReplaysUnchangedState(t *testing.T) {
	s := testRenderer(t)
	received := make(chan string, 8)
	var asleep atomic.Bool
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if asleep.Load() {
			w.WriteHeader(503)
			return
		}
		received <- r.Header.Get("SEQ")
		w.WriteHeader(200)
	}))
	defer callback.Close()
	sid := subscribeForTest(t, s, callback.URL, "").Header().Get("SID")
	select {
	case seq := <-received:
		if seq != "0" {
			t.Fatal(seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial state")
	}
	// Wait for the initial HTTP reply to be committed before going to sleep.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ready := s.subscriptions[sid].last != ""
		s.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	asleep.Store(true)
	subscribeForTest(t, s, "", sid)
	deadline = time.Now().Add(2 * time.Second)
	failed := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		failed = s.subscriptions[sid].failures > 0
		s.mu.Unlock()
		if failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !failed {
		t.Fatal("sleeping callback not exercised")
	}
	asleep.Store(false)
	subscribeForTest(t, s, "", sid)
	select {
	case seq := <-received:
		if seq != "1" {
			t.Fatal("renewal reset or skipped sequence", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake did not receive unchanged state")
	}
}

func TestRenewalDuringInflightNotifyCannotLoseRefresh(t *testing.T) {
	s := testRenderer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	received := make(chan string, 4)
	var count atomic.Int32
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if count.Add(1) == 1 {
			close(entered)
			<-release
		}
		received <- r.Header.Get("SEQ")
		w.WriteHeader(200)
	}))
	defer callback.Close()
	sid := subscribeForTest(t, s, callback.URL, "").Header().Get("SID")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("no pending notification")
	}
	subscribeForTest(t, s, "", sid)
	close(release)
	for _, expected := range []string{"0", "1"} {
		select {
		case seq := <-received:
			if seq != expected {
				t.Fatal(seq, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("pending old reply suppressed renewed snapshot")
		}
	}
}
