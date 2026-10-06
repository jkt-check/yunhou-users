package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeAttribution_NilAndEmpty(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]json.RawMessage{
		"nil":         nil,
		"empty bytes": {},
		"json null":   json.RawMessage(`null`),
		"empty obj":   json.RawMessage(`{}`),
		"null touches": json.RawMessage(
			`{"first_touch":null,"last_touch":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := SanitizeAttribution(in)
			if err != nil {
				t.Fatalf("SanitizeAttribution(%s): err = %v, want nil", in, err)
			}
			if out != nil {
				t.Errorf("SanitizeAttribution(%s) = %s, want nil (store NULL)", in, out)
			}
		})
	}
}

func TestSanitizeAttribution_InvalidJSON(t *testing.T) {
	t.Parallel()
	for _, in := range []json.RawMessage{
		json.RawMessage(`{`),
		json.RawMessage(`"just a string"`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`{"first_touch": 42}`),
	} {
		if out, err := SanitizeAttribution(in); err == nil {
			t.Errorf("SanitizeAttribution(%s) = %s, nil; want error", in, out)
		}
	}
}

func TestSanitizeAttribution_KnownKeysKeptUnknownDropped(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{
		"first_touch": {
			"utm_source": "google",
			"utm_medium": "cpc",
			"utm_campaign": null,
			"utm_content": null,
			"referrer_domain": "www.x.com",
			"landing_path": "/",
			"captured_at": "2026-10-01T08:30:00Z",
			"session_id": "drop-me",
			"nested": {"evil": true}
		},
		"last_touch": {
			"utm_source": "bing",
			"unknown_top": "drop-me-too"
		},
		"unknown_root": "drop-me-three"
	}`)
	out, err := SanitizeAttribution(in)
	if err != nil {
		t.Fatalf("SanitizeAttribution: %v", err)
	}
	if out == nil {
		t.Fatal("SanitizeAttribution returned nil, want sanitized snapshot")
	}

	s := string(out)
	for _, dropped := range []string{"session_id", "nested", "unknown_top", "unknown_root"} {
		if strings.Contains(s, dropped) {
			t.Errorf("output contains unknown key %q: %s", dropped, s)
		}
	}

	var got Attribution
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if got.FirstTouch == nil || got.LastTouch == nil {
		t.Fatalf("touches lost: %s", s)
	}
	ft := got.FirstTouch
	if ft.UtmSource == nil || *ft.UtmSource != "google" {
		t.Errorf("utm_source = %v, want google", ft.UtmSource)
	}
	if ft.UtmMedium == nil || *ft.UtmMedium != "cpc" {
		t.Errorf("utm_medium = %v, want cpc", ft.UtmMedium)
	}
	if ft.ReferrerDomain == nil || *ft.ReferrerDomain != "www.x.com" {
		t.Errorf("referrer_domain = %v, want www.x.com", ft.ReferrerDomain)
	}
	if ft.LandingPath == nil || *ft.LandingPath != "/" {
		t.Errorf("landing_path = %v, want /", ft.LandingPath)
	}
	// captured_at rides through as a raw string — never parsed to time.
	if ft.CapturedAt == nil || *ft.CapturedAt != "2026-10-01T08:30:00Z" {
		t.Errorf("captured_at = %v, want verbatim 2026-10-01T08:30:00Z", ft.CapturedAt)
	}
	// null sub-fields stay absent.
	if ft.UtmCampaign != nil || ft.UtmContent != nil {
		t.Errorf("null sub-fields should stay nil, got campaign=%v content=%v", ft.UtmCampaign, ft.UtmContent)
	}
	if got.LastTouch.UtmSource == nil || *got.LastTouch.UtmSource != "bing" {
		t.Errorf("last_touch.utm_source = %v, want bing", got.LastTouch.UtmSource)
	}
}

func TestSanitizeAttribution_TruncatesLongStrings(t *testing.T) {
	t.Parallel()
	// 250 ASCII chars and a 250-rune multi-byte string: truncation is by
	// rune, not byte — a byte-wise cut would split UTF-8 mid-sequence.
	longASCII := strings.Repeat("a", 250)
	longUTF8 := strings.Repeat("中", 250)
	in := json.RawMessage(`{"first_touch":{"utm_source":` + strconv(longASCII) +
		`,"landing_path":` + strconv(longUTF8) + `}}`)
	out, err := SanitizeAttribution(in)
	if err != nil {
		t.Fatalf("SanitizeAttribution: %v", err)
	}
	var got Attribution
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if got.FirstTouch == nil || got.FirstTouch.UtmSource == nil {
		t.Fatal("utm_source lost")
	}
	if n := len([]rune(*got.FirstTouch.UtmSource)); n != 200 {
		t.Errorf("utm_source runes = %d, want 200", n)
	}
	if got.FirstTouch.LandingPath == nil {
		t.Fatal("landing_path lost")
	}
	if n := len([]rune(*got.FirstTouch.LandingPath)); n != 200 {
		t.Errorf("landing_path runes = %d, want 200 (rune-safe truncation)", n)
	}
	if !json.Valid(out) {
		t.Errorf("output is not valid JSON: %s", out)
	}
}

// strconv JSON-quotes a string without pulling in strconv for real.
func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The server is the last enforcement point before PostHog: a website-side
// bug sending /checkout?email=x@y.com would otherwise ride into
// purchase_completed's attribution property verbatim. landing_path keeps
// only the path — query and fragment are cut before truncation.
func TestSanitizeAttribution_StripsLandingPathQueryFragment(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"last_touch":{"landing_path":"/checkout?email=x@y.com&plan=pro#token=abc","utm_source":"google"}}`)
	out, err := SanitizeAttribution(in)
	if err != nil {
		t.Fatalf("SanitizeAttribution: %v", err)
	}
	var got Attribution
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if got.LastTouch == nil || got.LastTouch.LandingPath == nil {
		t.Fatal("landing_path lost")
	}
	if *got.LastTouch.LandingPath != "/checkout" {
		t.Errorf("landing_path = %q, want /checkout (query+fragment stripped)", *got.LastTouch.LandingPath)
	}
	if strings.Contains(string(out), "x@y.com") || strings.Contains(string(out), "token=abc") {
		t.Errorf("PII/query leaked into sanitized output: %s", out)
	}
	// Other fields are untouched by the strip.
	if got.LastTouch.UtmSource == nil || *got.LastTouch.UtmSource != "google" {
		t.Errorf("utm_source = %v, want google", got.LastTouch.UtmSource)
	}
}
