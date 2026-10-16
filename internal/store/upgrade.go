package store

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"os"
	"path/filepath"
)

var upgradeFiles = []string{"account.key", "account.enc", "access.key", "admin-password", "config.json", "advertise-ip"}

func privateRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 10*1024*1024 {
		return nil, errors.New("升级配置文件不是有效的普通文件")
	}
	return os.ReadFile(path)
}
func validateAccountPair(account, key []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(account) < aead.NonceSize() {
		return errors.New("米家凭证不完整")
	}
	_, err = aead.Open(nil, account[:aead.NonceSize()], account[aead.NonceSize():], []byte("micast-go/account/v1"))
	return err
}
func upgradePath(dir string) (string, error) {
	backup := filepath.Join(dir, "upgrade-backup")
	info, err := os.Lstat(backup)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && !info.IsDir() {
		return "", errors.New("升级备份路径不是普通目录")
	}
	return backup, nil
}
func BackupUpgrade(dir string) error {
	backup, err := upgradePath(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(backup, 0700); err != nil {
		return err
	}
	for _, name := range upgradeFiles {
		data, err := privateRead(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(filepath.Join(backup, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := AtomicWrite(filepath.Join(backup, name), data); err != nil {
			return err
		}
	}
	account, err := privateRead(filepath.Join(backup, "account.enc"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	key, err := privateRead(filepath.Join(backup, "account.key"))
	if err != nil {
		return err
	}
	return validateAccountPair(account, key)
}
func RestoreUpgrade(dir string) error {
	backup, err := upgradePath(dir)
	if err != nil {
		return err
	}
	// Validate the selected pair before restoring either half; never mix keys
	// belonging to different account snapshots or replace a newer credential.
	account, ae := privateRead(filepath.Join(dir, "account.enc"))
	key, ke := privateRead(filepath.Join(dir, "account.key"))
	if ae != nil && !errors.Is(ae, os.ErrNotExist) {
		return ae
	}
	if ke != nil && !errors.Is(ke, os.ErrNotExist) {
		return ke
	}
	if ae != nil {
		account, ae = privateRead(filepath.Join(backup, "account.enc"))
	}
	if ke != nil {
		key, ke = privateRead(filepath.Join(backup, "account.key"))
	}
	if ae == nil {
		if ke != nil {
			return ke
		}
		if err := validateAccountPair(account, key); err != nil {
			return err
		}
	}
	for _, name := range upgradeFiles {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := privateRead(filepath.Join(backup, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := AtomicWrite(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}
