package billing

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tierLiteralPattern matches the three paid-tier string literals and a bare
// "plan ==" comparison. internal/billing is the ONLY package allowed to know
// these tier names or compare a plan value directly — every other domain
// must go through Entitlements()/CheckSiteCreate, so the plan ladder has a
// single owner (M16 Phase A). See the package doc comment.
//
// "free" is deliberately excluded: it is an ordinary English word that
// appears throughout the codebase for unrelated reasons (free disk space,
// "feel free", etc.) and would make this guard noisy without adding safety —
// the three PAID tier names are distinctive enough to be a reliable signal.
var tierLiteralPattern = regexp.MustCompile(`"starter"|"agency"|"scale"|\bplan\b\s*==`)

// TestNoPlanLiteralsOutsideBilling walks every .go file under apps/api
// (skipping generated/vendor trees and this package itself) and fails if any
// of them contain a paid-tier string literal or a "plan ==" comparison. This
// is a structural single-ownership guard, not a behavior test: it protects
// the plan ladder from silently growing a second, drifting copy elsewhere
// (e.g. a handler hardcoding "if plan == \"agency\"" instead of calling
// Entitlements()).
func TestNoPlanLiteralsOutsideBilling(t *testing.T) {
	root, err := filepath.Abs(filepath.Join(".", "..", ".."))
	if err != nil {
		t.Fatalf("resolve apps/api root: %v", err)
	}
	if filepath.Base(root) != "api" {
		t.Fatalf("computed root %q does not look like apps/api — refusing to walk it", root)
	}
	billingDir := filepath.Join(root, "internal", "billing")

	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == billingDir:
				return filepath.SkipDir
			case d.Name() == "gen" && strings.Contains(path, filepath.Join("internal", "api")):
				return filepath.SkipDir // ogen-generated
			case d.Name() == "sqlc":
				return filepath.SkipDir // sqlc-generated
			case d.Name() == "node_modules" || d.Name() == ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			if tierLiteralPattern.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk apps/api tree: %v", walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("plan-tier literal or comparison found outside internal/billing "+
			"(single-ownership violation — route this through internal/billing.Entitlements "+
			"or CheckSiteCreate instead):\n%s", strings.Join(offenders, "\n"))
	}
}
