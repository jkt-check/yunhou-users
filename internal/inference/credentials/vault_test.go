package credentials

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func testKeys(t *testing.T, versions ...int) map[int][]byte {
	t.Helper()
	keys := map[int][]byte{}
	for _, v := range versions {
		k := make([]byte, KeyLen)
		k[0] = byte(v)
		keys[v] = k
	}
	return keys
}

func TestVaultRoundtrip(t *testing.T) {
	v, err := NewVault(testKeys(t, 1), 1)
	if err != nil {
		t.Fatal(err)
	}
	ct, ver, err := v.Encrypt("cred-1", "prov-1", []byte("sk-live-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}
	plain, err := v.Decrypt("cred-1", "prov-1", 1, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "sk-live-secret" {
		t.Fatalf("roundtrip = %q", plain)
	}
	// The stored blob must not contain the plaintext.
	if bytes.Contains(ct, []byte("sk-live-secret")) {
		t.Fatal("ciphertext leaks plaintext")
	}
}

func TestVaultAADSwapRejected(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	ct, _, _ := v.Encrypt("cred-1", "prov-1", []byte("secret-a"))

	// Ciphertext moved to another credential row.
	if _, err := v.Decrypt("cred-2", "prov-1", 1, ct); err == nil {
		t.Fatal("decrypt with different credential ID must fail")
	}
	// Ciphertext moved to another provider.
	if _, err := v.Decrypt("cred-1", "prov-2", 1, ct); err == nil {
		t.Fatal("decrypt with different provider ID must fail")
	}
	// Bit flip in the ciphertext.
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := v.Decrypt("cred-1", "prov-1", 1, tampered); err == nil {
		t.Fatal("decrypt of tampered ciphertext must fail")
	}
}

func TestVaultUnknownKeyVersionRejected(t *testing.T) {
	v1, _ := NewVault(testKeys(t, 1), 1)
	v2, _ := NewVault(testKeys(t, 1, 2), 2)
	ct, ver, _ := v1.Encrypt("c", "p", []byte("old-secret"))
	if ver != 1 {
		t.Fatalf("version = %d", ver)
	}
	// v2 vault: old version still decrypts (rotation support).
	plain, err := v2.Decrypt("c", "p", 1, ct)
	if err != nil || string(plain) != "old-secret" {
		t.Fatalf("old-version decrypt: %v %q", err, plain)
	}
	// A vault that never held version 1 refuses.
	v3, _ := NewVault(testKeys(t, 2, 3), 3)
	if _, err := v3.Decrypt("c", "p", 1, ct); err == nil {
		t.Fatal("decrypt with unknown key version must fail")
	}
	// Re-encrypt under the new version.
	ct2, ver2, _ := v2.Encrypt("c", "p", []byte("new-secret"))
	if ver2 != 2 {
		t.Fatalf("new version = %d", ver2)
	}
	if plain, err := v2.Decrypt("c", "p", 2, ct2); err != nil || string(plain) != "new-secret" {
		t.Fatalf("new-version decrypt: %v", err)
	}
}

func TestParseKeysEnv(t *testing.T) {
	k1 := strings.Repeat("11", KeyLen)
	k2 := strings.Repeat("22", KeyLen)
	keys, current, err := ParseKeysEnv("1:" + k1 + ", 2:" + k2)
	if err != nil {
		t.Fatal(err)
	}
	if current != 2 || len(keys) != 2 {
		t.Fatalf("current=%d len=%d", current, len(keys))
	}
	for _, bad := range []string{
		"",
		"abc",
		"1:zz" + k1[4:],
		"0:" + k1,
		"1:" + k1 + ",1:" + k1,
		"1:" + k1[:len(k1)-2], // short key
		"x:" + k1,
	} {
		if _, _, err := ParseKeysEnv(bad); err == nil {
			t.Errorf("ParseKeysEnv(%q) must fail", bad)
		}
	}
}

func TestParseKeysEnvRoundTrip(t *testing.T) {
	raw := hex.EncodeToString(bytes.Repeat([]byte{7}, KeyLen))
	keys, current, err := ParseKeysEnv("5:" + raw)
	if err != nil {
		t.Fatal(err)
	}
	if current != 5 {
		t.Fatalf("current = %d", current)
	}
	if _, err := NewVault(keys, current); err != nil {
		t.Fatal(err)
	}
}

func TestNewVaultRejectsBadInput(t *testing.T) {
	if _, err := NewVault(map[int][]byte{}, 1); err == nil {
		t.Fatal("empty keys must fail")
	}
	keys := testKeys(t, 1)
	if _, err := NewVault(keys, 2); err == nil {
		t.Fatal("current version missing from keys must fail")
	}
	keys[2] = make([]byte, 16)
	if _, err := NewVault(keys, 2); err == nil {
		t.Fatal("short key must fail")
	}
	badVer := testKeys(t, 1)
	badVer[-1] = make([]byte, KeyLen)
	if _, err := NewVault(badVer, 1); err == nil {
		t.Fatal("non-positive version must fail")
	}
}

// TestParseKeysEnvErrorsRedactKeyMaterial: a malformed paste of REAL key
// material must not echo the key bytes into the error string — the error
// travels to the startup log via cmd/server log.Fatalf (Task 4 review
// fix #2). Errors may name the entry index or the version segment (the part
// before the colon, never key material) and nothing else.
func TestParseKeysEnvErrorsRedactKeyMaterial(t *testing.T) {
	material := strings.Repeat("ab1c", 16) // 64 hex chars standing in for a real key

	cases := []struct {
		name string
		in   string
	}{
		{"no colon echoes nothing", material},
		{"bad version echoes version segment only", "abc:" + material},
		{"odd length hex echoes nothing", "1:" + material + "0"},
		{"invalid hex byte echoes nothing", "1:zz" + material[:62]},
		{"bad second entry echoes nothing", "1:" + material + ",2:" + material + "0"},
	}
	for _, tc := range cases {
		_, _, err := ParseKeysEnv(tc.in)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if strings.Contains(err.Error(), material) {
			t.Fatalf("%s: error leaks key material: %v", tc.name, err)
		}
	}

	// The version segment (before the colon) is not key material and may be
	// quoted to help the operator locate the typo.
	_, _, err := ParseKeysEnv("abc:" + material)
	if !strings.Contains(err.Error(), `"abc"`) {
		t.Fatalf("invalid-version error should quote the version segment: %v", err)
	}
}
