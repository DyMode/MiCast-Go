package xiaomi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type speakerTransport func(*http.Request) (*http.Response, error)

func (f speakerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func speakerResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestPlaybackFallbackOnlyAfterExplicitRejection(t *testing.T) {
	var methods []string
	c := NewClient()
	c.HTTP.Transport = speakerTransport(func(r *http.Request) (*http.Response, error) {
		r.ParseForm()
		method := r.Form.Get("method")
		methods = append(methods, method)
		if method == "player_play_music" {
			return speakerResponse(`{"code":0,"data":{"code":-1}}`), nil
		}
		return speakerResponse(`{"code":0,"data":{"code":0}}`), nil
	})
	tokens := Tokens{UserID: "u", ServiceToken: "s"}
	for i := 0; i < 2; i++ {
		if err := c.PlayURL(context.Background(), tokens, "one", "OH2", "http://media/track", "id"); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(methods, ","); got != "player_play_music,player_play_url,player_play_url" {
		t.Fatal(got)
	}
	tokens.UserID = "other"
	if err := c.PlayURL(context.Background(), tokens, "one", "OH2", "http://media/track", "id"); err != nil {
		t.Fatal(err)
	}
	if methods[3] != "player_play_music" {
		t.Fatal("preference leaked between accounts")
	}
	methods = nil
	c.HTTP.Transport = speakerTransport(func(r *http.Request) (*http.Response, error) {
		r.ParseForm()
		methods = append(methods, r.Form.Get("method"))
		return nil, context.DeadlineExceeded
	})
	if err := c.PlayURL(context.Background(), tokens, "two", "OH2", "http://media/track", ""); err == nil || len(methods) != 1 {
		t.Fatal("ambiguous failure retried", methods, err)
	}
}

func TestCloudConcurrencySharedAcrossClientsAndWaitCancelable(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	transport := speakerTransport(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return speakerResponse(`{"code":0,"data":[]}`), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := NewClient()
			c.HTTP.Transport = transport
			_, err := c.Devices(context.Background(), Tokens{UserID: "u", ServiceToken: "s"})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			wg.Wait()
			t.Fatal("requests did not enter")
		}
	}
	c := NewClient()
	c.HTTP.Transport = transport
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Devices(ctx, Tokens{UserID: "u", ServiceToken: "s"}); err == nil {
		t.Error("queued request ignored cancellation")
	}
	close(release)
	wg.Wait()
	if peak.Load() > 4 {
		t.Fatal("global concurrency exceeded", peak.Load())
	}
}

func TestStatusReadbackAndStrictMetadata(t *testing.T) {
	for _, data := range []string{`{"data":{"info":"{\"status\":1,\"volume\":37}"}}`, `{"status":"playing","volume_level":"37"}`} {
		var status PlayerStatus
		c := NewClient()
		c.HTTP.Transport = speakerTransport(func(*http.Request) (*http.Response, error) {
			return speakerResponse(`{"code":0,"data":` + data + `}`), nil
		})
		var err error
		status, err = c.PlayerStatus(context.Background(), Tokens{UserID: "u", ServiceToken: "s"}, "one")
		if err != nil || status.Volume == nil || *status.Volume != 37 || status.State != "playing" {
			t.Fatal(status, err)
		}
	}
	if title, artist := normalizeMetadata(" Song  Name ", "Singer--Song Name"); title != "Song Name" || artist != "Singer" {
		t.Fatal(title, artist)
	}
	for _, title := range []string{"作词：某人", "混音：某人", "Composer: Person"} {
		if normalized, _ := normalizeMetadata(title, ""); normalized != "" {
			t.Fatal("credit treated as track", title)
		}
	}
}
