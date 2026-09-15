package service

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	testRelaySecret     = "test-secret-current-0123456789abcdef"
	testRelaySecretPrev = "test-secret-previous-0123456789abcdef"
)

func TestRelayTicketIssueVerifyRoundTrip(t *testing.T) {
	svc := NewRelayTicketService(testRelaySecret, "", 300*time.Second)
	tok, expiresIn, err := svc.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if expiresIn != 300 {
		t.Fatalf("expiresIn = %d, want 300", expiresIn)
	}
	userID, exp, err := svc.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if userID != "user-1" {
		t.Fatalf("userID = %q, want user-1", userID)
	}
	if d := time.Until(exp); d < 250*time.Second || d > 330*time.Second {
		t.Fatalf("exp off: %v", d)
	}
}

func TestRelayTicketVerifyRejects(t *testing.T) {
	svc := NewRelayTicketService(testRelaySecret, testRelaySecretPrev, 300*time.Second)
	good, _, _ := svc.Issue("user-1")

	cases := map[string]string{
		"空 token":       "",
		"非 JWT":        "not-a-jwt",
		"错误 secret 签名": mustSignRelayTicket(t, "wrong-secret", "user-1", time.Now().Add(300*time.Second)),
		"已过期(超宽限)": mustSignRelayTicket(t, testRelaySecret, "user-1", time.Now().Add(-time.Minute)),
		"错误 aud":      mustSignRelayTicketAud(t, testRelaySecret, "user-1", "chat"),
	}
	for name, tok := range cases {
		if _, _, err := svc.Verify(tok); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
	// 篡改 payload 段
	tampered := good[:len(good)-4] + "AAAA"
	if _, _, err := svc.Verify(tampered); err == nil {
		t.Fatalf("篡改: expected error, got nil")
	}
}

func TestRelayTicketSecretRotation(t *testing.T) {
	old := NewRelayTicketService(testRelaySecretPrev, "", 300*time.Second)
	tok, _, err := old.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue with old secret: %v", err)
	}
	// 新实例:当前 secret 已轮换,旧 secret 仍应通过
	rotated := NewRelayTicketService(testRelaySecret, testRelaySecretPrev, 300*time.Second)
	if _, _, err := rotated.Verify(tok); err != nil {
		t.Fatalf("prev secret should verify: %v", err)
	}
}

func mustSignRelayTicket(t *testing.T, secret, userID string, exp time.Time) string {
	t.Helper()
	return mustSignRelayTicketAud(t, secret, userID, "relay", exp)
}

func mustSignRelayTicketAud(t *testing.T, secret, userID, aud string, exp ...time.Time) string {
	t.Helper()
	e := time.Now().Add(300 * time.Second)
	if len(exp) > 0 {
		e = exp[0]
	}
	claims := jwt.RegisteredClaims{
		Issuer:    "yunhou-users",
		Audience:  jwt.ClaimStrings{aud},
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(e),
		ID:        uuid.New().String(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}
