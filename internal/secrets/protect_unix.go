//go:build !windows

package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"os"
)

// Unix credential protection — DEVELOPMENT PLATFORM ONLY.
//
// DeBrowser ships for Windows, where the protector is DPAPI. Unix
// builds (developer machines, this repo's Linux tests) use
// AES-256-GCM with a random key kept next to the vault in
// <vault>.key (mode 0600, tightened through RestrictFile). This
// protects against accidental disclosure of the vault file alone,
// but the key sits beside the data, so it is NOT equivalent to
// DPAPI's user binding and must never be described as such. It
// exists so the vault logic, tests and developer workflows run
// identically off Windows.

type gcmProtector struct {
	gcm cipher.AEAD
}

func platformProtector(vaultPath string) (Protector, error) {
	keyPath := vaultPath + ".key"
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("secrets: read key file: %w", err)
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("secrets: generate key: %w", err)
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, fmt.Errorf("secrets: write key file: %w", err)
		}
	}
	if err := RestrictFile(keyPath); err != nil {
		return nil, fmt.Errorf("secrets: restrict key file: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key file has %d bytes, expected 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: init gcm: %w", err)
	}
	return gcmProtector{gcm: gcm}, nil
}

func (p gcmProtector) Protect(plain []byte) ([]byte, error) {
	nonce := make([]byte, p.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return p.gcm.Seal(nonce, nonce, plain, nil), nil
}

func (p gcmProtector) Unprotect(blob []byte) ([]byte, error) {
	ns := p.gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("secrets: blob shorter than nonce")
	}
	plain, err := p.gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm open: %w", err)
	}
	return plain, nil
}
