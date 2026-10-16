package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOptionalLoginMobileOriginAndPasswordLifecycle(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{1}, 32)
	access, err := NewAccessControl(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	tcp, _ := handler(t, Options{Access: access})
	gateway, _ := handler(t, Options{Access: access, TrustedGateway: true})
	request := func(h http.Handler, method, path, body, session string, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Origin", "null")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if session != "" {
			r.Header.Set("Authorization", "Bearer "+session)
		}
		if csrf {
			r.Header.Set("X-MiCast-CSRF", access.csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(tcp, "GET", "/api/status", "", "", false); w.Code != 200 {
		t.Fatal("first install requires login", w.Code)
	}
	if w := request(gateway, "PUT", "/api/access", `{"enabled":true,"password":"first-password"}`, "", false); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w := request(gateway, "PUT", "/api/access", `{"enabled":true,"password":"first-password"}`, "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	first := result.Session
	if w := request(tcp, "GET", "/api/status", "", "", false); w.Code != 401 {
		t.Fatal("enabled login not enforced")
	}
	if w := request(tcp, "GET", "/api/status", "", first, false); w.Code != 200 {
		t.Fatal("mobile bearer session failed")
	}
	w = request(gateway, "POST", "/api/access", `{"key":"first-password"}`, "", true)
	if w.Code != 200 {
		t.Fatal("opaque mobile Origin login blocked", w.Code, w.Body.String())
	}
	w = request(tcp, "PUT", "/api/access", `{"enabled":true,"current_password":"wrong","password":"second-password"}`, first, true)
	if w.Code != 400 {
		t.Fatal("password changed without current password")
	}
	w = request(gateway, "PUT", "/api/access", `{"enabled":true,"current_password":"first-password","password":"second-password"}`, first, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	second := result.Session
	if second == first {
		t.Fatal("password change did not rotate session")
	}
	if w := request(tcp, "GET", "/api/status", "", first, false); w.Code != 401 {
		t.Fatal("old session survived password change")
	}
	w = request(tcp, "PUT", "/api/access", `{"enabled":false,"current_password":"second-password"}`, second, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	restored, err := NewAccessControl(dir, key)
	if err != nil || restored.required() {
		t.Fatal("disabled login not persisted", err)
	}
	r := httptest.NewRequest("OPTIONS", "http://localhost/api/access", nil)
	r.Header.Set("Origin", "https://evil.test")
	r.Header.Set("Access-Control-Request-Headers", "X-MiCast-CSRF")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, r)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("cross-origin token access enabled")
	}
}

func TestPasswordAndCSRFMetadataAreSafe(t *testing.T) {
	access, err := NewAccessControl(t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := handler(t, Options{Access: access})
	r := httptest.NewRequest("GET", "http://localhost/api/access", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), access.token()) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, origin := range []string{"null", "https://mobile.example:5667", "http://192.168.1.2:5666"} {
		r = httptest.NewRequest("POST", "http://localhost/api/access", strings.NewReader(`{"key":"wrong"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("X-MiCast-CSRF", access.csrf)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("origin check hid actual password result", origin, w.Code)
		}
	}
}
