package xiaomi

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func TestQRPollRefusalPreservesPendingLogin(t *testing.T) {
	for _, statusCode := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		client := NewClient()
		calls := 0
		client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: statusCode, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("refused")), Request: r}, nil
		})
		qr := QR{PollURL: "https://account.xiaomi.com/poll/test", DeviceID: "test-device"}
		status, tokens, pending, err := client.PollQR(context.Background(), qr)
		if err != nil || status != "waiting_limited" || tokens != nil || pending != qr || calls != 1 {
			t.Fatalf("HTTP %d: status=%s tokens=%v pending=%v calls=%d err=%v", statusCode, status, tokens, pending, calls, err)
		}
	}
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCompleteQRLoginExchange(t *testing.T) {
	client := NewClient()
	var paths []string
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		body := ""
		headers := http.Header{}
		switch r.URL.Path {
		case "/longPolling/loginUrl":
			if r.URL.Query().Get("sid") != "xiaomiio" {
				t.Error("QR SID mismatch")
			}
			body = `&&&START&&&{"loginUrl":"https://account.xiaomi.com/qr/abc","lp":"https://account.xiaomi.com/poll/abc"}`
		case "/poll/abc":
			body = `&&&START&&&{"userId":123456789012345678,"passToken":"private-pass"}`
		case "/pass/serviceLogin":
			cookie, err := r.Cookie("passToken")
			if err != nil || cookie.Value != "private-pass" {
				t.Error("pass token missing")
			}
			if r.URL.Query().Get("sid") != "micoapi" {
				t.Error("service SID mismatch")
			}
			body = `&&&START&&&{"code":0,"ssecurity":"security","nonce":123456789012345678,"location":"https://sts.api.io.mi.com/sts?ticket=abc"}`
		case "/sts":
			digest := sha1.Sum([]byte("nonce=123456789012345678&security"))
			if r.URL.Query().Get("clientSign") != base64.StdEncoding.EncodeToString(digest[:]) {
				t.Error("incorrect exchange signature")
			}
			headers.Add("Set-Cookie", "serviceToken=private-service; Path=/; Secure; HttpOnly")
			body = "ok"
		default:
			t.Error("unexpected path", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	qr, err := client.StartQR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, tokens, _, err := client.PollQR(context.Background(), qr)
	if err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || tokens.UserID != "123456789012345678" || tokens.ServiceToken != "private-service" || tokens.RefreshedAt.IsZero() {
		t.Fatal("login exchange incomplete")
	}
	if strings.Join(paths, ",") != "/longPolling/loginUrl,/poll/abc,/pass/serviceLogin,/sts" {
		t.Fatal(paths)
	}
}

func TestMiNARequestAndSpeakerCommands(t *testing.T) {
	commands := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("serviceToken")
		if err != nil || cookie.Value != "service-secret" {
			t.Error("missing service cookie")
		}
		if r.URL.Path == "/admin/v2/device_list" {
			if r.URL.Query().Get("requestId") == "" {
				t.Error("missing request id")
			}
			w.Write([]byte(`{"code":0,"data":[{"deviceID":"speaker","name":"客厅","hardware":"OH2"}]}`))
			return
		}
		r.ParseForm()
		if r.Form.Get("requestId") == "" || r.Form.Get("deviceId") != "speaker" {
			t.Error("missing command fields")
		}
		commands = append(commands, r.Form.Get("method"))
		var msg map[string]any
		if err = json.Unmarshal([]byte(r.Form.Get("message")), &msg); err != nil {
			t.Error(err)
		}
		if r.Form.Get("method") == "player_play_music" {
			var music map[string]any
			if json.Unmarshal([]byte(msg["music"].(string)), &music) != nil {
				t.Error("music payload malformed")
			}
			if msg["startaudioid"] != "track-123" {
				t.Error("audio id lost")
			}
		}
		w.Write([]byte(`{"code":0,"data":{"code":0}}`))
	}))
	defer srv.Close()
	client := NewClient()
	client.MinaBase = srv.URL
	tokens := Tokens{UserID: "user", ServiceToken: "service-secret"}
	devices, err := client.Devices(context.Background(), tokens)
	if err != nil || len(devices) != 1 {
		t.Fatal(err)
	}
	if err = client.PlayURL(context.Background(), tokens, "speaker", "OH2", "http://speaker-host/a.mp3", "track-123"); err != nil {
		t.Fatal(err)
	}
	if err = client.PlayURL(context.Background(), tokens, "speaker", "other", "http://speaker-host/a.mp3", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(commands, ",") != "player_play_music,player_play_url" {
		t.Fatal(commands)
	}
}
func TestCloudErrorsAreRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":42,"message":"SECRET_TOKEN"}`)) }))
	defer srv.Close()
	c := NewClient()
	c.MinaBase = srv.URL
	_, err := c.Devices(context.Background(), Tokens{UserID: "u", ServiceToken: "secret"})
	if err == nil || strings.Contains(err.Error(), "SECRET_TOKEN") {
		t.Fatal("unsafe cloud error", err)
	}
}
func TestTrustedLoginHosts(t *testing.T) {
	for _, raw := range []string{"http://account.xiaomi.com/", "https://account.xiaomi.com.evil.test/", "https://user:pass@account.xiaomi.com/", "https://account.xiaomi.com:8443/", "https://127.0.0.1/"} {
		if trustedURL(raw) {
			t.Fatal("accepted", raw)
		}
	}
	if !trustedURL("https://account.xiaomi.com/pass") || !trustedURL("https://sts.api.io.mi.com/sts") {
		t.Fatal("valid host rejected")
	}
}
