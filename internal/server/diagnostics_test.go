package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticExportWritesUserFile(t *testing.T) {
	dataDir := t.TempDir()
	h, key := handler(t, Options{DataDir: dataDir})
	r := httptest.NewRequest("POST", "http://localhost/api/diagnostics/export", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Name == "" {
		t.Fatal(w.Body.String(), err)
	}
	path := filepath.Join(dataDir, "micast-go", result.Name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "MiCast Go 诊断报告") || !strings.Contains(text, "version=0.1.1") {
		t.Fatal(text)
	}
	if strings.Contains(text, key) {
		t.Fatal("diagnostic export leaked the access key")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "micast-go", "latest.txt")); err != nil {
		t.Fatal("latest report missing", err)
	}
}

func TestDiagnosticsAuthenticatedAndSettingsValidated(t *testing.T) {
	h, key := handler(t)
	for _, authorized := range []bool{false, true} {
		r := httptest.NewRequest("GET", "http://localhost/api/diagnostics", nil)
		if authorized {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !authorized {
			if w.Code != http.StatusUnauthorized {
				t.Fatal(w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, sensitive := range []string{"pass_token", "service_token", "owner", "cacheURL", "mediaURL", key} {
			if strings.Contains(w.Body.String(), sensitive) {
				t.Fatal("diagnostic leak", sensitive)
			}
		}
	}
	for _, body := range []string{
		`{"name":"MiCast Go","format":"mp3","volume_mode":"independent","equalizer":{"enabled":true,"bass":3,"mid":-1,"treble":2}}`,
		`{"name":"MiCast Go","format":"mp3","equalizer":{"enabled":true,"bass":20}}`,
	} {
		r := httptest.NewRequest("PUT", "http://localhost/api/settings", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if strings.Contains(body, "20") {
			want = 400
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var data struct {
		Config struct {
			Equalizer struct {
				Bass float64 `json:"bass"`
			} `json:"equalizer"`
		} `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || data.Config.Equalizer.Bass != 3 {
		t.Fatal("invalid EQ changed settings", w.Body.String(), err)
	}
}
