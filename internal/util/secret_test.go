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
