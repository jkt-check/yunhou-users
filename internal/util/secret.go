package util

import "golang.org/x/crypto/bcrypt"

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
