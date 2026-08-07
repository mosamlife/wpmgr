package auth

import "testing"

// The signup form asks for email and password only, so defaultTenantName is
// what every self-serve account is actually named. Before that field was
// removed the fallback was the literal "Default", which would have given every
// account on an instance the same meaningless name in the org switcher, in
// client reports, and in the admin console.
func TestDefaultTenantName(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  string
	}{
		// A work address carries the organization.
		{"company domain", "sarah@acme.com", "Acme"},
		{"subdomain is ignored", "ops@mail.acme.com", "Acme"},
		{"multi-part public suffix keeps the registrable label", "ops@acme.co.uk", "Acme"},
		{"hyphenated company", "hi@blue-fox.io", "Blue Fox"},
		{"uppercase domain is normalised", "hi@ACME.COM", "Acme"},

		// A consumer mailbox does not, so the local part is the better guess.
		{"gmail falls back to the local part", "sarah.jones@gmail.com", "Sarah Jones"},
		{"icloud falls back to the local part", "sarah_jones@icloud.com", "Sarah Jones"},
		{"plus addressing is split", "sarah+wpmgr@gmail.com", "Sarah Wpmgr"},
		{"proton falls back to the local part", "dev@proton.me", "Dev"},

		// Degenerate input must never produce an empty organization name.
		{"no at sign", "notanemail", "My organization"},
		{"empty local part", "@acme.com", "My organization"},
		{"empty string", "", "My organization"},
		{"punctuation-only local part on a consumer domain", "...@gmail.com", "My organization"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultTenantName(tc.email); got != tc.want {
				t.Fatalf("defaultTenantName(%q) = %q, want %q", tc.email, got, tc.want)
			}
		})
	}
}

// The column this lands in is bounded, and a pathological local part must not
// be the thing that discovers the limit.
func TestDefaultTenantNameIsBounded(t *testing.T) {
	long := ""
	for range 400 {
		long += "a"
	}
	got := defaultTenantName(long + "@gmail.com")
	if len([]rune(got)) > 200 {
		t.Fatalf("name is %d runes, want <= 200", len([]rune(got)))
	}
	if got == "" {
		t.Fatal("name must never be empty")
	}
}

// registrableLabel is the part that decides the name for a work address, and
// its edge cases are where a naive "second-to-last label" implementation went
// wrong: acme.co.uk yielded "Co".
func TestRegistrableLabel(t *testing.T) {
	cases := map[string]string{
		"acme.com":            "acme",
		"mail.acme.com":       "acme",
		"acme.co.uk":          "acme",
		"mail.acme.co.uk":     "acme",
		"acme.com.au":         "acme",
		"acme.co.in":          "acme",
		"deep.sub.acme.co.za": "acme",
		"localhost":           "localhost",
		// A bare two-part suffix has nothing in front of it; stepping over it
		// must not walk off the front of the slice.
		"co.uk": "co",
	}
	for in, want := range cases {
		if got := registrableLabel(in); got != want {
			t.Errorf("registrableLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
