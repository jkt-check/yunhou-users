package util

import (
	"strings"
	"testing"
)

func TestHashSecret(t *testing.T) {
	t.Parallel()

	t.Run("returns non-empty hash for non-empty input", func(t *testing.T) {
		t.Parallel()
		h, err := HashSecret("mypassword")
		if err != nil {
			t.Fatalf("HashSecret: %v", err)
		}
		if h == "" {
			t.Fatal("hash is empty")
		}
		if h == "mypassword" {
			t.Fatal("hash equals the plain secret")
		}
	})

	t.Run("different calls produce different hashes (bcrypt salt)", func(t *testing.T) {
		t.Parallel()
		h1, err := HashSecret("same-secret")
		if err != nil {
			t.Fatalf("HashSecret 1: %v", err)
		}
		h2, err := HashSecret("same-secret")
		if err != nil {
			t.Fatalf("HashSecret 2: %v", err)
		}
		if h1 == h2 {
			t.Fatal("two hashes of the same secret should differ due to bcrypt salt")
		}
	})

	t.Run("empty string input", func(t *testing.T) {
		t.Parallel()
		h, err := HashSecret("")
		if err != nil {
			t.Fatalf("HashSecret empty: %v", err)
		}
		if h == "" {
			t.Fatal("hash of empty string should not be empty")
		}
	})

	t.Run("input exceeding bcrypt max length returns error", func(t *testing.T) {
		t.Parallel()
		longSecret := strings.Repeat("a", 73) // bcrypt max is 72 bytes
		h, err := HashSecret(longSecret)
		if err == nil {
			t.Fatal("expected error for input exceeding bcrypt max length, got nil")
		}
		if h != "" {
			t.Errorf("expected empty hash on error, got %q", h)
		}
	})
}

func TestCheckSecret(t *testing.T) {
	t.Parallel()

	t.Run("matching secret returns true", func(t *testing.T) {
		t.Parallel()
		plain := "correct-horse-battery-staple"
		hashed, err := HashSecret(plain)
		if err != nil {
			t.Fatalf("HashSecret: %v", err)
		}
		if !CheckSecret(hashed, plain) {
			t.Fatal("CheckSecret should return true for matching secret")
		}
	})

	t.Run("non-matching secret returns false", func(t *testing.T) {
		t.Parallel()
		plain := "correct-horse-battery-staple"
		hashed, err := HashSecret(plain)
		if err != nil {
			t.Fatalf("HashSecret: %v", err)
		}
		if CheckSecret(hashed, "wrong-secret") {
			t.Fatal("CheckSecret should return false for non-matching secret")
		}
	})

	t.Run("empty plain against hashed empty returns true", func(t *testing.T) {
		t.Parallel()
		hashed, err := HashSecret("")
		if err != nil {
			t.Fatalf("HashSecret: %v", err)
		}
		if !CheckSecret(hashed, "") {
			t.Fatal("CheckSecret should return true when both are empty")
		}
	})

	t.Run("empty plain against non-empty hash returns false", func(t *testing.T) {
		t.Parallel()
		hashed, err := HashSecret("nonempty")
		if err != nil {
			t.Fatalf("HashSecret: %v", err)
		}
		if CheckSecret(hashed, "") {
			t.Fatal("CheckSecret should return false for empty plain against non-empty hash")
		}
	})

	t.Run("invalid hash format returns false", func(t *testing.T) {
		t.Parallel()
		if CheckSecret("not-a-bcrypt-hash", "anything") {
			t.Fatal("CheckSecret should return false for invalid hash format")
		}
	})
}

// TestDummyBcryptHash_Populated covers the package-level init() which
// generates a constant-time bcrypt hash for use as a sentinel in user-lookup
// paths (defends against timing-attack user enumeration). The hash is set
// once at process start; this test asserts it was populated.
func TestDummyBcryptHash_Populated(t *testing.T) {
	t.Parallel()
	if DummyBcryptHash == "" {
		t.Fatal("DummyBcryptHash should be populated by init()")
	}
	// Even though we don't know the plain, we should be able to call
	// CheckSecret with a wrong value and get false — confirms it's a valid
	// bcrypt hash, not gibberish.
	if CheckSecret(DummyBcryptHash, "not-the-dummy") {
		t.Fatal("DummyBcryptHash should not match a wrong plain value")
	}
}

func TestGenerateSecret(t *testing.T) {
	t.Parallel()

	t.Run("returns non-empty plaintext and hash", func(t *testing.T) {
		t.Parallel()
		plain, hash, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		if plain == "" {
			t.Error("plaintext is empty")
		}
		if hash == "" {
			t.Error("hash is empty")
		}
		if plain == hash {
			t.Error("plaintext equals hash")
		}
		// 32 bytes → 64 hex chars
		if len(plain) != 64 {
			t.Errorf("plaintext length: got %d, want 64", len(plain))
		}
		// hash should be a valid bcrypt hash
		if !CheckSecret(hash, plain) {
			t.Error("hash does not verify the plaintext")
		}
	})

	t.Run("produces unique plaintexts across calls", func(t *testing.T) {
		t.Parallel()
		seen := make(map[string]bool)
		for i := 0; i < 50; i++ {
			plain, _, err := GenerateSecret()
			if err != nil {
				t.Fatalf("GenerateSecret: %v", err)
			}
			if seen[plain] {
				t.Errorf("duplicate plaintext on iter %d: %s", i, plain)
			}
			seen[plain] = true
		}
	})
}

func TestDummyBcryptHash(t *testing.T) {
	t.Parallel()
	// DummyBcryptHash should be a valid bcrypt hash that does NOT match
	// common test secrets. Used to make CheckSecret take constant time
	// even for non-existent app IDs.
	if DummyBcryptHash == "" {
		t.Fatal("DummyBcryptHash is empty")
	}
	// It must verify against its own dummy value.
	if !CheckSecret(DummyBcryptHash, "dummy-timing-mitigation-value") {
		t.Error("DummyBcryptHash does not match the dummy plaintext")
	}
	// It must NOT verify against an attacker-chosen guess.
	if CheckSecret(DummyBcryptHash, "guess") {
		t.Error("DummyBcryptHash incorrectly matches 'guess'")
	}
	if CheckSecret(DummyBcryptHash, "") {
		t.Error("DummyBcryptHash incorrectly matches empty string")
	}
	// Cheap heuristic: ensure the hash isn't obviously the plaintext.
	if !strings.HasPrefix(DummyBcryptHash, "$2") {
		t.Errorf("DummyBcryptHash doesn't look like a bcrypt hash: %q", DummyBcryptHash)
	}
}
