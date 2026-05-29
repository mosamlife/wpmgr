package sqlinspect

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// optionLineByteCap bounds the bytes the scanner is willing to spend tuple-
// parsing a single wp_options INSERT line. mysqldump's extended-INSERT lines
// can reach a megabyte; option-value scanning is quadratic in the worst case
// (regex over the full tuple body), so we cap each line individually rather
// than risk turning a single oversized line into a CPU loop.
const optionLineByteCap = 1 * 1024 * 1024 // 1 MiB

// cancellationCheckEvery is the line count between context-cancellation polls.
// The dump scan is otherwise CPU-bound and would not notice a cancelled ctx
// until io.EOF; checking every N lines bounds tail latency on cancel to ~one
// I/O batch (typically <100 ms on a real dump).
const cancellationCheckEvery = 1024

// Anchored regexes — all of them match against a single dump line. Keeping
// them line-anchored is what lets the scanner stream a multi-GB dump in
// constant memory: we never have to backtrack across line boundaries.
var (
	// CREATE TABLE `name` — captures the bare table name. mysqldump always
	// quotes the table identifier with backticks even when the name is plain.
	reCreateTable = regexp.MustCompile("^CREATE TABLE `([^`]+)`")

	// CREATE TABLE trailer:  ) ENGINE=... AUTO_INCREMENT=N ... DEFAULT CHARSET=x COLLATE=y;
	// Each subgroup is optional because mysqldump emits a different subset
	// depending on table options (no AUTO_INCREMENT for tables without one,
	// COLLATE omitted when it matches the charset default, etc.).
	reTableTrailer       = regexp.MustCompile(`^\) ENGINE=`)
	reTrailerAutoIncr    = regexp.MustCompile(`AUTO_INCREMENT=(\d+)`)
	reTrailerCharset     = regexp.MustCompile(`DEFAULT CHARSET=(\w+)`)
	reTrailerCollate     = regexp.MustCompile(`COLLATE=(\w+)`)
	reHeaderCharsetMySQL = regexp.MustCompile(`^/\*!40101 SET NAMES (\w+)`)
	reHeaderCharsetPlain = regexp.MustCompile(`^SET NAMES (\w+)`)
	reInsertInto         = regexp.MustCompile("^INSERT INTO `([^`]+)` VALUES")

	// Inside the CREATE TABLE body — anchored to start-of-line OR appearing
	// after some whitespace + comma; we only care that the keyword appears in
	// the table block at all. Case-insensitive: mysqldump emits uppercase but
	// hand-rolled dumps may not.
	reForeignKey = regexp.MustCompile(`(?i)FOREIGN KEY`)
)

// Inspect streams a mysqldump-style SQL dump from r and returns a structured
// Report. It honors ctx cancellation (returning ctx.Err()) and bounds its
// memory + CPU budget so a maliciously oversized dump cannot wedge the worker.
//
// Streaming design (per Research C):
//
//   - bufio.Reader.ReadString('\n') (NOT bufio.Scanner — extended-INSERT lines
//     routinely exceed bufio.Scanner's 64 KiB default and grow() costs would
//     dominate).
//   - Anchored regexes against each line; no backtracking across lines.
//   - ctx.Err() is checked every cancellationCheckEvery lines, NOT every line
//     (the ctx.Err() call dominates a tight loop on otherwise-uninteresting
//     header/index/lock-table lines).
//
// The returned Report's Source is empty — the caller (handler or worker) stamps
// it to "agent" or "cp-legacy" depending on which path produced the bytes.
func Inspect(ctx context.Context, r io.Reader) (*Report, error) {
	if r == nil {
		return nil, errors.New("sqlinspect: nil reader")
	}

	// 64 KiB read buffer balances syscall overhead with per-line allocation:
	// long INSERT lines stay readable in a few iterations of ReadString.
	br := bufio.NewReaderSize(r, 64*1024)

	report := &Report{
		SchemaVersion: ReportSchemaVersion,
		Tables:        nil,
		GeneratedAt:   time.Now().UTC(),
	}

	// We track tables by name in a map for O(1) trailer/insert lookup, and
	// emit them in CREATE order at the end so the wire order is deterministic.
	byName := map[string]*Table{}
	order := []string{}
	var current *Table

	// In-CREATE-TABLE-block state — once we see CREATE TABLE we look for
	// FOREIGN KEY inside the body and the trailer line. The body ends at the
	// trailer (which starts with `) ENGINE=`).
	inCreateBlock := false

	var lineNum int64
	for {
		lineNum++
		if lineNum%cancellationCheckEvery == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return report, cerr
			}
		}

		line, err := br.ReadString('\n')
		// Always process whatever bytes ReadString returned, even on err==EOF:
		// the final line of a dump frequently lacks a trailing newline.
		if len(line) > 0 {
			report.DumpBytes += int64(len(line))
			processLine(line, report, byName, &order, &current, &inCreateBlock)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return report, err
		}
	}

	// Finalise the table slice in CREATE order.
	report.Tables = make([]Table, 0, len(order))
	for _, name := range order {
		report.Tables = append(report.Tables, *byName[name])
	}

	// Table prefix detection: most-common prefix-before-first-underscore
	// across all tables. Returns "" if no underscore-bearing tables exist.
	report.TablePrefix = detectPrefix(order)

	// WordPress detection: prefix is non-empty AND `<prefix>options` table is
	// present. Per Research C, this is the strongest single signal.
	if report.TablePrefix != "" {
		if _, ok := byName[report.TablePrefix+"options"]; ok {
			report.IsWordPress = true
		}
	}

	return report, nil
}

// processLine routes a single dump line to the appropriate state-machine
// branch. Split out so callers/tests can exercise it in isolation if needed.
func processLine(
	line string,
	report *Report,
	byName map[string]*Table,
	order *[]string,
	current **Table,
	inCreateBlock *bool,
) {
	// Header charset (`/*!40101 SET NAMES utf8mb4` and the plain variant).
	// Capture from whichever fires first; the second is a no-op if the first
	// already set it.
	if report.Charset == "" {
		if m := reHeaderCharsetMySQL.FindStringSubmatch(line); m != nil {
			report.Charset = m[1]
		} else if m := reHeaderCharsetPlain.FindStringSubmatch(line); m != nil {
			report.Charset = m[1]
		}
	}

	// CREATE TABLE — opens a new Table and the block tracking it.
	if m := reCreateTable.FindStringSubmatch(line); m != nil {
		name := m[1]
		t := &Table{Name: name}
		byName[name] = t
		*order = append(*order, name)
		*current = t
		*inCreateBlock = true
		return
	}

	// Inside a CREATE TABLE body: detect FOREIGN KEY mentions (case-
	// insensitive) and the closing trailer line.
	if *inCreateBlock {
		// Trailer first — terminates the block.
		if reTableTrailer.MatchString(line) {
			if *current != nil {
				applyTrailer(line, *current, report)
			}
			*current = nil
			*inCreateBlock = false
			return
		}
		if *current != nil && reForeignKey.MatchString(line) {
			(*current).HasFK = true
		}
		return
	}

	// INSERT INTO `name` VALUES ... — count tuples, opportunistically scan
	// for wp_options canonical entries.
	if m := reInsertInto.FindStringSubmatch(line); m != nil {
		name := m[1]
		t, ok := byName[name]
		if !ok {
			// Insert into a table we didn't see CREATE for (split-file dump
			// fragments, partial restore artifacts, etc.). Create it on the
			// fly so the inventory at least lists the row count.
			t = &Table{Name: name}
			byName[name] = t
			*order = append(*order, name)
		}
		rows := countTuples(line)
		t.Rows += rows
		t.Bytes += int64(len(line))

		// wp_options option-row probing: only when the prefix is known to be
		// "wp_" OR the table name ends in "options" (covers the common
		// custom-prefix case). Bounded so an oversized line cannot CPU-wedge
		// the parser.
		if isOptionsTable(name) && len(line) <= optionLineByteCap {
			scanOptionsLine(line, report)
		} else if isOptionsTable(name) {
			report.Warnings = append(report.Warnings,
				"wp_options INSERT line exceeded "+strconv.Itoa(optionLineByteCap)+" bytes — skipped option scan",
			)
		}
		return
	}
}

// applyTrailer extracts the AUTO_INCREMENT, DEFAULT CHARSET, and COLLATE
// values from a CREATE TABLE trailer line and applies them to the current
// table (and to the report's default collation when present).
func applyTrailer(line string, t *Table, report *Report) {
	if m := reTrailerAutoIncr.FindStringSubmatch(line); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			t.AutoIncrement = n
		}
	}
	if m := reTrailerCharset.FindStringSubmatch(line); m != nil {
		t.Charset = m[1]
	}
	if m := reTrailerCollate.FindStringSubmatch(line); m != nil && report.Collation == "" {
		// First-seen wins for the report-level Collation; per-table collation
		// isn't tracked separately in the wire shape today.
		report.Collation = m[1]
	}
}

// countTuples returns the number of top-level `(...)` tuples in the VALUES
// portion of an INSERT line. The scanner is quote- and escape-aware so it
// doesn't miscount a literal "(" inside a quoted string.
//
// Algorithm: walk the line, track string-quoting (' or " — both are valid in
// MySQL string literals) and the backslash-escape state, and count opening
// parens at depth 1 that are NOT inside a quoted string.
func countTuples(line string) int64 {
	const (
		stateNormal = iota
		stateInSingle
		stateInDouble
	)
	state := stateNormal
	depth := 0
	var tuples int64
	escape := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escape {
			escape = false
			continue
		}
		switch state {
		case stateNormal:
			switch c {
			case '\'':
				state = stateInSingle
			case '"':
				state = stateInDouble
			case '(':
				depth++
				if depth == 1 {
					tuples++
				}
			case ')':
				if depth > 0 {
					depth--
				}
			}
		case stateInSingle:
			if c == '\\' {
				escape = true
			} else if c == '\'' {
				state = stateNormal
			}
		case stateInDouble:
			if c == '\\' {
				escape = true
			} else if c == '"' {
				state = stateNormal
			}
		}
	}
	return tuples
}

// isOptionsTable reports whether a table name looks like a WordPress options
// table. The conservative match is suffix "options" — that catches the default
// "wp_options" plus any custom-prefix install. There is no separate
// "subscriber_options" or similar in the WP schema to worry about, but a
// hand-rolled non-WP table happening to end in "options" will trigger an
// option-row scan that simply won't find siteurl/home/db_version (no harm).
func isOptionsTable(name string) bool {
	return strings.HasSuffix(name, "options")
}

// optionRegexes maps wp_options option_name values we care about to the
// regex that pulls the option_value out of a tuple like
// (123,'siteurl','https://example.com','yes'). Group 1 is the option_value.
//
// Pattern shape:
//
//   - Leading `(` and a numeric id (or quoted id — older WP exports vary).
//   - The option_name as a quoted literal.
//   - The option_value as a single-quoted literal — we allow backslash-
//     escaped quotes inside via `(?:\\.|[^'])*`.
//
// We compile these once at package init for the hot-path scan.
var optionRegexes = map[string]*regexp.Regexp{
	"siteurl":    regexp.MustCompile(`\((?:\d+|'\d+'),'siteurl','((?:\\.|[^'])*)'`),
	"home":       regexp.MustCompile(`\((?:\d+|'\d+'),'home','((?:\\.|[^'])*)'`),
	"db_version": regexp.MustCompile(`\((?:\d+|'\d+'),'db_version','((?:\\.|[^'])*)'`),
}

// scanOptionsLine pulls the three canonical WordPress option_values from a
// single wp_options INSERT line and applies them to the report. First-seen
// wins — a multi-row INSERT will list each option once, but a malformed dump
// could conceivably repeat them.
func scanOptionsLine(line string, report *Report) {
	if report.SiteURL == "" {
		if m := optionRegexes["siteurl"].FindStringSubmatch(line); m != nil {
			report.SiteURL = unescapeSQL(m[1])
		}
	}
	if report.HomeURL == "" {
		if m := optionRegexes["home"].FindStringSubmatch(line); m != nil {
			report.HomeURL = unescapeSQL(m[1])
		}
	}
	if report.WPVersion == "" {
		if m := optionRegexes["db_version"].FindStringSubmatch(line); m != nil {
			report.WPVersion = unescapeSQL(m[1])
		}
	}
}

// unescapeSQL undoes mysqldump's single-quote escaping for the small set of
// sequences it actually emits inside string literals (\\, \', \", \n, \r, \t,
// \0). We don't aim for full SQL standard compatibility — these option values
// are URLs and version strings; the worst case is a Punycode URL with a
// literal backslash, which renders unchanged either way.
func unescapeSQL(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			case '\\', '\'', '"':
				b.WriteByte(next)
			default:
				// Unknown escape: preserve both bytes verbatim — safer than
				// silently dropping the backslash.
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// detectPrefix picks the most common prefix (chars up to the first underscore)
// across the table set. Ties are broken by lexicographic order on the prefix
// so the output is deterministic for any given input. Returns "" when no
// table contains an underscore.
func detectPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, n := range names {
		// Skip empty names (defensive; CREATE TABLE always has a name).
		if n == "" {
			continue
		}
		idx := strings.Index(n, "_")
		if idx <= 0 {
			continue
		}
		// Include the underscore in the prefix: "wp_" rather than "wp". This
		// matches WP's own convention ($wpdb->prefix is "wp_") and lets the
		// caller concatenate prefix+"options" directly.
		prefix := n[:idx+1]
		counts[prefix]++
	}
	if len(counts) == 0 {
		return ""
	}
	// Pick the prefix with the highest count; ties → lexicographic min.
	type kv struct {
		prefix string
		count  int
	}
	all := make([]kv, 0, len(counts))
	for p, c := range counts {
		all = append(all, kv{p, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].prefix < all[j].prefix
	})
	return all[0].prefix
}
