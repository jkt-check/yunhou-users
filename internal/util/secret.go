package util

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

var DummyBcryptHash string

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-timing-mitigation-value"), bcrypt.DefaultCost)
	if err != nil {
		panic("util: failed to generate dummy bcrypt hash: " + err.Error())
	}
	DummyBcryptHash = string(h)
}

func HashSecret(secret string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func CheckSecret(hashed, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)) == nil
}

// GenerateSecret produces a fresh (plaintext, hash) pair for use as an app
// shared secret. 32 random bytes from crypto/rand → 64-char hex. bcrypt
// hashing uses DefaultCost to slow down brute force if the DB leaks.
//
// The plaintext is returned alongside the hash so the caller can hand it to
// the human/admin exactly once (CreateApp response, RotateSecret response).
// After this call only the hash should be persisted.
func GenerateSecret() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plaintext = hex.EncodeToString(b)
	h, err := HashSecret(plaintext)
	if err != nil {
		return "", "", err
	}
	return plaintext, h, nil
}
