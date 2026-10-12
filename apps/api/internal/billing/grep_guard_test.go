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

// designKeyForms are the spellings of the page design object's own member
// named scale (its type scale, holding base_px and ratio). That name is part
// of the design wire format and says nothing about a plan. A line is judged
// by tierLiteralPattern only after these spellings are set aside, so a "scale"
// that is left over, or any other tier on the same line, is still reported.
//
// Each alternative recognises one spelling and nothing wider:
//   - a struct tag naming the member;
//   - a JSON or Go map member whose value opens with a type-scale member;
//   - the name listed beside another design member;
//   - the name passed to, or indexing a value of, a design-named identifier.
//
// The filler allowed ahead of the name in a call holds no quote, so no tier
// literal can sit inside a span that is set aside.
var designKeyForms = regexp.MustCompile(strings.Join([]string{
	`json:"scale"`,
	`"scale"\s*:\s*(?:map\[string\](?:\w+|interface\{\}))?\{\s*"(?:base_px|ratio)"`,
	`"(?:colors|fonts|radius|density)"\s*,\s*"scale"`,
	`"scale"\s*,\s*"(?:colors|fonts|radius|density)"`,
	`\w*(?i:design)\w*\([\w.,\s\[]*"scale"`,
	`\w*(?i:design)\w*\["scale"\]`,
}, "|"))

// namesPlanTier reports whether a source line spells a paid plan tier or
// compares a plan value, once the design object's scale member is set aside.
func namesPlanTier(line string) bool {
	return tierLiteralPattern.MatchString(designKeyForms.ReplaceAllString(line, " "))
}

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
			if namesPlanTier(line) {
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

// TestNamesPlanTier proves the line matcher in both directions. Every row in
// fires names a plan tier or compares a plan value and must be reported;
// every row in quiet is the page design object's own scale member, or text
// that only contains the word, and must pass. The rows copied from the live
// design code keep their original spelling.
func TestNamesPlanTier(t *testing.T) {
	type row struct{ name, line string }

	fires := []row{
		{"plan comparison", `if plan == "scale" {`},
		{"plan comparison against another value", `if plan == "free" {`},
		{"field comparison", `if t.Plan == "scale" {`},
		{"switch case", `case "scale":`},
		{"keyed field", `Plan: "scale",`},
		{"short declaration", `plan := "scale"`},
		{"assignment", `tenant.Plan = "scale"`},
		{"constant", `TierScale Tier = "scale"`},
		{"table row", `{"scale", TierScale, 200},`},
		{"tier list", `tiers := []string{"free", "starter", "agency", "scale"}`},
		{"call argument", `ent := Entitlements("scale")`},
		{"map index", `cap := siteCaps["scale"]`},
		{"tier-keyed map entry", `"scale": {MaxSites: 200},`},
		{"json plan value", "body := `{\"plan\":\"scale\"}`"},
		{"starter case", `case "starter":`},
		{"agency field", `Plan: "agency",`},
		{"design list beside a tier", `d := []string{"colors", "fonts", "scale"}; tier = "scale"`},
		{"design call beside other tiers", `designFor("agency", "scale")`},
		{"design word in a comment", `Plan: "scale", // design review`},
	}

	quiet := []row{
		// The design object's scale member, spelled as the live code spells it.
		{"struct tag", "Scale   designScale   `json:\"scale\"`"},
		{"member list", `designTopKeys   = []string{"colors", "fonts", "scale", "radius", "density"}`},
		{"group loop", `for _, group := range []string{"colors", "fonts", "scale"} {`},
		{"spaced json member", `"fonts": {"heading": "Manrope", "body": "Source Sans 3"}, "scale": {"base_px": 17, "ratio": 1.25},`},
		{"compact json member", "`\"fonts\":{\"heading\":\"Manrope\",\"body\":\"Source Sans 3\"},\"scale\":{\"base_px\":17,\"ratio\":1.25},\"radius\":\"md\",\"density\":\"normal\"}`"},
		{"call with an index", `d.Scale, more = parseDesignScale(obj["scale"])`},
		{"call with the name as an argument", `obj, fails := designGroupObject(raw, "scale", designScaleKeys, "a base size and a ratio")`},
		// Other spellings of the same member.
		{"go map member", `"scale": map[string]any{"base_px": 17, "ratio": 1.25},`},
		{"member with the ratio first", `"scale":{"ratio":1.25,"base_px":17}`},
		{"index of a design value", `scaleObj := design["scale"]`},
		{"list with scale first", `want := []string{"scale", "radius", "density"}`},
		// Text that only contains the word.
		{"unquoted word in a comment", `// the type scale of a design`},
		{"longer literal", `kind := "autoscale"`},
		{"identifier", `scale := ratio * base`},
	}

	if len(fires) == 0 || len(quiet) == 0 {
		t.Fatal("the table needs rows in both directions")
	}
	for _, c := range fires {
		t.Run("fires/"+c.name, func(t *testing.T) {
			if !namesPlanTier(c.line) {
				t.Errorf("not reported, but it names a plan tier:\n\t%s", c.line)
			}
		})
	}
	for _, c := range quiet {
		t.Run("quiet/"+c.name, func(t *testing.T) {
			if namesPlanTier(c.line) {
				t.Errorf("reported, but it is not a plan tier:\n\t%s", c.line)
			}
		})
	}
}
