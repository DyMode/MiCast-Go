// Package store persists configuration and authenticated encrypted credentials.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".micast-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp, path)
}

// LoadOrCreateKey refuses to replace an existing malformed key.
func LoadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid key length")
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	b = make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreateKey(path)
	}
	if err != nil {
		return nil, err
	}
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return b, nil
}

type Vault struct {
	path string
	aead cipher.AEAD
}

func NewVault(dir string) (*Vault, error) {
	// Never create a replacement key alongside existing encrypted credentials.
	if _, err := os.Stat(filepath.Join(dir, "account.enc")); err == nil {
		if _, err = os.Stat(filepath.Join(dir, "account.key")); err != nil {
			return nil, fmt.Errorf("account key missing: %w", err)
		}
	}
	key, err := LoadOrCreateKey(filepath.Join(dir, "account.key"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{filepath.Join(dir, "account.enc"), aead}, nil
}
func (v *Vault) Save(value any) error {
	plain, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	return AtomicWrite(v.path, v.aead.Seal(nonce, nonce, plain, []byte("micast-go/account/v1")))
}
func (v *Vault) Load(value any) error {
	data, err := os.ReadFile(v.path)
	if err != nil {
		return err
	}
	n := v.aead.NonceSize()
	if len(data) < n {
		return errors.New("invalid encrypted account")
	}
	plain, err := v.aead.Open(nil, data[:n], data[n:], []byte("micast-go/account/v1"))
	if err != nil {
		return errors.New("account decryption failed")
	}
	return json.Unmarshal(plain, value)
}
func (v *Vault) Clear() error {
	for _, path := range []string{v.path, filepath.Join(filepath.Dir(v.path), "upgrade-backup", "account.enc"), filepath.Join(filepath.Dir(v.path), "upgrade-backup", "account.key")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type Speaker struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hardware   string `json:"hardware"`
	Joined     bool   `json:"joined"`
	WholeHouse bool   `json:"whole_house"`
	Volume     int    `json:"volume"`
	DelayMS    int    `json:"delay_ms"`
	Format     string `json:"format"`
	Muted      bool   `json:"muted"`
}
type Config struct {
	SchemaVersion int                `json:"schema_version,omitempty"`
	Owner         string             `json:"owner,omitempty"`
	Name          string             `json:"name"`
	Mode          string             `json:"mode"`
	Selected      string             `json:"selected"`
	Format        string             `json:"format"`
	Speakers      map[string]Speaker `json:"speakers"`
}
type ConfigStore struct {
	mu    sync.RWMutex
	path  string
	value Config
}

func NewConfig(dir string) (*ConfigStore, error) {
	s := &ConfigStore{path: filepath.Join(dir, "config.json"), value: Config{SchemaVersion: 2, Name: "MiCast Go", Mode: "whole_house", Format: "mp3", Speakers: map[string]Speaker{}}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		s.value.SchemaVersion = 0
		err = json.Unmarshal(b, &s.value)
		if s.value.SchemaVersion < 2 {
			if s.value.Name == "全屋" {
				s.value.Name = "MiCast Go"
			}
			s.value.SchemaVersion = 2
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.value.Speakers == nil {
		s.value.Speakers = map[string]Speaker{}
	}
	if err = validate(s.value); err != nil {
		return nil, err
	}
	return s, nil
}
func clone(c Config) Config {
	m := make(map[string]Speaker, len(c.Speakers))
	for k, v := range c.Speakers {
		m[k] = v
	}
	c.Speakers = m
	return c
}
func (s *ConfigStore) Snapshot() Config { s.mu.RLock(); defer s.mu.RUnlock(); return clone(s.value) }
func (s *ConfigStore) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.value)
	if err := fn(&next); err != nil {
		return err
	}
	if err := validate(next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = AtomicWrite(s.path, b); err != nil {
		return err
	}
	s.value = next
	return nil
}
func validate(c Config) error {
	if len(c.Name) == 0 || len(c.Name) > 100 {
		return errors.New("名称长度须为 1～100 字节")
	}
	if c.Mode != "single" && c.Mode != "whole_house" {
		return errors.New("无效播放模式")
	}
	if !ValidFormat(c.Format) {
		return errors.New("无效音频格式")
	}
	for id, s := range c.Speakers {
		if id == "" || s.ID != id || s.Volume < 0 || s.Volume > 100 || s.DelayMS < 0 || s.DelayMS > 5000 || (!ValidFormat(s.Format) && s.Format != "auto") {
			return errors.New("无效音箱配置")
		}
	}
	return nil
}
func ValidFormat(f string) bool { return f == "mp3" || f == "flac" || f == "wav" || f == "pcm" }
func (c Config) Targets() []Speaker {
	result := []Speaker{}
	for _, s := range c.Speakers {
		if s.Format == "auto" {
			s.Format = c.Format
		}
		if s.Joined && ((c.Mode == "whole_house" && s.WholeHouse) || (c.Mode == "single" && s.ID == c.Selected)) {
			result = append(result, s)
		}
	}
	return result
}

// RouteTargets keeps every speaker endpoint independent from whole-house membership.
// An empty route retains the legacy API selection for older clients.
func (c Config) RouteTargets(route string) []Speaker {
	if route == "" {
		return c.Targets()
	}
	result := []Speaker{}
	for _, s := range c.Speakers {
		if !s.Joined || (route == "whole_house" && !s.WholeHouse) || (route != "whole_house" && route != "speaker:"+s.ID) {
			continue
		}
		if s.Format == "auto" {
			s.Format = c.Format
		}
		result = append(result, s)
	}
	return result
}
