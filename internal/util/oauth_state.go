// Package util — OAuth state token helpers.
//
// OAuth state tokens defend the /auth/github/callback flow against:
//   - CSRF (attacker swaps their own code into our flow): the state is HMAC-signed
//     so a third party cannot forge one without the server-side secret.
//   - replay (attacker re-uses a previously observed state): the token embeds an
//     expiry; VerifyState rejects tokens whose expiry has passed.
//   - open redirect (attacker submits redirect_uri=evil.com): the token binds
//     callback_index — the index of the chosen callback URL inside the app's
//     configured callback_urls array — so the callback handler can verify the
//     redirect_uri exactly matches the array entry the state was issued for.
package util

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

// StateExpiry is how long an issued state remains valid. 5 minutes is enough
// for a human to complete GitHub's consent screen but short enough that a
// leaked state token expires before an attacker can plausibly replay it.
const StateExpiry = 5 * time.Minute

// State payload. The wire format is:
//
//	base64url( expiry(8) || callback_index(4) || nonce(16) || hmac(32) )
//
// where hmac = HMAC-SHA256(secret, expiry || callback_index || nonce || app_id).
//
// The server does not need to keep any per-state storage: signing + expiry +
// callback-index binding all live inside the token. Multiple yunhou-users
// instances share the same secret and therefore all accept the same state.
type statePayload struct {
	expiry        int64  // unix seconds, big-endian
	callbackIndex uint32 // big-endian; must be a valid index into apps.config.oauth_providers.github.callback_urls
	nonce         []byte // 16 random bytes
}

// ErrInvalidState is returned by VerifyState for any decode / signature /
// expiry / format failure. We intentionally do not distinguish the reasons:
// telling an attacker "your nonce was bad" vs "your expiry passed" only helps
// them iterate.
var ErrInvalidState = errors.New("invalid oauth state")

// IssueOAuthState returns a state token bound to appID and callbackIndex
// (the index into apps.config.oauth_providers.github.callback_urls the caller
// intends to redirect to after GitHub's callback). secret is the server-side
// HMAC key (OAUTH_STATE_SECRET). now is injectable for tests.
//
// Empty secret and out-of-range callback_index both surface as ErrInvalidState
// so callers can use errors.Is uniformly across the Issue/Verify pair. The
// config layer's Validate() rejects empty secrets up front, so reaching this
// path implies a programming error — the sentinel lets tests and admin code
// distinguish "bad input" from "everything else" without parsing messages.
func IssueOAuthState(secret []byte, appID string, callbackIndex int, now time.Time) (string, error) {
	if len(secret) == 0 {
		return "", ErrInvalidState
	}
	if appID == "" {
		return "", ErrInvalidState
	}
	if callbackIndex < 0 || callbackIndex > 0xFFFFFFFF {
		return "", ErrInvalidState
	}

	nonce := make([]byte, 16)
	// crypto/rand is the only acceptable source — math/rand would let an
	// attacker predict the nonce and forge a valid state with their own
	// callback_index.
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	expiry := now.Add(StateExpiry).Unix()
	payload := encodeState(expiry, uint32(callbackIndex), nonce)
	sig := signState(secret, payload, appID)

	out := make([]byte, 0, len(payload)+sha256.Size)
	out = append(out, payload...)
	out = append(out, sig...)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// VerifyOAuthState parses a state token previously produced by
// IssueOAuthState. On success it returns the bound appID and callbackIndex.
// On any failure (bad base64, wrong length, signature mismatch, expiry
// passed) it returns ErrInvalidState.
func VerifyOAuthState(secret []byte, token, expectedAppID string, now time.Time) (callbackIndex int, err error) {
	if len(secret) == 0 {
		return 0, ErrInvalidState
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return 0, ErrInvalidState
	}
	// 8 (expiry) + 4 (callback_index) + 16 (nonce) + 32 (sha256) = 60
	const want = 8 + 4 + 16 + sha256.Size
	if len(raw) != want {
		return 0, ErrInvalidState
	}

	payload := raw[:len(raw)-sha256.Size]
	gotSig := raw[len(raw)-sha256.Size:]
	wantSig := signState(secret, payload, expectedAppID)
	// hmac.Equal is constant-time. Compare both 32-byte slices.
	if !hmac.Equal(gotSig, wantSig) {
		return 0, ErrInvalidState
	}

	expiry := int64(binary.BigEndian.Uint64(payload[:8]))
	if now.Unix() > expiry {
		return 0, ErrInvalidState
	}

	idx := binary.BigEndian.Uint32(payload[8:12])
	return int(idx), nil
}

// encodeState packs the (expiry, callback_index, nonce) triple into the
// fixed-width prefix that precedes the HMAC signature. 28 bytes total.
func encodeState(expiry int64, callbackIndex uint32, nonce []byte) []byte {
	out := make([]byte, 0, 8+4+len(nonce))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(expiry))
	out = append(out, buf[:]...)
	var ibuf [4]byte
	binary.BigEndian.PutUint32(ibuf[:], callbackIndex)
	out = append(out, ibuf[:]...)
	out = append(out, nonce...)
	return out
}

// signState computes HMAC-SHA256(secret, payload || appID). The appID is
// appended to the payload (not prepended) so a state issued for one app
// cannot be replayed against another.
func signState(secret, payload []byte, appID string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	mac.Write([]byte(appID))
	return mac.Sum(nil)
}