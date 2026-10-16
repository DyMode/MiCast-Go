package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeRestoresAccountPairAndKeepsNewerConfig(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Save(map[string]string{"user_id": "saved-user", "service_token": "saved-token"}); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(dir, "config.json"), []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := SetAdminPassword(dir, "saved-password"); err != nil {
		t.Fatal(err)
	}
	if err := BackupUpgrade(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"account.key", "account.enc", "admin-password"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := AtomicWrite(filepath.Join(dir, "config.json"), []byte("newer")); err != nil {
		t.Fatal(err)
	}
	if err := RestoreUpgrade(dir); err != nil {
		t.Fatal(err)
	}
	v, err = NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	if err := v.Load(&saved); err != nil || saved["user_id"] != "saved-user" {
		t.Fatal(saved, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(data) != "newer" {
		t.Fatal("newer configuration overwritten")
	}
	data, _ = os.ReadFile(filepath.Join(dir, "admin-password"))
	if !VerifyAdminPassword("saved-password", string(data)) {
		t.Fatal("password was not retained")
	}
	if err := v.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := RestoreUpgrade(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "account.enc")); !os.IsNotExist(err) {
		t.Fatal("logout resurrected backup login")
	}
}
func TestUpgradeRefusesMismatchedCredentialKeys(t *testing.T) {
	dir := t.TempDir()
	v, _ := NewVault(dir)
	_ = v.Save(map[string]string{"user_id": "old"})
	if err := BackupUpgrade(dir); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "account.enc"))
	_ = os.Remove(filepath.Join(dir, "account.key"))
	_, err := NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreUpgrade(dir); err == nil {
		t.Fatal("restored account using unrelated new key")
	}
	if _, err := os.Stat(filepath.Join(dir, "account.enc")); !os.IsNotExist(err) {
		t.Fatal("mismatched credentials published")
	}
}

func TestUpgradeRefusesNonDirectoryBackup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "upgrade-backup"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := BackupUpgrade(dir); err == nil {
		t.Fatal("accepted non-directory backup")
	}
	if err := RestoreUpgrade(dir); err == nil {
		t.Fatal("accepted non-directory restore")
	}
}
