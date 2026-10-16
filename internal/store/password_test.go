package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordStoredAsSaltedHash(t *testing.T) {
	dir := t.TempDir()
	password := "test-admin-password"
	if err := SetAdminPassword(dir, password); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "admin-password"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), password) || !VerifyAdminPassword(password, string(raw)) || VerifyAdminPassword("wrong-password", string(raw)) {
		t.Fatal("invalid password storage/verification")
	}
	if SetAdminPassword(dir, "short") == nil {
		t.Fatal("short password accepted")
	}
}
