package service

import (
	"testing"
)

// TestNormalizeEmail covers the email canonicalizer used across providers.
func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                    "",
		"   ":                 "",
		"USER@example.com":    "user@example.com",
		"User@Example.COM":    "user@example.com",
		"  alice@example.com": "alice@example.com",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := normalizeEmail(in); got != want {
				t.Errorf("normalizeEmail(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// TestFirstNonEmpty covers the variadic helper used to pick the first
// non-empty value (e.g. for choosing an authoritative email among several).
func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values []string
		want   string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"first set", []string{"a", "b", "c"}, "a"},
		{"middle set", []string{"", "b", "c"}, "b"},
		{"last set", []string{"", "", "c"}, "c"},
		{"empty slice", []string{}, ""},
		{"nil-ish", nil, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstNonEmpty(tc.values...); got != tc.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.values, got, tc.want)
			}
		})
	}
}
