package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"micastgo/internal/store"
)

// AccessControl is shared by the TCP listener and fnOS gateway. Missing/empty
// password means local management login is optional, including on first install.
type AccessControl struct {
	mu              sync.RWMutex
	dir, hash, csrf string
	key             []byte
}

func NewAccessControl(dir string, key []byte) (*AccessControl, error) {
	data, err := os.ReadFile(filepath.Join(dir, "admin-password"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	hash := strings.TrimSpace(string(data))
	if hash != "" && !strings.HasPrefix(hash, "pbkdf2-sha256:210000:") {
		return nil, errors.New("管理密码文件无效，请在本机重置")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &AccessControl{dir: dir, hash: hash, key: append([]byte(nil), key...), csrf: hex.EncodeToString(nonce)}, nil
}
func (a *AccessControl) required() bool { a.mu.RLock(); defer a.mu.RUnlock(); return a.hash != "" }
func (a *AccessControl) token() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte("management-session/" + a.hash))
	return hex.EncodeToString(mac.Sum(nil))
}
func requestKey(r *http.Request) string {
	if value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); value != "" {
		return value
	}
	if cookie, err := r.Cookie("micast_go_access"); err == nil {
		return cookie.Value
	}
	return ""
}
func (s *Server) accessAllowed(r *http.Request) bool {
	if s.accessControl != nil && !s.accessControl.required() {
		return true
	}
	key := requestKey(r)
	if sameKey(key, s.key) && (s.accessControl == nil || r.Header.Get("Authorization") == "Bearer "+s.key) {
		return true
	}
	return s.accessControl != nil && sameKey(key, s.accessControl.token())
}
func (s *Server) accessInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"required": s.accessControl.required(), "authenticated": s.accessAllowed(r), "csrf": s.accessControl.csrf})
}
func (s *Server) saveAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  bool   `json:"enabled"`
		Password string `json:"password"`
		Current  string `json:"current_password"`
	}
	if err := decode(w, r, &body); err != nil {
		respond(w, err)
		return
	}
	a := s.accessControl
	a.mu.Lock()
	if a.hash != "" && !store.VerifyAdminPassword(body.Current, a.hash) {
		a.mu.Unlock()
		respond(w, errors.New("当前管理密码不正确"))
		return
	}
	hash := ""
	if body.Enabled {
		if body.Password == "" && a.hash != "" {
			hash = a.hash
		} else {
			var err error
			hash, err = store.EncodeAdminPassword(body.Password)
			if err != nil {
				a.mu.Unlock()
				respond(w, err)
				return
			}
		}
	}
	if err := store.AtomicWrite(filepath.Join(a.dir, "admin-password"), []byte(hash)); err != nil {
		a.mu.Unlock()
		respond(w, err)
		return
	}
	a.hash = hash
	a.mu.Unlock()
	s.setAccessCookie(w, r, a.token())
	writeJSON(w, 200, map[string]any{"required": a.required(), "session": a.token()})
}
func (s *Server) setAccessCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{Name: "micast_go_access", Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, MaxAge: 86400})
}
