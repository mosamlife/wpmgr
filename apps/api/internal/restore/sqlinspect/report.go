// Package sqlinspect produces a structured projection of a mysqldump-style SQL
// dump: per-table inventory, charset, table prefix, and WordPress hints
// (siteurl / home / db_version probed from a wp_options multi-row INSERT).
//
// Two consumers exist:
//
//   - The CP-legacy fallback path streams a snapshot's DB artifact through
//     Inspect when no agent-generated inspection JSON ships with the backup.
//   - Test fixtures and golden-file tests exercise Inspect against small
//     synthetic dumps to keep the regex/scanner logic honest.
//
// The Report wire shape is also the OpenAPI SqlInspection schema; bump
// SchemaVersion when fields change so older clients can negotiate.
package sqlinspect

import "time"

// ReportSchemaVersion is the wire-shape version of Report. The handler emits
// this in every Report it returns; client code should ignore unknown fields and
// fail gracefully on a higher schema version than it knows.
const ReportSchemaVersion = 1

// Source values for Report.Source.
const (
	// SourceAgent — the report was produced by the agent at backup time and
	// returned verbatim (manifest entry of kind="inspection" or
	// path="sql-inspection.json"). Cheap, always-correct.
	SourceAgent = "agent"
	// SourceCPLegacy — the CP streamed the DB artifact through the legacy
	// parser. Fallback for older snapshots that pre-date agent inspection.
	SourceCPLegacy = "cp-legacy"
)

// Report is the JSON projection of a SQL dump's structure and (where it looks
// like a WordPress install) the canonical wp_options values that identify the
// site. Field names mirror the OpenAPI SqlInspection schema exactly so the
// handler can json.Marshal a Report straight to the wire.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	DumpBytes     int64     `json:"dump_bytes"`
	Charset       string    `json:"charset"`
	Collation     string    `json:"collation"`
	TablePrefix   string    `json:"table_prefix"`
	WPVersion     string    `json:"wp_version,omitempty"`
	SiteURL       string    `json:"siteurl,omitempty"`
	HomeURL       string    `json:"home,omitempty"`
	Tables        []Table   `json:"tables"`
	IsWordPress   bool      `json:"is_wordpress"`
	Warnings      []string  `json:"parser_warnings,omitempty"`
	Truncated     bool      `json:"truncated,omitempty"`
	GeneratedAt   time.Time `json:"generated_at"`
	Source        string    `json:"source"` // "agent" | "cp-legacy"
}

// Table is one CREATE TABLE plus a row-count estimate derived from its
// INSERT-VALUES tuples. Counts are estimates because:
//
//   - Multi-row INSERTs are counted by tuple, but a malformed/oversized tuple
//     line is skipped (logged via Warnings, not aborted).
//   - mysqldump emits AUTO_INCREMENT and DEFAULT CHARSET in the CREATE TABLE
//     trailer; the scanner extracts both with anchored regexes.
type Table struct {
	Name          string `json:"name"`
	Rows          int64  `json:"rows_estimate"`
	Bytes         int64  `json:"bytes_estimate"`
	AutoIncrement int64  `json:"auto_increment,omitempty"`
	Charset       string `json:"charset,omitempty"`
	HasFK         bool   `json:"has_fk,omitempty"`
}
