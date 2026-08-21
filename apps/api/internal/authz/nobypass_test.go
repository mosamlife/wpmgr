package authz_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDirectAllowsOutsideAuthz makes the capability bypass unrepresentable.
//
// authz.Allows takes a Role and nothing else. For a capability principal the
// role is a placeholder — the authority is the explicit set — so any call site
// outside this package that reaches for Allows hands a capability-limited key
// its full role rank. That is precisely the hole #510 exists to close, and it
// had opened twice already (internal/files/handler.go and
// internal/perf/handler.go, both in a private `allows` helper that read as
// boilerplate). The correct call is authz.PrincipalAllows.
//
// Allows stays exported because the role matrix is genuinely useful to admin
// and introspection surfaces; this test is what keeps it from being used as an
// authorization decision outside the package that owns the model.
func TestNoDirectAllowsOutsideAuthz(t *testing.T) {
	root := repoAPIRoot(t)

	// The sentinel proves the scanner can actually see source. It is a string
	// this test knows exists in the tree: PrincipalAllows is the replacement
	// every converted call site now uses, so if the walk finds zero of them the
	// walk is broken — wrong root, wrong extension filter, an empty checkout —
	// and a "no violations" result would be a false pass. A guard that cannot
	// find its inputs must fail, not pass.
	const sentinel = "authz.PrincipalAllows("
	const forbidden = "authz.Allows("

	var (
		goFilesScanned int
		sentinelHits   int
		violations     []string
	)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "gen", "sqlc", "testdata", "vendor", "node_modules":
				return filepath.SkipDir
			}
			// The authz package owns Allows; calls inside it are the point.
			if path != root && filepath.Base(filepath.Dir(path)) == "internal" && d.Name() == "authz" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// _test.go is excluded deliberately, and this is the honest case the
		// guard must not block. A role-model test computes the EXPECTED answer
		// with Allows(role, perm) and asserts PrincipalAllows agrees — see
		// tests/apikey_capability_integration_test.go, which is the proof that
		// the role model did not move. Reddening that is reddening correct
		// work, and a guard that reddens correct work gets switched off and
		// then guards nothing. Only non-test source makes authorization
		// decisions in a request path, and only non-test source is scanned.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		goFilesScanned++
		src := string(b)
		sentinelHits += strings.Count(src, sentinel)

		for i, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, forbidden) {
				continue
			}
			// authz.PrincipalAllows( does not contain authz.Allows(, so no
			// exclusion is needed for the correct call; anything matching here
			// is a genuine role-only decision.
			rel, _ := filepath.Rel(root, path)
			violations = append(violations, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Fail loudly when the search found nothing to search. Both of these are
	// "the guard is blind", not "the code is clean".
	if goFilesScanned == 0 {
		t.Fatalf("scanned 0 .go files under %s — the guard is blind, not clean", root)
	}
	if sentinelHits == 0 {
		t.Fatalf("found 0 occurrences of the sentinel %q across %d .go files under %s — "+
			"the guard cannot see the call sites it is meant to police, so a clean result is meaningless",
			sentinel, goFilesScanned, root)
	}

	if len(violations) > 0 {
		t.Fatalf("%d call site(s) use %s outside the authz package; use authz.PrincipalAllows so a "+
			"capability key is not handed its role rank:\n  %s",
			len(violations), forbidden, strings.Join(violations, "\n  "))
	}

	t.Logf("scanned %d .go files, %d PrincipalAllows call sites, 0 direct Allows call sites",
		goFilesScanned, sentinelHits)
}

// repoAPIRoot returns apps/api, located by walking up from this test file's
// package directory until go.mod is found. It fails the test rather than
// falling back to a guessed path.
func repoAPIRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod above %s — cannot determine the scan root", dir)
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
