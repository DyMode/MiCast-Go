package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewaySupportsPreservedAndStrippedPrefixes(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(r.URL.Path)) })
	handler := gatewayHandler(base)
	for _, path := range []string{"/api/status", "/app/micast-go/api/status"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://nas"+path, nil))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "/api/status" {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://nas/app/micast-go", nil))
	if w.Code != 307 || w.Header().Get("Location") != "/app/micast-go/" {
		t.Fatal("missing gateway trailing slash")
	}
}
