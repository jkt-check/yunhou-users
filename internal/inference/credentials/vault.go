// Package credentials owns upstream-credential secrets end to end:
// AEAD encryption at rest (key-versioned, rotation-friendly), the egress
// (SSRF) guard for upstream URLs, and the operator-facing credential
// lifecycle (create / rotate / test / disable) with audit attribution.
// Plaintext never crosses the HTTP boundary and never appears in logs,
// responses, or audit detail.
package credentials

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeyLen is the XChaCha20-Poly1305 key size (32 bytes). Go stdlib has no
// AEAD primitive of its own; x/crypto is already a module dependency
// (go.mod), so we use NewX's 24-byte random nonces instead of hand-rolling
// AES-GCM nonce bookkeeping.
const KeyLen = chacha20poly1305.KeySize

// nonceLen is the XChaCha20 nonce size; the nonce is prepended to the
// ciphertext so decryption needs no out-of-band nonce storage.
const nonceLen = chacha20poly1305.NonceSizeX

// Vault encrypts and decrypts upstream credential secrets. It holds every
// key version still needed for decryption; the highest version is the
// current encryption key. Rotation = deploy a new key version and re-encrypt
// credentials lazily (Rotate) or on demand; old versions stay until no
// ciphertext references them.
type Vault struct {
	keys    map[int][]byte
	current int
}

// NewVault builds a vault from versioned keys (at least one). current is the
// encryption version; it must exist in keys.
func NewVault(keys map[int][]byte, current int) (*Vault, error) {
	if len(keys) == 0 {
		return nil, errors.New("credentials vault: no keys configured")
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("credentials vault: current key version %d not in key set", current)
	}
	for v, k := range keys {
		if v <= 0 {
			return nil, fmt.Errorf("credentials vault: key version must be positive, got %d", v)
		}
		if len(k) != KeyLen {
			return nil, fmt.Errorf("credentials vault: key version %d must be %d bytes (hex %d chars)", v, KeyLen, KeyLen*2)
		}
	}
	return &Vault{keys: keys, current: current}, nil
}

// CurrentVersion returns the version new ciphertext is encrypted under.
func (v *Vault) CurrentVersion() int { return v.current }

// ParseKeysEnv parses the deployment-secret env format:
//
//	1:64hexchars,2:64hexchars
//
// Entries are version:hexkey pairs separated by commas. The highest version
// becomes the current encryption key. The plaintext key material comes from
// the deployment secret store (env / injected file) only — never from code,
// config files in the repo, or request input.
func ParseKeysEnv(s string) (map[int][]byte, int, error) {
	keys := map[int][]byte{}
	highest := 0
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		verStr, hexKey, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, 0, fmt.Errorf("credentials vault: key entry %q must be version:hex", entry)
		}
		ver, err := strconv.Atoi(strings.TrimPrefix(verStr, "v"))
		if err != nil || ver <= 0 {
			return nil, 0, fmt.Errorf("credentials vault: key entry %q has invalid version", entry)
		}
		key, err := hex.DecodeString(strings.TrimSpace(hexKey))
		if err != nil {
			return nil, 0, fmt.Errorf("credentials vault: key version %d is not hex: %v", ver, err)
		}
		if len(key) != KeyLen {
			return nil, 0, fmt.Errorf("credentials vault: key version %d must decode to %d bytes", ver, KeyLen)
		}
		if _, dup := keys[ver]; dup {
			return nil, 0, fmt.Errorf("credentials vault: duplicate key version %d", ver)
		}
		keys[ver] = key
		if ver > highest {
			highest = ver
		}
	}
	if len(keys) == 0 {
		return nil, 0, errors.New("credentials vault: empty key material")
	}
	return keys, highest, nil
}

// AAD binds a ciphertext to its credential ID and provider. Swapping
// ciphertext between rows (or between providers) fails authentication even
// with the correct key — 密文串换被拒绝.
func AAD(credentialID, providerID string) []byte {
	return []byte("inference-credential\x00" + credentialID + "\x00" + providerID)
}

// Encrypt seals plaintext under the current key version with AAD(id, provider).
// The returned blob is nonce || ciphertext and is safe to store in the DB.
func (v *Vault) Encrypt(credentialID, providerID string, plaintext []byte) ([]byte, int, error) {
	aead, err := chacha20poly1305.NewX(v.keys[v.current])
	if err != nil {
		return nil, 0, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, fmt.Errorf("credentials vault: nonce: %w", err)
	}
	out := aead.Seal(nonce, nonce, plaintext, AAD(credentialID, providerID))
	return out, v.current, nil
}

// Decrypt opens ciphertext stored under keyVersion. An unknown version, a
// swapped ciphertext (AAD mismatch), or a corrupted blob all fail closed
// with an error that carries no secret material.
func (v *Vault) Decrypt(credentialID, providerID string, keyVersion int, ciphertext []byte) ([]byte, error) {
	key, ok := v.keys[keyVersion]
	if !ok {
		return nil, fmt.Errorf("credentials vault: unknown key version %d", keyVersion)
	}
	if len(ciphertext) < nonceLen+chacha20poly1305.Overhead {
		return nil, errors.New("credentials vault: ciphertext too short")
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce, ct := ciphertext[:nonceLen], ciphertext[nonceLen:]
	plain, err := aead.Open(nil, nonce, ct, AAD(credentialID, providerID))
	if err != nil {
		// AEAD auth failure — wrong key, swapped ciphertext, or tampering.
		// The error string intentionally reveals nothing beyond that.
		return nil, errors.New("credentials vault: decrypt failed (authentication mismatch)")
	}
	return plain, nil
}
