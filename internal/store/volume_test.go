package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVolumeModeDefaultsMigrationAndValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"schema_version":2,"name":"Existing","mode":"whole_house","format":"mp3","speakers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().VolumeMode != "independent" {
		t.Fatal("old configuration did not adopt default")
	}
	if err = s.Update(func(c *Config) error { c.VolumeMode = "linked"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, err = NewConfig(dir)
	if err != nil || s.Snapshot().VolumeMode != "linked" {
		t.Fatal("mode did not persist", err)
	}
	if err = s.Update(func(c *Config) error { c.VolumeMode = "unknown"; return nil }); err == nil || s.Snapshot().VolumeMode != "linked" {
		t.Fatal("invalid mode accepted or changed settings")
	}
}
