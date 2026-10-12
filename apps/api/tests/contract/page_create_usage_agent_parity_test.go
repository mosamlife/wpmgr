package contract

// The usage WPMgr stores for wpmgr/page-create tells the AI which Elementor
// versions a draft can be built on, which outline values Elementor takes, and
// which WPMgr plugin release it needs. The agent decides the first two in its
// own code, and the control plane's builder floor the third. These gates read
// the agent's constants from its source, the floor from agentcmd, and the
// stored usage from the migrations and db/schema.sql, and fail when the usage
// says anything the agent or the floor does not. They also read the refusal
// codes the agent's designed-page tables answer, and fail when one is outside
// the closed set of agent codes or has no hint for the AI.
//
// The usage checked is the one the newest page-create copy migration writes,
// found by its declaration, and db/schema.sql's seed must carry the same text.
// A migration that rewrites the usage is therefore checked here without any
// edit to this file, and one that rewrites schema.sql's copy but declares its
// text some other way fails the equality check instead of passing unseen.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const (
	agentElementorFactsFile  = "../agent/includes/abilities/builders/class-elementor-facts.php"
	agentElementorMapperFile = "../agent/includes/abilities/builders/class-elementor-classic-mapper.php"
	agentOutlineV3SchemaFile = "../agent/includes/abilities/builders/class-outline-v3-schema.php"
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
	maxSections int      // OutlineV3Schema::MAX_SECTIONS
}

var (
	reClassicMin = regexp.MustCompile(`(?m)^\s*public const CLASSIC_MIN = '(\d+)\.(\d+)\.(\d+)';`)
	reClassicMax = regexp.MustCompile(`(?m)^\s*public const CLASSIC_MAX = '(\d+)\.(\d+)\.(\d+)';`)

	// An Atomic request is refused before any version is looked at.
	reAtomicRefused = regexp.MustCompile(
		`if \(\$format === self::FORMAT_ATOMIC\) \{\s*return \['code' => self::CODE, 'detail' => 'atomic_unavailable'\];`)

	reMaxSections = regexp.MustCompile(`(?m)^\s*public const MAX_SECTIONS = (\d+);`)

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
func parseElementorAgentFacts(factsSrc, mapperSrc, schemaSrc string) (elementorAgentFacts, error) {
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

	if m := reMaxSections.FindStringSubmatch(schemaSrc); m != nil {
		f.maxSections, _ = strconv.Atoi(m[1])
	} else {
		problems = append(problems, "OutlineV3Schema::MAX_SECTIONS not found as a whole number")
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
// builder page to (agentcmd.MinAgentVersionForBuilderAdapters), or with
// designedFloor, the first one it sends a designed page to
// (agentcmd.MinAgentVersionForDesignedPages).
func pageCreateUsageProblems(usage string, f elementorAgentFacts, builderFloor, designedFloor string) []string {
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
	if want := "Designed pages need the WPMgr plugin " + designedFloor + " or later"; !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not name the WPMgr plugin floor for a designed page "+
			"(MinAgentVersionForDesignedPages %s): want %q", designedFloor, want))
	}
	if want := fmt.Sprintf("Use up to %d sections", f.maxSections); !strings.Contains(usage, want) {
		problems = append(problems, fmt.Sprintf("does not name the section cap the agent enforces "+
			"(OutlineV3Schema::MAX_SECTIONS %d): want %q", f.maxSections, want))
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

func readElementorAgentFacts(t *testing.T) (factsSrc, mapperSrc, schemaSrc string, f elementorAgentFacts) {
	t.Helper()
	factsSrc = readRepoFile(t, agentElementorFactsFile)
	mapperSrc = readRepoFile(t, agentElementorMapperFile)
	schemaSrc = readRepoFile(t, agentOutlineV3SchemaFile)
	f, err := parseElementorAgentFacts(factsSrc, mapperSrc, schemaSrc)
	if err != nil {
		t.Fatalf("the agent's Elementor rules could not be read, so the usage cannot be checked against them:\n%v", err)
	}
	return factsSrc, mapperSrc, schemaSrc, f
}

// TestPageCreateUsageMatchesAgentElementorRules: the stored page-create usage
// names the Elementor range the agent builds, the image alignments and the
// button link rule it enforces, that Atomic is always refused, the section cap
// of a designed page, and the plugin release a designed page needs; and
// db/schema.sql seeds the same usage the newest copy migration writes.
func TestPageCreateUsageMatchesAgentElementorRules(t *testing.T) {
	_, _, _, f := readElementorAgentFacts(t)

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

	if problems := pageCreateUsageProblems(usage, f, agentcmd.MinAgentVersionForBuilderAdapters,
		agentcmd.MinAgentVersionForDesignedPages); len(problems) > 0 {
		t.Fatalf("the page-create usage %s writes disagrees with the agent (%s, %s):\n  - %s\nusage: %q",
			newest, agentElementorFactsFile, agentElementorMapperFile, strings.Join(problems, "\n  - "), usage)
	}
}

// TestPageCreateUsageCheckRefusesDrift: the check above is not vacuous. The
// usage #890 corrected fails it, and today's usage fails it, for the planted
// reason, once the agent's range or alignments or the builder floor move, or
// once the agent drops a rule the usage describes, or once the designed-pages
// floor or the section cap moves.
func TestPageCreateUsageCheckRefusesDrift(t *testing.T) {
	factsSrc, mapperSrc, schemaSrc, f := readElementorAgentFacts(t)
	floor, designed := agentcmd.MinAgentVersionForBuilderAdapters, agentcmd.MinAgentVersionForDesignedPages
	names := pageCreateUsageMigrations(t)
	if len(names) == 0 {
		t.Fatal("no page-create copy migration found")
	}
	usage := migrationPageCreateUsage(t, names[len(names)-1])
	if p := pageCreateUsageProblems(usage, f, floor, designed); len(p) > 0 {
		t.Fatalf("SETUP: today's usage must pass before drift is planted: %v", p)
	}

	t.Run("m166's usage", func(t *testing.T) {
		if p := pageCreateUsageProblems(migrationPageCreateUsage(t, pageCreateUsageAnchor), f, floor, designed); len(p) == 0 {
			t.Fatal("m166's usage, which claimed Elementor 3.20 or later and left out the alignment and link rules, passes the check")
		}
	})

	t.Run("a raised builder floor", func(t *testing.T) {
		p := pageCreateUsageProblems(usage, f, floor+".1", designed)
		if len(p) != 1 || !strings.Contains(p[0], "WPMgr plugin floor for a builder page") {
			t.Fatalf("today's usage against a moved builder floor gave %q, want the floor problem alone", p)
		}
	})

	t.Run("a raised designed-pages floor", func(t *testing.T) {
		p := pageCreateUsageProblems(usage, f, floor, designed+".1")
		if len(p) != 1 || !strings.Contains(p[0], "WPMgr plugin floor for a designed page") {
			t.Fatalf("today's usage against a moved designed-pages floor gave %q, want the floor problem alone", p)
		}
	})
	t.Run("a designed-pages floor the usage never names", func(t *testing.T) {
		cut := strings.Replace(usage, "Designed pages need the WPMgr plugin "+designed+" or later", "Designed pages need a recent plugin", 1)
		if cut == usage {
			t.Fatal("SETUP: the plant changed nothing")
		}
		p := pageCreateUsageProblems(cut, f, floor, designed)
		if len(p) != 1 || !strings.Contains(p[0], "WPMgr plugin floor for a designed page") {
			t.Fatalf("a usage without the designed-pages floor gave %q, want the floor problem alone", p)
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
	raise := func(t *testing.T, re *regexp.Regexp, src string) string {
		t.Helper()
		m := re.FindStringSubmatchIndex(src)
		if m == nil {
			t.Fatalf("SETUP: %s not found", re)
		}
		n, err := strconv.Atoi(src[m[2]:m[3]])
		if err != nil {
			t.Fatalf("SETUP: %v", err)
		}
		return src[:m[2]] + strconv.Itoa(n+1) + src[m[3]:]
	}
	for _, tc := range []struct {
		what      string
		factsSrc  string
		mapperSrc string
		schemaSrc string
		want      string // the problem the plant must produce
	}{
		{"a raised CLASSIC_MAX", bump(t, reClassicMax, factsSrc), mapperSrc, schemaSrc, "Elementor range"},
		{"a raised CLASSIC_MIN", bump(t, reClassicMin, factsSrc), mapperSrc, schemaSrc, "Elementor range"},
		{"an added image alignment", factsSrc, strings.Replace(mapperSrc,
			"private const IMAGE_ALIGN = ['none', 'center'];", "private const IMAGE_ALIGN = ['none', 'center', 'left'];", 1),
			schemaSrc, "image alignments"},
		{"a raised MAX_SECTIONS", factsSrc, mapperSrc, raise(t, reMaxSections, schemaSrc), "section cap"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			moved, err := parseElementorAgentFacts(tc.factsSrc, tc.mapperSrc, tc.schemaSrc)
			if err != nil {
				t.Fatalf("SETUP: %v", err)
			}
			if reflectEqualFacts(moved, f) {
				t.Fatalf("SETUP: the planted change did not move the facts: %+v", moved)
			}
			p := pageCreateUsageProblems(usage, moved, floor, designed)
			if len(p) != 1 || !strings.Contains(p[0], tc.want) {
				t.Fatalf("today's usage against facts %+v gave %q, want the %s problem alone", moved, p, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		what      string
		factsSrc  string
		mapperSrc string
		schemaSrc string
	}{
		{"a dropped button link rule", factsSrc, strings.Replace(mapperSrc, "strpos($url, '&')", "strpos($url, '#')", 1), schemaSrc},
		{"a dropped Atomic refusal", strings.Replace(factsSrc, "'detail' => 'atomic_unavailable'", "'detail' => 'atomic_later'", 1), mapperSrc, schemaSrc},
		{"a dropped section cap", factsSrc, mapperSrc, strings.Replace(schemaSrc, "MAX_SECTIONS", "MAX_PAGE_SECTIONS", 1)},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if tc.factsSrc == factsSrc && tc.mapperSrc == mapperSrc && tc.schemaSrc == schemaSrc {
				t.Fatal("SETUP: the plant changed no source")
			}
			if _, err := parseElementorAgentFacts(tc.factsSrc, tc.mapperSrc, tc.schemaSrc); err == nil {
				t.Fatal("the agent's rules parse without it; the usage would keep claiming a rule the agent dropped")
			}
		})
	}
}

func reflectEqualFacts(a, b elementorAgentFacts) bool {
	return a.classicMin == b.classicMin && a.classicMax == b.classicMax && a.maxSections == b.maxSections &&
		strings.Join(a.imageAligns, ",") == strings.Join(b.imageAligns, ",")
}

// ---------------------------------------------------------------------------
// The refusal codes the agent's designed-page tables answer.
//
// The agent answers a designed page's input with a code. The control plane
// reads that code against its closed set (agentcmd.AbilityRunRefusalCodes): a
// code outside it arrives as unknown. It gives the AI a fixed hint for an
// input-related code (mcp.PrecheckRefusalHint): a code without one arrives
// with nothing to act on. Both are checked here for every code the agent's
// designed-page tables answer, read from the agent's own files so that a code
// the agent adds is seen without an edit to this file.
// ---------------------------------------------------------------------------

const (
	agentBuildersDir       = "../agent/includes/abilities/builders/"
	agentOutlineV3File     = agentBuildersDir + "class-outline-v3.php"
	agentPageCreateV3Cases = "../agent/tests/fixtures/ability-run/page-create-v3-cases.json"
	agentPageEditCases     = "../agent/tests/fixtures/ability-run/page-edit-ops-cases.json"
)

// agentDesignedCodeConsts are the constants that spell the refusal codes a
// designed page's checks answer on the site: the design, a font, the page
// frame and its template, containers, a background, and a logo's alt text.
var agentDesignedCodeConsts = []struct{ file, name string }{
	{"class-design-input.php", "CODE_NOT_READY"},
	{"class-design-input.php", "CODE_CONTRAST_LOW"},
	{"class-font-registry.php", "CODE_UNKNOWN"},
	{"class-builder-page-create.php", "CODE_FRAME_UNAVAILABLE"},
	{"class-elementor-document.php", "CODE_TEMPLATE_UNAVAILABLE"},
	{"class-elementor-classic-mapper.php", "CODE_CONTAINERS_REQUIRED"},
	{"class-elementor-section-compiler.php", "CODE_BACKGROUND_NOT_IMAGE"},
	{"class-elementor-pattern-compiler.php", "CODE_ALT_FROM_LIBRARY"},
}

var (
	// OutlineV3's table of what a refusal says first, by code.
	reOutlineSummary    = regexp.MustCompile(`(?s)private const SUMMARY = \[(.*?)\n\s*\];`)
	reOutlineSummaryKey = regexp.MustCompile(`(?m)^\s*'([a-z][a-z0-9_]*)'\s*=>`)
)

// agentV3Sources is the text of everything the agent's codes are read from.
type agentV3Sources struct {
	createCases string            // page-create-v3-cases.json
	editCases   string            // page-edit-ops-cases.json
	outline     string            // class-outline-v3.php
	classes     map[string]string // builder class file name -> source
}

// agentCaseCodes adds, to codes, the refusal codes the rows of the named lists
// of a shared case table answer (the "agent" column; "ok" is an accept), keyed
// to where the first one was found. A list that is missing or answers no
// refusal is an error: a table that lost its refused rows proves nothing.
func agentCaseCodes(codes map[string]string, table, label string, lists ...string) error {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(table), &doc); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	for _, list := range lists {
		raw, ok := doc[list]
		if !ok {
			return fmt.Errorf("%s has no list %q", label, list)
		}
		var rows []struct {
			Name  string `json:"name"`
			Agent string `json:"agent"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("%s %s: %w", label, list, err)
		}
		refused := 0
		for _, r := range rows {
			if r.Agent == "" {
				return fmt.Errorf("%s %s: row %q has no agent answer", label, list, r.Name)
			}
			if r.Agent == "ok" {
				continue
			}
			refused++
			if _, seen := codes[r.Agent]; !seen {
				codes[r.Agent] = label + " " + list + " row " + r.Name
			}
		}
		if refused == 0 {
			return fmt.Errorf("%s %s answers no refusal at all", label, list)
		}
	}
	return nil
}

// agentV3RefusalCodes is every refusal code the agent's designed-page tables
// answer, keyed to the first place it was found: the rows of the page-create
// table and of the page-edit table's version 3 rows, the codes OutlineV3 says
// a refusal first by, and the constants of agentDesignedCodeConsts.
func agentV3RefusalCodes(src agentV3Sources) (map[string]string, error) {
	codes := map[string]string{}
	if err := agentCaseCodes(codes, src.createCases, "page-create-v3-cases.json", "cases"); err != nil {
		return nil, err
	}
	if err := agentCaseCodes(codes, src.editCases, "page-edit-ops-cases.json", "v3_cases"); err != nil {
		return nil, err
	}
	block := reOutlineSummary.FindStringSubmatch(src.outline)
	if block == nil {
		return nil, errors.New("OutlineV3::SUMMARY not found")
	}
	keys := reOutlineSummaryKey.FindAllStringSubmatch(block[1], -1)
	if len(keys) == 0 {
		return nil, errors.New("OutlineV3::SUMMARY holds no code")
	}
	for _, k := range keys {
		if _, seen := codes[k[1]]; !seen {
			codes[k[1]] = "OutlineV3::SUMMARY"
		}
	}
	for _, c := range agentDesignedCodeConsts {
		re := regexp.MustCompile(`(?m)^\s*(?:public |private |protected )?const ` + regexp.QuoteMeta(c.name) + `\s*=\s*'([a-z][a-z0-9_]*)';`)
		m := re.FindStringSubmatch(src.classes[c.file])
		if m == nil {
			return nil, fmt.Errorf("%s::%s not found as a quoted code", c.file, c.name)
		}
		if _, seen := codes[m[1]]; !seen {
			codes[m[1]] = c.file + " " + c.name
		}
	}
	return codes, nil
}

// agentCodeProblems lists every code that the closed set lacks or that has no
// hint of one line.
func agentCodeProblems(codes map[string]string, inClosedSet func(string) bool, hint func(string) string) []string {
	var problems []string
	names := make([]string, 0, len(codes))
	for code := range codes {
		names = append(names, code)
	}
	sort.Strings(names)
	for _, code := range names {
		if !inClosedSet(code) {
			problems = append(problems, fmt.Sprintf("%s (%s) is not in agentcmd.AbilityRunRefusalCodes, so it arrives as unknown", code, codes[code]))
		}
		if h := hint(code); h == "" || strings.Contains(h, "\n") {
			problems = append(problems, fmt.Sprintf("%s (%s) has no one-line hint in the control plane (mcp.PrecheckRefusalHint)", code, codes[code]))
		}
	}
	return problems
}

func closedSetHas(code string) bool {
	_, ok := agentcmd.AbilityRunRefusalCodes[code]
	return ok
}

func readAgentV3Sources(t *testing.T) agentV3Sources {
	t.Helper()
	src := agentV3Sources{
		createCases: readRepoFile(t, agentPageCreateV3Cases),
		editCases:   readRepoFile(t, agentPageEditCases),
		outline:     readRepoFile(t, agentOutlineV3File),
		classes:     map[string]string{},
	}
	for _, c := range agentDesignedCodeConsts {
		if _, ok := src.classes[c.file]; !ok {
			src.classes[c.file] = readRepoFile(t, agentBuildersDir+c.file)
		}
	}
	return src
}

// TestAgentV3RefusalCodesAreKnownWithAHint: every refusal code the agent's
// designed-page tables answer is in the closed set of agent codes and has a
// hint for the AI.
func TestAgentV3RefusalCodesAreKnownWithAHint(t *testing.T) {
	codes, err := agentV3RefusalCodes(readAgentV3Sources(t))
	if err != nil {
		t.Fatalf("the agent's designed-page refusal codes could not be read:\n%v", err)
	}
	// Positive control: codes that only one place of the agent gives. If one
	// is not found the reading lost that place, and nothing below would be
	// checked for it.
	for code, from := range map[string]string{
		"anchor_in_use":             "page-edit-ops-cases.json",
		"link_anchor_missing":       "page-create-v3-cases.json",
		"font_unknown":              "page-create-v3-cases.json",
		"page_template_unavailable": "class-elementor-document.php",
		"image_alt_from_library":    "class-elementor-pattern-compiler.php",
	} {
		if got, ok := codes[code]; !ok || !strings.Contains(got, from) {
			t.Fatalf("the reading of the agent's codes lost %s, which %s gives (found: %q); "+
				"the check below would pass over it", code, from, got)
		}
	}
	if problems := agentCodeProblems(codes, closedSetHas, mcp.PrecheckRefusalHint); len(problems) > 0 {
		t.Fatalf("the agent answers codes the control plane does not know with a hint:\n  - %s", strings.Join(problems, "\n  - "))
	}
}

// TestAgentV3RefusalCodeCheckRefusesDrift: the check above is not vacuous. A
// code the agent adds without the control plane, a code with no hint, a hint
// of two lines, a table that lost its refused rows and a renamed constant are
// each refused for their own reason, and today's codes are not.
func TestAgentV3RefusalCodeCheckRefusesDrift(t *testing.T) {
	src := readAgentV3Sources(t)
	codes, err := agentV3RefusalCodes(src)
	if err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if p := agentCodeProblems(codes, closedSetHas, mcp.PrecheckRefusalHint); len(p) > 0 {
		t.Fatalf("SETUP: today's codes must pass before drift is planted: %v", p)
	}

	t.Run("a code the agent added alone", func(t *testing.T) {
		added := map[string]string{"a_code_the_agent_added": "a planted row"}
		for k, v := range codes {
			added[k] = v
		}
		p := agentCodeProblems(added, closedSetHas, mcp.PrecheckRefusalHint)
		if len(p) != 2 || !strings.Contains(p[0], "AbilityRunRefusalCodes") || !strings.Contains(p[1], "hint") {
			t.Fatalf("a new code gave %q, want one problem for the closed set and one for the hint", p)
		}
	})
	t.Run("a code in the closed set with no hint", func(t *testing.T) {
		bare := func(code string) string {
			if code == "anchor_in_use" {
				return ""
			}
			return mcp.PrecheckRefusalHint(code)
		}
		p := agentCodeProblems(codes, closedSetHas, bare)
		if len(p) != 1 || !strings.Contains(p[0], "anchor_in_use") || !strings.Contains(p[0], "hint") {
			t.Fatalf("a missing hint gave %q, want the hint problem for anchor_in_use alone", p)
		}
	})
	t.Run("a hint of two lines", func(t *testing.T) {
		two := func(code string) string { return mcp.PrecheckRefusalHint(code) + "\nand more" }
		if p := agentCodeProblems(codes, closedSetHas, two); len(p) != len(codes) {
			t.Fatalf("a two-line hint gave %d problems for %d codes", len(p), len(codes))
		}
	})
	t.Run("a table that lost its refused rows", func(t *testing.T) {
		allOK := `{"cases":[{"name":"a","agent":"ok"}],"v3_cases":[{"name":"b","agent":"ok"}]}`
		moved := src
		moved.createCases, moved.editCases = allOK, allOK
		if _, err := agentV3RefusalCodes(moved); err == nil {
			t.Fatal("tables that answer only ok are read as holding every code")
		}
	})
	t.Run("a renamed constant", func(t *testing.T) {
		moved := src
		moved.classes = map[string]string{}
		for k, v := range src.classes {
			moved.classes[k] = v
		}
		moved.classes["class-elementor-document.php"] = strings.Replace(
			src.classes["class-elementor-document.php"], "CODE_TEMPLATE_UNAVAILABLE", "CODE_TEMPLATE_GONE", 1)
		if _, err := agentV3RefusalCodes(moved); err == nil {
			t.Fatal("a renamed code constant is read as holding every code")
		}
	})
}
