package playback

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type mediaTestTransport func(*http.Request) (*http.Response, error)

func (f mediaTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func queuedDownload(t *testing.T, e *Engine) *prefetch {
	t.Helper()
	e.prefetchMu.Lock()
	p := e.prefetch
	e.prefetchMu.Unlock()
	if p == nil {
		t.Fatal("no queued download")
	}
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("queued download not completed")
	}
	return p
}

func TestNextDownloadSurvivesEOFAndPromotesWithoutSecondRequest(t *testing.T) {
	e := lifecycleEngine(t)
	var requests atomic.Int32
	e.transport = mediaTestTransport(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("next-track")))}, nil
	})
	s := e.newSession("dlna", Track{}, 0)
	e.PrefetchNext(s.identity, "http://media/next")
	p := queuedDownload(t, e)
	e.PrefetchNext(s.identity, "http://media/next")
	if e.cachePath != "" {
		t.Fatal("prefetch replaced current cache")
	}
	e.stop(s.id, true)
	select {
	case <-p.done:
	default:
		t.Fatal("download lost")
	}
	e.op.Lock()
	p = e.takePrefetch("http://media/next")
	next := e.newSession("dlna", Track{}, 0)
	next.prefetched = p
	e.op.Unlock()
	path, err := e.download(next, "http://media/next")
	if err != nil || requests.Load() != 1 {
		t.Fatal("next was downloaded twice", requests.Load(), err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "next-track" {
		t.Fatal(string(data), err)
	}
}

func TestNextDownloadCanceledByStopAndTakeover(t *testing.T) {
	for _, action := range []string{"stop", "takeover"} {
		t.Run(action, func(t *testing.T) {
			e := lifecycleEngine(t)
			entered := make(chan struct{})
			e.transport = mediaTestTransport(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			s := e.newSession("dlna", Track{}, 0)
			e.PrefetchNext(s.identity, "http://media/next")
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("no download")
			}
			e.prefetchMu.Lock()
			p := e.prefetch
			e.prefetchMu.Unlock()
			if action == "stop" {
				e.Stop(s.identity)
			} else {
				e.newSession("airplay", Track{}, 0)
			}
			select {
			case <-p.done:
			case <-time.After(time.Second):
				t.Fatal("download not canceled")
			}
			e.prefetchMu.Lock()
			remaining := e.prefetch
			e.prefetchMu.Unlock()
			if remaining != nil {
				t.Fatal("queued reference retained")
			}
		})
	}
}

func TestFailedPrefetchFallsBackAndOldOwnerCannotQueue(t *testing.T) {
	e := lifecycleEngine(t)
	var requests atomic.Int32
	e.transport = mediaTestTransport(func(*http.Request) (*http.Response, error) {
		status := 200
		if requests.Add(1) == 1 {
			status = 500
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString("track"))}, nil
	})
	s := e.newSession("dlna", Track{}, 0)
	e.PrefetchNext(s.identity, "http://media/next")
	queuedDownload(t, e)
	e.op.Lock()
	p := e.takePrefetch("http://media/next")
	next := e.newSession("dlna", Track{}, 0)
	next.prefetched = p
	e.op.Unlock()
	if _, err := e.download(next, "http://media/next"); err != nil || requests.Load() != 2 {
		t.Fatal("failed prefetch prevented playback", err)
	}
	e.PrefetchNext(s.identity, "http://media/old")
	e.prefetchMu.Lock()
	remaining := e.prefetch
	e.prefetchMu.Unlock()
	if remaining != nil {
		t.Fatal("old owner queued work")
	}
}
