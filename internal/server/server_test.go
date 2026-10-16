package server

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"micastgo/internal/app"
	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
)

func handler(t *testing.T, options ...Options) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	config, err := store.NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := store.NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(config, vault, xiaomi.NewClient())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{1}, 32)
	return New(a, key, options...), hex.EncodeToString(key)
}
func TestAuthenticationAndSafeStatus(t *testing.T) {
	h, key := handler(t)
	for _, valid := range []bool{false, true} {
		r := httptest.NewRequest("GET", "http://localhost/api/status", nil)
		if valid {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expected := 401
		if valid {
			expected = 200
		}
		if w.Code != expected {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "pass_token") {
			t.Fatal("credentials leaked")
		}
	}
}
func TestCrossOriginMutationRejected(t *testing.T) {
	h, key := handler(t)
	r := httptest.NewRequest("POST", "http://localhost/api/access", strings.NewReader(`{"key":"`+key+`"}`))
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestLoginCookieAndEmbeddedUI(t *testing.T) {
	h, key := handler(t)
	r := httptest.NewRequest("POST", "http://localhost/api/access", strings.NewReader(`{"key":"`+key+`"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	r = httptest.NewRequest("GET", "http://localhost/api/status", nil)
	r.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/icon.svg"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
}
func TestUnknownFieldsAndInvalidActionsRejected(t *testing.T) {
	h, key := handler(t)
	r := httptest.NewRequest("POST", "http://localhost/api/control", strings.NewReader(`{"action":"play_url","url":"file:///etc/passwd"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestGatewayOriginValidation(t *testing.T) {
	for _, tc := range []struct {
		name, origin, site, marker string
		gateway                    bool
		want                       int
	}{
		{"rewritten HTTPS host", "https://nas.example:5667", "same-origin", "1", true, 200},
		{"HTTP without fetch metadata", "http://192.168.1.8:5666", "", "1", true, 200},
		{"missing UI header", "https://nas.example", "same-origin", "", true, 403},
		{"cross site", "https://evil.example", "cross-site", "1", true, 403},
		{"same site different origin", "https://evil.example", "same-site", "1", true, 403},
		{"TCP spoofing", "https://nas.example", "same-origin", "1", false, 403},
		{"opaque origin", "null", "same-origin", "1", true, 403},
		{"invalid origin", "https://nas.example/path", "same-origin", "1", true, 403},
		{"direct browser", "http://localhost", "same-origin", "1", false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, key := handler(t, Options{TrustedGateway: tc.gateway})
			r := httptest.NewRequest("POST", "http://localhost/api/access", strings.NewReader(`{"key":"`+key+`"}`))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-MiCast-Request", tc.marker)
			r.Header.Set("X-Forwarded-Host", "nas.example")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	h, _ := handler(t, Options{TrustedGateway: true})
	r := httptest.NewRequest("OPTIONS", "http://localhost/api/access", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Headers", "X-MiCast-Request")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Headers") != "" {
		t.Fatal("cross-origin preflight permitted")
	}
}

func TestGlobalWholeHouseSwitchIsRemoved(t *testing.T) {
	h, key := handler(t)
	r := httptest.NewRequest("PATCH", "http://localhost/api/routes/whole_house", strings.NewReader(`{"enabled":false}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatal("global whole-house switch remains")
	}
	r = httptest.NewRequest("GET", "http://localhost/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "whole_enabled") {
		t.Fatal("obsolete switch exposed")
	}
}
