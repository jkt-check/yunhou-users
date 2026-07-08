package util

import (
	"strings"
	"testing"
	"time"
)

var testStateSecret = []byte("test-secret-32-bytes-long-12345678")

func TestIssueOAuthState_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tok, err := IssueOAuthState(testStateSecret, "yundian", 2, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	idx, err := VerifyOAuthState(testStateSecret, tok, "yundian", now.Add(time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if idx != 2 {
		t.Errorf("callback_index = %d, want 2", idx)
	}
}

func TestIssueOAuthState_Expiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tok, err := IssueOAuthState(testStateSecret, "yundian", 0, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Just past expiry: must fail.
	if _, err := VerifyOAuthState(testStateSecret, tok, "yundian", now.Add(StateExpiry+time.Second)); !errIs(err, ErrInvalidState) {
		t.Errorf("post-expiry verify err = %v, want ErrInvalidState", err)
	}
	// One second before expiry: must pass.
	if _, err := VerifyOAuthState(testStateSecret, tok, "yundian", now.Add(StateExpiry-time.Second)); err != nil {
		t.Errorf("just-before-expiry verify: %v", err)
	}
}

func TestIssueOAuthState_DistinctNonce(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	seen := make(map[string]struct{}, 16)
	for i := 0; i < 16; i++ {
		tok, err := IssueOAuthState(testStateSecret, "yundian", 0, now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("token collision at i=%d: %s", i, tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestVerifyOAuthState_WrongSecret(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tok, err := IssueOAuthState(testStateSecret, "yundian", 0, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	otherSecret := []byte("a-different-secret-of-the-same-length-")
	if _, err := VerifyOAuthState(otherSecret, tok, "yundian", now.Add(time.Second)); !errIs(err, ErrInvalidState) {
		t.Errorf("wrong-secret verify err = %v, want ErrInvalidState", err)
	}
}

func TestVerifyOAuthState_WrongAppID(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tok, err := IssueOAuthState(testStateSecret, "yundian", 0, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Same secret, but the callback handler passes a different appID —
	// simulating an attacker swapping the state onto another app's flow.
	if _, err := VerifyOAuthState(testStateSecret, tok, "yundash", now.Add(time.Second)); !errIs(err, ErrInvalidState) {
		t.Errorf("wrong-appID verify err = %v, want ErrInvalidState", err)
	}
}

func TestVerifyOAuthState_TamperedPayload(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tok, err := IssueOAuthState(testStateSecret, "yundian", 0, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Flip one character of the token — signature check must reject it.
	flipped := flipChar(tok)
	if flipped == tok {
		t.Fatal("flipChar did not change the token")
	}
	if _, err := VerifyOAuthState(testStateSecret, flipped, "yundian", now.Add(time.Second)); !errIs(err, ErrInvalidState) {
		t.Errorf("tampered verify err = %v, want ErrInvalidState", err)
	}
}

func TestVerifyOAuthState_BadBase64(t *testing.T) {
	t.Parallel()
	if _, err := VerifyOAuthState(testStateSecret, "!!!not-base64!!!", "yundian", time.Unix(1_700_000_000, 0)); !errIs(err, ErrInvalidState) {
		t.Errorf("bad base64 err = %v, want ErrInvalidState", err)
	}
}

func TestVerifyOAuthState_WrongLength(t *testing.T) {
	t.Parallel()
	// 60 bytes expected (8+4+16+32). Submit 30 bytes — signature mismatch
	// would fail too, but length check is the first gate and we want it
	// covered explicitly.
	short := strings.Repeat("a", 30)
	if _, err := VerifyOAuthState(testStateSecret, short, "yundian", time.Unix(1_700_000_000, 0)); !errIs(err, ErrInvalidState) {
		t.Errorf("short token err = %v, want ErrInvalidState", err)
	}
}

func TestVerifyOAuthState_EmptyToken(t *testing.T) {
	t.Parallel()
	if _, err := VerifyOAuthState(testStateSecret, "", "yundian", time.Unix(1_700_000_000, 0)); !errIs(err, ErrInvalidState) {
		t.Errorf("empty token err = %v, want ErrInvalidState", err)
	}
}

func TestIssueOAuthState_EmptySecret(t *testing.T) {
	t.Parallel()
	if _, err := IssueOAuthState(nil, "yundian", 0, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for empty secret")
	}
}

func TestIssueOAuthState_EmptyAppID(t *testing.T) {
	t.Parallel()
	if _, err := IssueOAuthState(testStateSecret, "", 0, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for empty appID")
	}
}

func TestIssueOAuthState_NegativeCallbackIndex(t *testing.T) {
	t.Parallel()
	if _, err := IssueOAuthState(testStateSecret, "yundian", -1, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for negative callback index")
	}
}

func TestIssueOAuthState_OverflowCallbackIndex(t *testing.T) {
	t.Parallel()
	if _, err := IssueOAuthState(testStateSecret, "yundian", 1<<32, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for overflowing callback index")
	}
}

func TestVerifyOAuthState_EmptySecret(t *testing.T) {
	t.Parallel()
	if _, err := VerifyOAuthState(nil, "irrelevant", "yundian", time.Unix(1_700_000_000, 0)); !errIs(err, ErrInvalidState) {
		t.Errorf("empty-secret verify err = %v, want ErrInvalidState", err)
	}
}

// flipChar returns the input with one base64url character replaced. Used to
// confirm any mutation of the token breaks the signature.
func flipChar(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func errIs(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}