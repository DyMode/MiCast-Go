package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultAuthenticatedPersistence(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret := map[string]string{"pass_token": "private-pass-token"}
	if err = v.Save(secret); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "account.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret["pass_token"]) {
		t.Fatal("plaintext credential on disk")
	}
	restarted, err := NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err = restarted.Load(&got); err != nil || got["pass_token"] != secret["pass_token"] {
		t.Fatal("credential restart failed", err)
	}
	raw[len(raw)-1] ^= 1
	if err = os.WriteFile(v.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if v.Load(&got) == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
func TestMissingKeyDoesNotReplaceCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "account.enc"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVault(dir); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "account.key")); !os.IsNotExist(err) {
		t.Fatal("replacement key created")
	}
}
func TestSpeakerSwitchesSurviveRediscoveryAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewConfig(dir)
	err := s.Update(func(c *Config) error {
		c.Speakers["one"] = Speaker{ID: "one", Joined: true, WholeHouse: false, Volume: 30, Format: "mp3"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s2.Snapshot()
	if !c.Speakers["one"].Joined || c.Speakers["one"].WholeHouse || len(c.Targets()) != 0 {
		t.Fatal("switches lost")
	}
	c.Mode = "single"
	c.Selected = "one"
	if len(c.Targets()) != 1 {
		t.Fatal("whole-house switch excluded single target")
	}
	c.Speakers["one"] = Speaker{ID: "one", Joined: false, WholeHouse: true, Format: "mp3"}
	if len(c.Targets()) != 0 {
		t.Fatal("unjoined target selected")
	}
}
func TestInvalidUpdateDoesNotChangeMemoryOrDisk(t *testing.T) {
	s, _ := NewConfig(t.TempDir())
	err := s.Update(func(c *Config) error { c.Mode = "broken"; return nil })
	if err == nil || s.Snapshot().Mode != "whole_house" {
		t.Fatal("invalid update committed")
	}
	if _, err = os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("invalid config written")
	}
}

func TestOldWholeSwitchIsIgnoredAndMembersPersist(t *testing.T) {
	dir := t.TempDir()
	raw := `{"name":"全屋","mode":"whole_house","whole_enabled":false,"format":"mp3","speakers":{"one":{"id":"one","joined":true,"whole_house":true,"format":"auto","volume":20}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Snapshot().RouteTargets("whole_house")) != 1 {
		t.Fatal("old global switch blocks membership-driven whole house")
	}
	if err := c.Update(func(c *Config) error { s := c.Speakers["one"]; s.WholeHouse = false; c.Speakers["one"] = s; return nil }); err != nil {
		t.Fatal(err)
	}
	restored, err := NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot().RouteTargets("whole_house")) != 0 || len(restored.Snapshot().RouteTargets("speaker:one")) != 1 {
		t.Fatal("member switch failed to persist independently")
	}
}
