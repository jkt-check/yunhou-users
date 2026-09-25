package management

import (
	"strings"
	"testing"
)

func TestPermissionsOfMatrix(t *testing.T) {
	cases := []struct {
		roles []string
		want  map[string]bool
	}{
		{[]string{"admin"}, map[string]bool{PermModelsManage: true, PermCredentialsManage: true, PermBillingAdjust: true, PermUsageRead: true}},
		{[]string{"operator"}, map[string]bool{PermModelsManage: true, PermCredentialsManage: true, PermBillingAdjust: false, PermUsageRead: true}},
		{[]string{"auditor"}, map[string]bool{PermModelsManage: false, PermCredentialsManage: false, PermBillingAdjust: false, PermUsageRead: true}},
		{[]string{}, map[string]bool{}},
		{[]string{"unknown-role"}, map[string]bool{}},
	}
	for _, tc := range cases {
		perms := PermissionsOf(tc.roles)
		for perm, want := range tc.want {
			if got := perms[perm]; got != want {
				t.Errorf("roles %v: %s = %v, want %v", tc.roles, perm, got, want)
			}
		}
		if len(tc.want) == 0 && len(perms) != 0 {
			t.Errorf("roles %v: expected no permissions, got %v", tc.roles, perms)
		}
	}
	// 后台读权限与秘密写权限分开：auditor 可读统计、不可写凭据。
	auditor := PermissionsOf([]string{"auditor"})
	if auditor[PermUsageRead] != true || len(auditor) != 1 {
		t.Fatalf("auditor must have exactly usage:read, got %v", auditor)
	}
}

func TestValidRole(t *testing.T) {
	for _, r := range []string{"admin", "operator", "auditor"} {
		if !ValidRole(r) {
			t.Errorf("%s must be valid", r)
		}
	}
	for _, r := range []string{"", "root", "superuser", "Admin"} {
		if ValidRole(r) {
			t.Errorf("%q must be invalid", r)
		}
	}
}

func TestSanitizeDetail(t *testing.T) {
	in := map[string]any{
		"label":       "prod",
		"secret":      "sk-live",
		"api_key":     "leaked",
		"nested":      map[string]any{"client_secret": "x", "ok": 1},
		"list":        []any{map[string]any{"token": "y"}, "fine"},
		"key_version": 2,
	}
	out := SanitizeDetail(in)
	if out["label"] != "prod" || out["key_version"] != 2 {
		t.Fatalf("benign keys must survive: %v", out)
	}
	for _, banned := range []string{"secret", "api_key"} {
		if _, present := out[banned]; present {
			t.Fatalf("%s must be stripped: %v", banned, out)
		}
	}
	nested := out["nested"].(map[string]any)
	if _, present := nested["client_secret"]; present {
		t.Fatalf("nested secret must be stripped: %v", nested)
	}
	if nested["ok"] != 1 {
		t.Fatalf("nested benign key lost: %v", nested)
	}
	items := out["list"].([]any)
	if items[0].(map[string]any)["token"] != nil || len(items[0].(map[string]any)) != 0 {
		t.Fatalf("list item secret must be stripped: %v", items[0])
	}
	if SanitizeDetail(nil) == nil {
		t.Fatal("nil detail must produce a non-nil empty map")
	}
}

func TestParseActor(t *testing.T) {
	u, a := ParseActor("user:abc-123@app:yunhou-website")
	if u != "abc-123" || a != "yunhou-website" {
		t.Fatalf("dual attribution: %q %q", u, a)
	}
	// Bare strings land entirely in the user slot; no crash.
	u, a = ParseActor("legacy-actor")
	if u != "legacy-actor" || a != "" {
		t.Fatalf("bare actor: %q %q", u, a)
	}
	u, a = ParseActor("@app:x")
	if strings.Contains(u, "@app:") {
		t.Fatalf("malformed actor must not keep the marker: %q", u)
	}
}
