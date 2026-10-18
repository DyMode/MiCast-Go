package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"micastgo/internal/app"
	"micastgo/internal/playback"
	"micastgo/internal/receivers"
	"micastgo/internal/store"
)

//go:embed web/*
var files embed.FS

type Server struct {
	accessControl *AccessControl
	routes        *receivers.Manager
	app           *app.App
	key           string
	mux           *http.ServeMux
	engine        *playback.Engine
	passwordHash  string
	dataDir       string
}
type Options struct {
	Access *AccessControl
	Routes *receivers.Manager
	// TrustedGateway is only enabled on the local fnOS Unix socket.
	TrustedGateway bool
	Engine         *playback.Engine
	PasswordHash   string
	DataDir        string
}

func New(a *app.App, key []byte, options ...Options) http.Handler {
	s := &Server{app: a, key: hex.EncodeToString(key), mux: http.NewServeMux()}
	if len(options) > 0 {
		s.accessControl = options[0].Access
		s.routes = options[0].Routes
		s.engine = options[0].Engine
		s.passwordHash = options[0].PasswordHash
		s.dataDir = options[0].DataDir
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("POST /api/access", s.access)
	if s.accessControl != nil {
		s.mux.HandleFunc("GET /api/access", s.accessInfo)
		s.mux.Handle("PUT /api/access", s.authorize(http.HandlerFunc(s.saveAccess)))
	}
	s.mux.HandleFunc("DELETE /api/access", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "micast_go_access", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	s.mux.Handle("GET /api/status", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.Snapshot()) })))
	s.mux.Handle("POST /api/account/qr", s.authorize(http.HandlerFunc(s.startQR)))
	s.mux.Handle("GET /api/account/qr/{id}/image", s.authorize(http.HandlerFunc(s.qrImage)))
	s.mux.Handle("POST /api/account/qr/{id}/poll", s.authorize(http.HandlerFunc(s.pollQR)))
	s.mux.Handle("DELETE /api/account", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.routes != nil {
			s.routes.StopAll()
		}
		if s.engine != nil {
			s.engine.Stop("")
		}
		err := a.Logout()
		if s.routes != nil {
			_ = s.routes.Sync()
		}
		respond(w, err)
	})))
	s.mux.Handle("POST /api/devices/refresh", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := a.Refresh(r.Context(), true)
		if s.routes != nil {
			_ = s.routes.Sync()
		}
		respond(w, err)
	})))
	s.mux.Handle("DELETE /api/devices", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := s.app.ClearEntries()
		if err == nil && s.routes != nil {
			_ = s.routes.Sync()
		}
		respond(w, err)
	})))
	s.mux.Handle("PATCH /api/devices/{id}", s.authorize(http.HandlerFunc(s.speaker)))
	s.mux.Handle("GET /api/diagnostics", s.authorize(http.HandlerFunc(s.diagnostics)))
	s.mux.Handle("POST /api/diagnostics/export", s.authorize(http.HandlerFunc(s.exportDiagnostics)))
	s.mux.Handle("PUT /api/settings", s.authorize(http.HandlerFunc(s.settings)))
	s.mux.Handle("POST /api/control", s.authorize(http.HandlerFunc(s.control)))
	s.mux.Handle("GET /api/routes", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.routes == nil {
			writeJSON(w, 200, []receivers.Status{})
			return
		}
		writeJSON(w, 200, s.routes.Status())
	})))
	s.mux.Handle("GET /api/playback", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine := s.engine
		if s.routes != nil {
			var err error
			engine, err = s.routes.Engine(r.URL.Query().Get("route"))
			if err != nil {
				respond(w, err)
				return
			}
		}
		if engine == nil {
			writeJSON(w, 200, map[string]string{"state": "disabled"})
			return
		}
		writeJSON(w, 200, engine.Status())
	})))
	sub, _ := fs.Sub(files, "web")
	s.mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			validToken := s.accessControl != nil && sameKey(r.Header.Get("X-MiCast-CSRF"), s.accessControl.csrf)
			validBearer := r.Header.Get("Authorization") != "" && sameKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.key)
			if s.accessControl != nil && !validToken && !validBearer {
				writeJSON(w, 403, map[string]string{"error": "页面已更新，请刷新后重试"})
				return
			}
			if !validToken && !validBearer {
				if origin := r.Header.Get("Origin"); origin != "" {
					u, err := url.Parse(origin)
					validOrigin := err == nil && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && (u.Scheme == "http" || u.Scheme == "https")
					// A custom header requires a CORS preflight from another origin.
					// This server never grants CORS access. fnOS may rewrite Host,
					// so accept our same-origin UI header on its Unix socket only.
					gatewayRequest := len(options) > 0 && options[0].TrustedGateway && r.Header.Get("X-MiCast-Request") == "1" && (r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin")
					if !validOrigin || (u.Host != r.Host && !gatewayRequest) {
						writeJSON(w, 403, map[string]string{"error": "请求来源不匹配"})
						return
					}
				}
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					writeJSON(w, 403, map[string]string{"error": "拒绝跨站请求"})
					return
				}
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}
func sameKey(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.accessAllowed(r) {
			writeJSON(w, 401, map[string]string{"error": "请先输入管理密码"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("请求内容无效")
	}
	var rest any
	if err := d.Decode(&rest); err != io.EOF {
		return errors.New("请求内容无效")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func respond(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := decode(w, r, &body); err != nil {
		respond(w, err)
		return
	}
	hash := s.passwordHash
	if s.accessControl != nil {
		s.accessControl.mu.RLock()
		hash = s.accessControl.hash
		s.accessControl.mu.RUnlock()
	}
	passwordOK := hash != "" && store.VerifyAdminPassword(body.Key, hash)
	if !sameKey(body.Key, s.key) && !passwordOK {
		writeJSON(w, 401, map[string]string{"error": "管理密码不正确"})
		return
	}
	token := s.key
	if s.accessControl != nil {
		token = s.accessControl.token()
	}
	s.setAccessCookie(w, r, token)
	writeJSON(w, 200, map[string]any{"ok": true, "session": token})
}
func (s *Server) startQR(w http.ResponseWriter, r *http.Request) {
	qr, err := s.app.StartQR(r.Context())
	if err != nil {
		respond(w, err)
		return
	}
	writeJSON(w, 200, qr)
}
func (s *Server) qrImage(w http.ResponseWriter, r *http.Request) {
	raw, err := s.app.QRLoginURL(r.PathValue("id"))
	if err != nil {
		respond(w, err)
		return
	}
	png, err := qrcode.Encode(raw, qrcode.Medium, 320)
	if err != nil {
		respond(w, errors.New("二维码生成失败"))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}
func (s *Server) pollQR(w http.ResponseWriter, r *http.Request) {
	status, err := s.app.PollQR(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, err)
		return
	}
	if status == "waiting_limited" {
		writeJSON(w, 200, map[string]any{"status": "waiting", "retry_after": 30, "message": "小米暂时拒绝请求，稍后自动重试"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": status})
}
func (s *Server) speaker(w http.ResponseWriter, r *http.Request) {
	var p app.SpeakerPatch
	if err := decode(w, r, &p); err != nil {
		respond(w, err)
		return
	}
	err := s.app.UpdateSpeaker(r.Context(), r.PathValue("id"), p)
	if s.routes != nil {
		_ = s.routes.Sync()
	}
	respond(w, err)
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Mode       string           `json:"mode"`
		Selected   string           `json:"selected"`
		Name       string           `json:"name"`
		Format     string           `json:"format"`
		VolumeMode string           `json:"volume_mode"`
		Equalizer  *store.Equalizer `json:"equalizer"`
	}
	if err := decode(w, r, &p); err != nil {
		respond(w, err)
		return
	}
	if !store.ValidFormat(p.Format) {
		respond(w, errors.New("无效音频格式"))
		return
	}
	if p.Mode == "" {
		p.Mode = "whole_house"
	}
	err := s.app.UpdateSettings(r.Context(), p.Mode, p.Selected, p.Name, p.Format, app.SettingsOptions{VolumeMode: p.VolumeMode, Equalizer: p.Equalizer})
	if s.routes != nil {
		_ = s.routes.Sync()
	}
	respond(w, err)
}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Route   string `json:"route"`
		Action  string `json:"action"`
		URL     string `json:"url"`
		AudioID string `json:"audio_id"`
		Volume  int    `json:"volume"`
	}
	if err := decode(w, r, &p); err != nil {
		respond(w, err)
		return
	}
	switch p.Action {
	case "play_url":
		u, err := url.Parse(p.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			respond(w, errors.New("请提供音箱可访问的 HTTP 音频地址"))
			return
		}
	case "pause", "stop", "play":
	case "volume":
		if p.Volume < 0 || p.Volume > 100 {
			respond(w, errors.New("音量须为 0～100"))
			return
		}
	default:
		respond(w, errors.New("无效播放操作"))
		return
	}
	engine := s.engine
	if s.routes != nil {
		var err error
		engine, err = s.routes.Engine(p.Route)
		if err != nil {
			respond(w, err)
			return
		}
	}
	if engine != nil {
		var err error
		switch p.Action {
		case "play_url":
			seq := engine.Sequence()
			_, err = engine.StartFile(playback.StartRequest{Seq: seq, Protocol: "web", TrackID: "web-" + strconv.FormatUint(seq, 10), URI: p.URL, Continuation: false})
		case "pause":
			err = engine.Pause(r.Context())
		case "play":
			err = engine.Resume(r.Context())
		case "stop":
			engine.Stop("")
		case "volume":
			err = engine.Volume(r.Context(), p.Volume)
		}
		if err != nil {
			respond(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"results": map[string]string{}, "playback": engine.Status()})
		return
	}
	results, err := s.app.Control(r.Context(), p.Action, p.URL, p.AudioID, p.Volume)
	if err != nil {
		respond(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"results": results, "accepted_at": time.Now()})
}
