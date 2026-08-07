package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// linkIdentityError translates ONE Postgres error into a sentence, and it
// recognises it by index NAME. A name is a string in two places that nothing
// otherwise connects: rename or reshape the index in a migration and the Go
// constant keeps compiling, the tests keep passing, and the only visible effect
// is that a user who hits the collision goes back to "Sign-in failed. Please
// try again." forever.
//
// So the binding is asserted. Both files are checked, because they answer
// different questions: the migration is what actually ran against production,
// and db/schema.sql is what sqlc and the next Atlas diff read.

// The columns matter as much as the name: an index on (user_id, provider) is
// the one identity per provider per user rule. If someone changes the columns
// but keeps the name, the error message linkIdentityError produces is a lie.
func userProviderIndexRe() *regexp.Regexp {
	return regexp.MustCompile(
		`CREATE\s+UNIQUE\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
			regexp.QuoteMeta(identityUserProviderIndex) +
			`\s+ON\s+user_identities\s*\(\s*user_id\s*,\s*provider\s*\)`)
}

func TestIdentityUserProviderIndex_MatchesTheSchema(t *testing.T) {
	const schema = "../../db/schema.sql"

	src, err := os.ReadFile(schema)
	if err != nil {
		abs, _ := filepath.Abs(schema)
		t.Fatalf("cannot read %s: %v", abs, err)
	}
	if !userProviderIndexRe().MatchString(string(src)) {
		t.Fatalf("identityUserProviderIndex = %q, but db/schema.sql declares no unique index by that name on user_identities (user_id, provider). "+
			"linkIdentityError recognises the collision by this name, so a rename here silently turns social_provider_already_linked back into a generic failure.",
			identityUserProviderIndex)
	}
}

func TestIdentityUserProviderIndex_IsCreatedByAMigration(t *testing.T) {
	const dir = "../../migrations"

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	re := userProviderIndexRe()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if re.MatchString(string(src)) {
			return
		}
	}
	t.Fatalf("no migration creates a unique index named %q on user_identities (user_id, provider); "+
		"the name linkIdentityError matches on is not one this database will ever report.",
		identityUserProviderIndex)
}
