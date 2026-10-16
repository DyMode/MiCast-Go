package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"golang.org/x/crypto/pbkdf2"
	"path/filepath"
	"strings"
)

func SetAdminPassword(dir, password string) error {
	encoded, err := EncodeAdminPassword(password)
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(dir, "admin-password"), []byte(encoded))
}
func EncodeAdminPassword(password string) (string, error) {
	if len(password) < 8 || len(password) > 128 {
		return "", errors.New("管理密码须为 8～128 字节")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := pbkdf2.Key([]byte(password), salt, 210000, 32, sha256.New)
	encoded := "pbkdf2-sha256:210000:" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash)
	return encoded, nil
}
func VerifyAdminPassword(password, encoded string) bool {
	parts := strings.Split(strings.TrimSpace(encoded), ":")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "210000" || len(password) > 128 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) != 32 {
		return false
	}
	got := pbkdf2.Key([]byte(password), salt, 210000, 32, sha256.New)
	return subtle.ConstantTimeCompare(got, want) == 1
}
