package contract

// The usage WPMgr stores for wpmgr/page-create tells the AI which Elementor
// versions a draft can be built on, which outline values Elementor takes, and
// which WPMgr plugin release it needs. The agent decides the first two in its
// own code, and the control plane's builder floor the third. These gates read
// the agent's constants from its source, the floor from agentcmd, and the
// stored usage from the migrations and db/schema.sql, and fail when the usage
// says anything the agent or the floor does not.
//
// The usage checked is the one the newest page-create copy migration writes,
// found by its declaration, and db/schema.sql's seed must carry the same text.
// A migration that rewrites the usage is therefore checked here without any
// edit to this file, and one that rewrites schema.sql's copy but declares its
// text some other way fails the equality check instead of passing unseen.

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const (
	agentElementorFactsFile  = "../agent/includes/abilities/builders/class-elementor-facts.php"
	agentElementorMapperFile = "../agent/includes/abilities/builders/class-elementor-classic-mapper.php"
	pageCreateSchemaFile     = "db/schema.sql"

	// The first migration whose page-create usage described Elementor. It is
	// immutable, so discovery must always find it: the positive control.
	pageCreateUsageAnchor = "20261009060000_m166_builder_page_create.sql"
)

// elementorAgentFacts is what the agent's source says it builds in Elementor.
type elementorAgentFacts struct {
	classicMin  string   // ElementorFacts::CLASSIC_MIN, e.g. 3.20.0
	classicMax  string   // ElementorFacts::CLASSIC_MAX, e.g. 4.3.99
	imageAligns []string // ElementorClassicMapper::IMAGE_ALIGN, in order
}

var (
	reClassicMin = regexp.MustCompile(`(?m)^\s*public const CLASSIC_MIN = '(\d+)\.(\d+)\.(\d+)';`)
	reClassicMax = regexp.MustCompile(`(?m)^\s*public const CLASSIC_MAX = '(\d+)\.(\d+)\.(\d+)';`)

	// An Atomic request is refused before any version is looked at.
	reAtomicRefused = regexp.MustCompile(
		`if \(\$format === self::FORMAT_ATOMIC\) \{\s*return \['code' => self::CODE, 'detail' => 'atomic_unavailable'\];`)

	reImageAlign      = regexp.MustCompile(`(?m)^\s*private const IMAGE_ALIGN = \[([^\]]*)\];`)
	reImageAlignValue = regexp.MustCompile(`^'([a-z_]+)'$`)

	// A button link holding "&" is refused.
	reButtonLinkAmp = regexp.MustCompile(
		`if \(strpos\(\$url, '&'\) !== false\) \{\s*return \$this->fail\('link_invalid'`)

	// An open-ended Elementor version claim, which no bounded range allows.
	reOpenEndedElementor = regexp.MustCompile(`Elementor \d+(?:\.\d+)* or (?:later|newer|above)`)

	// How a page-create copy migration declares the usage it writes.
	reUsageDeclaration = regexp.MustCompile(`(?m)^\s*v_usage\s+constant\s+text\s*:=`)

	// db/schema.sql's page-create seed: its column list and its SELECT list.
	reSchemaPageCreateSeed = regexp.MustCompile(
		`(?s)INSERT INTO ability_catalogue \(([^)]*)\)\s*SELECT ('wpmgr/page-create',.*?)\nWHERE NOT EXISTS`)
)

// parseElementorAgentFacts reads the agent's constants and rules. Anything it
// cannot find is an error, never a default: a check that cannot see the
// agent's rule has nothing to compare the usage with.
func parseElementorAgentFacts(factsSrc, mapperSrc string) (elementorAgentFacts, error) {
	var f elementorAgentFacts
	var problems []string

	if m := reClassicMin.FindStringSubmatch(factsSrc); m != nil {
		f.classicMin = m[1] + "." + m[2] + "." + m[3]
	} else {
		problems = append(problems, "ElementorFacts::CLASSIC_MIN not found as a quoted x.y.z version")
	}
	if m := reClassicMax.FindStringSubmatch(factsSrc); m != nil {
		f.classicMax = m[1] + "." + m[2] + "." + m[3]
	} else {
		problems = append(problems, "ElementorFacts::CLASSIC_MAX not found as a quoted x.y.z version")
	}
	if !reAtomicRefused.MatchString(factsSrc) {
		problems = append(problems, "ElementorFacts::createRefusal no longer refuses every Atomic request first "+
			"(atomic_unavailable); the usage says Atomic is always refused")
	}

	if m := reImageAlign.FindStringSubmatch(mapperSrc); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			v := reImageAlignValue.FindStringSubmatch(strings.TrimSpace(part))
			if v == nil {
				problems = append(problems, fmt.Sprintf("ElementorClassicMapper::IMAGE_ALIGN holds %q, not a quoted value", part))
				continue
			}
			f.imageAligns = append(f.imageAligns, v[1])
		}
		if len(f.imageAligns) == 0 {
			problems = append(problems, "ElementorClassicMapper::IMAGE_ALIGN is empty")
		}
	} else {
		problems = append(problems, "ElementorClassicMapper::IMAGE_ALIGN not found")
	}
	if !reButtonLinkAmp.MatchString(mapperSrc) {
		problems = append(problems, "ElementorClassicMapper no longer refuses a button link holding \"&\" (link_invalid); "+
			"the usage says a button link cannot contain &")
	}

	if len(problems) > 0 {
		return f, errors.New(strings.Join(problems, "\n"))
	}
	return f, nil
}

// versionLabel is how the usage names a bound of the agent's range: major.minor
// when the bound takes in its whole minor line (a minimum ending in .0, a
// maximum ending in .99), the full version otherwise.
func versionLabel(v string, upper bool) string {
	p := strings.Split(v, ".")
	if (upper && p[2] == "99") || (!upper && p[2] == "0") {
		return p[0] + "." + p[1]
	}
	return v
}

// englishOr joins values as "a", "a or b", "a, b or c".
func englishOr(vals []string) string {
	if len(vals) < 2 {
		return strings.Join(vals, "")
	}
	return strings.Join(vals[:len(vals)-1], ", ") + " or " + vals[len(vals)-1]
}

// pageCreateUsageProblems lists every way usage disagrees with the agent, or
// with builderFloor, the first agent release the control plane sends a
// builder page to (agentcmd.MinAgentVersionForBuilderAdapters).
func pageCreateUsageProblems(usage string, f elementorAgentFacts, builderFloor string) []string {
	var problems []string
	if want := "Elementor pages need the WPMgr plugin " + builderFloor + " or later"; !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not name the WPMgr plugin floor for a builder page "+
			"(MinAgentVersionForBuilderAdapters %s): want %q", builderFloor, want))
	}
	wantRange := "Elementor " + versionLabel(f.classicMin, false) + " to " + versionLabel(f.classicMax, true) + " on the site"
	if !strings.Contains(usage, wantRange) {
		problems = append(problems, fmt.Sprintf("does not name the agent's Elementor range (CLASSIC_MIN %s, CLASSIC_MAX %s): want %q",
			f.classicMin, f.classicMax, wantRange))
	}
	if m := reOpenEndedElementor.FindString(usage); m != "" {
		problems = append(problems, fmt.Sprintf("claims %q, but the agent refuses every Elementor above CLASSIC_MAX %s",
			m, f.classicMax))
	}
	if want := "an image's align can only be " + englishOr(f.imageAligns); !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not name the image alignments the agent builds (IMAGE_ALIGN %v): want %q",
			f.imageAligns, want))
	}
	if want := "a button link cannot contain &"; !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not say the agent refuses a button link holding \"&\": want %q", want))
	}
	if want := "elementor_format atomic is always refused"; !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not say an Atomic request is always refused: want %q", want))
	}
	return problems
}

// sqlLiteralConcat returns the value of the SQL expression that starts at the
// beginning of src and runs to the first ',' or ';' outside a literal (or to
// the end of src): one or more single-quoted literals joined by ||. Anything
// else in the expression is an error, so a usage built some other way is
// refused rather than half read.
func sqlLiteralConcat(src string) (string, error) {
	var b strings.Builder
	literals := 0
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\'':
			j := i + 1
			for {
				if j >= len(src) {
					return "", errors.New("unterminated string literal")
				}
				if src[j] == '\'' {
					if j+1 < len(src) && src[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(src[j])
				j++
			}
			literals++
			i = j + 1
		case c == ',' || c == ';':
			i = len(src)
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '|' || c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			return "", fmt.Errorf("unexpected %q in what should be a concatenation of string literals", c)
		}
	}
	if literals == 0 {
		return "", errors.New("no string literal")
	}
	return b.String(), nil
}

// pageCreateUsageMigrations lists, in apply order, the migrations that
// declare a page-create usage.
func pageCreateUsageMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("list the embedded migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "'wpmgr/page-create'") && reUsageDeclaration.Match(b) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// migrationPageCreateUsage is the usage a page-create copy migration writes.
func migrationPageCreateUsage(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	loc := reUsageDeclaration.FindIndex(b)
	if loc == nil {
		t.Fatalf("%s declares no v_usage", name)
	}
	usage, err := sqlLiteralConcat(string(b[loc[1]:]))
	if err != nil {
		t.Fatalf("%s: read v_usage: %v", name, err)
	}
	return usage
}

// schemaPageCreateUsage is the usage db/schema.sql seeds page-create with.
func schemaPageCreateUsage(t *testing.T) string {
	t.Helper()
	m := reSchemaPageCreateSeed.FindStringSubmatch(readRepoFile(t, pageCreateSchemaFile))
	if m == nil {
		t.Fatalf("%s: the wpmgr/page-create seed was not found", pageCreateSchemaFile)
	}
	column := -1
	for i, c := range strings.Split(m[1], ",") {
		if strings.TrimSpace(c) == "usage" {
			column = i
		}
	}
	if column < 0 {
		t.Fatalf("%s: the page-create seed names no usage column: %s", pageCreateSchemaFile, m[1])
	}
	// Split the SELECT list at the commas outside literals and parentheses.
	var items []string
	depth, start, inLiteral := 0, 0, false
	list := m[2]
	for i := 0; i < len(list); i++ {
		switch c := list[i]; {
		case c == '\'':
			inLiteral = !inLiteral
		case inLiteral:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			items = append(items, list[start:i])
			start = i + 1
		}
	}
	items = append(items, list[start:])
	if column >= len(items) {
		t.Fatalf("%s: the page-create seed has %d values for its column list", pageCreateSchemaFile, len(items))
	}
	usage, err := sqlLiteralConcat(strings.TrimSpace(items[column]))
	if err != nil {
		t.Fatalf("%s: read the page-create usage: %v", pageCreateSchemaFile, err)
	}
	return usage
}

func readElementorAgentFacts(t *testing.T) (factsSrc, mapperSrc string, f elementorAgentFacts) {
	t.Helper()
	factsSrc = readRepoFile(t, agentElementorFactsFile)
	mapperSrc = readRepoFile(t, agentElementorMapperFile)
	f, err := parseElementorAgentFacts(factsSrc, mapperSrc)
	if err != nil {
		t.Fatalf("the agent's Elementor rules could not be read, so the usage cannot be checked against them:\n%v", err)
	}
	return factsSrc, mapperSrc, f
}

// TestPageCreateUsageMatchesAgentElementorRules: the stored page-create usage
// names the Elementor range the agent builds, the image alignments and the
// button link rule it enforces, and that Atomic is always refused; and
// db/schema.sql seeds the same usage the newest copy migration writes.
func TestPageCreateUsageMatchesAgentElementorRules(t *testing.T) {
	_, _, f := readElementorAgentFacts(t)

	names := pageCreateUsageMigrations(t)
	found := false
	for _, n := range names {
		found = found || n == pageCreateUsageAnchor
	}
	if !found {
		t.Fatalf("discovery did not find %s among the page-create copy migrations %v; "+
			"the declaration pattern no longer matches, so nothing below would be checked", pageCreateUsageAnchor, names)
	}
	newest := names[len(names)-1]
	usage := migrationPageCreateUsage(t, newest)

	if schema := schemaPageCreateUsage(t); schema != usage {
		t.Fatalf("%s seeds page-create with a usage other than the one %s writes:\nschema.sql: %q\nmigration:  %q",
			pageCreateSchemaFile, newest, schema, usage)
	}

	if problems := pageCreateUsageProblems(usage, f, agentcmd.MinAgentVersionForBuilderAdapters); len(problems) > 0 {
		t.Fatalf("the page-create usage %s writes disagrees with the agent (%s, %s):\n  - %s\nusage: %q",
			newest, agentElementorFactsFile, agentElementorMapperFile, strings.Join(problems, "\n  - "), usage)
	}
}

// TestPageCreateUsageCheckRefusesDrift: the check above is not vacuous. The
// usage #890 corrected fails it, and today's usage fails it, for the planted
// reason, once the agent's range or alignments or the builder floor move, or
// once the agent drops a rule the usage describes.
func TestPageCreateUsageCheckRefusesDrift(t *testing.T) {
	factsSrc, mapperSrc, f := readElementorAgentFacts(t)
	floor := agentcmd.MinAgentVersionForBuilderAdapters
	names := pageCreateUsageMigrations(t)
	if len(names) == 0 {
		t.Fatal("no page-create copy migration found")
	}
	usage := migrationPageCreateUsage(t, names[len(names)-1])
	if p := pageCreateUsageProblems(usage, f, floor); len(p) > 0 {
		t.Fatalf("SETUP: today's usage must pass before drift is planted: %v", p)
	}

	t.Run("m166's usage", func(t *testing.T) {
		if p := pageCreateUsageProblems(migrationPageCreateUsage(t, pageCreateUsageAnchor), f, floor); len(p) == 0 {
			t.Fatal("m166's usage, which claimed Elementor 3.20 or later and left out the alignment and link rules, passes the check")
		}
	})

	t.Run("a raised builder floor", func(t *testing.T) {
		p := pageCreateUsageProblems(usage, f, floor+".1")
		if len(p) != 1 || !strings.Contains(p[0], "WPMgr plugin floor") {
			t.Fatalf("today's usage against a moved builder floor gave %q, want the floor problem alone", p)
		}
	})

	bump := func(t *testing.T, re *regexp.Regexp, src string) string {
		t.Helper()
		m := re.FindStringSubmatchIndex(src)
		if m == nil {
			t.Fatalf("SETUP: %s not found", re)
		}
		minor, err := strconv.Atoi(src[m[4]:m[5]])
		if err != nil {
			t.Fatalf("SETUP: %v", err)
		}
		return src[:m[4]] + strconv.Itoa(minor+1) + src[m[5]:]
	}
	for _, tc := range []struct {
		what      string
		factsSrc  string
		mapperSrc string
		want      string // the problem the plant must produce
	}{
		{"a raised CLASSIC_MAX", bump(t, reClassicMax, factsSrc), mapperSrc, "Elementor range"},
		{"a raised CLASSIC_MIN", bump(t, reClassicMin, factsSrc), mapperSrc, "Elementor range"},
		{"an added image alignment", factsSrc, strings.Replace(mapperSrc,
			"private const IMAGE_ALIGN = ['none', 'center'];", "private const IMAGE_ALIGN = ['none', 'center', 'left'];", 1),
			"image alignments"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			moved, err := parseElementorAgentFacts(tc.factsSrc, tc.mapperSrc)
			if err != nil {
				t.Fatalf("SETUP: %v", err)
			}
			if reflectEqualFacts(moved, f) {
				t.Fatalf("SETUP: the planted change did not move the facts: %+v", moved)
			}
			p := pageCreateUsageProblems(usage, moved, floor)
			if len(p) != 1 || !strings.Contains(p[0], tc.want) {
				t.Fatalf("today's usage against facts %+v gave %q, want the %s problem alone", moved, p, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		what      string
		factsSrc  string
		mapperSrc string
	}{
		{"a dropped button link rule", factsSrc, strings.Replace(mapperSrc, "strpos($url, '&')", "strpos($url, '#')", 1)},
		{"a dropped Atomic refusal", strings.Replace(factsSrc, "'detail' => 'atomic_unavailable'", "'detail' => 'atomic_later'", 1), mapperSrc},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if tc.factsSrc == factsSrc && tc.mapperSrc == mapperSrc {
				t.Fatal("SETUP: the plant changed neither source")
			}
			if _, err := parseElementorAgentFacts(tc.factsSrc, tc.mapperSrc); err == nil {
				t.Fatal("the agent's rules parse without it; the usage would keep claiming a rule the agent dropped")
			}
		})
	}
}

func reflectEqualFacts(a, b elementorAgentFacts) bool {
	return a.classicMin == b.classicMin && a.classicMax == b.classicMax &&
		strings.Join(a.imageAligns, ",") == strings.Join(b.imageAligns, ",")
}
