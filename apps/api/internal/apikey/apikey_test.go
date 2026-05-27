package apikey

import "testing"

func TestParseToken(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantOK     bool
		wantPrefix string
		wantSecret string
	}{
		{"valid", "wpmgr_abc123_secretpart", true, "abc123", "secretpart"},
		{"wrong prefix", "other_abc_secret", false, "", ""},
		{"too few parts", "wpmgr_abc", false, "", ""},
		{"too many parts", "wpmgr_abc_secret_extra", false, "", ""},
		{"empty prefix", "wpmgr__secret", false, "", ""},
		{"empty secret", "wpmgr_abc_", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, secret, ok := parseToken(tt.token)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if prefix != tt.wantPrefix || secret != tt.wantSecret {
					t.Fatalf("got (%q,%q), want (%q,%q)", prefix, secret, tt.wantPrefix, tt.wantSecret)
				}
			}
		})
	}
}

func TestHashSecretDeterministic(t *testing.T) {
	if hashSecret("abc") != hashSecret("abc") {
		t.Fatal("hashSecret not deterministic")
	}
	if hashSecret("abc") == hashSecret("abd") {
		t.Fatal("hashSecret collision for different inputs")
	}
}
