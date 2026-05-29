package sqlinspect

import (
	"context"
	"strings"
	"testing"
)

// wpDump is a small synthetic mysqldump-style fixture that mimics the
// portions of a real WordPress dump our scanner cares about: the SET NAMES
// header, two CREATE TABLE blocks with realistic trailers, a multi-row
// wp_options INSERT carrying siteurl/home/db_version, and a second INSERT
// into wp_users to exercise the non-options path. Comments and BEGIN/COMMIT
// blocks are present so the scanner walks the line types it would see in
// the wild.
const wpDump = `-- WPMgr synthetic dump (test fixture, NOT a real export)
/*!40101 SET NAMES utf8mb4 */;
SET FOREIGN_KEY_CHECKS=0;

CREATE TABLE ` + "`wp_options`" + ` (
  ` + "`option_id`" + ` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  ` + "`option_name`" + ` varchar(191) NOT NULL DEFAULT '',
  ` + "`option_value`" + ` longtext NOT NULL,
  ` + "`autoload`" + ` varchar(20) NOT NULL DEFAULT 'yes',
  PRIMARY KEY (` + "`option_id`" + `),
  UNIQUE KEY ` + "`option_name`" + ` (` + "`option_name`" + `)
) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO ` + "`wp_options`" + ` VALUES (1,'siteurl','https://example.com','yes'),(2,'home','https://example.com/blog','yes'),(3,'blogname','Example','yes'),(4,'db_version','58975','yes');

CREATE TABLE ` + "`wp_users`" + ` (
  ` + "`ID`" + ` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  ` + "`user_login`" + ` varchar(60) NOT NULL DEFAULT '',
  ` + "`user_email`" + ` varchar(100) NOT NULL DEFAULT '',
  PRIMARY KEY (` + "`ID`" + `),
  CONSTRAINT ` + "`wp_users_email_fk`" + ` FOREIGN KEY (` + "`user_email`" + `) REFERENCES ` + "`wp_options`" + ` (` + "`option_name`" + `)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4;

INSERT INTO ` + "`wp_users`" + ` VALUES (1,'admin','admin@example.com');

SET FOREIGN_KEY_CHECKS=1;
`

// nonWPDump is a deliberately bare-bones non-WordPress dump: one CREATE
// TABLE without an "options" suffix, one INSERT, no SET NAMES, no
// wp_options-shaped tuples. The scanner should report IsWordPress=false,
// no siteurl/home/db_version, and a single-table inventory.
const nonWPDump = `CREATE TABLE ` + "`foo`" + ` (
  ` + "`id`" + ` int(11) NOT NULL,
  ` + "`name`" + ` varchar(64) NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
) ENGINE=InnoDB DEFAULT CHARSET=latin1;

INSERT INTO ` + "`foo`" + ` VALUES (1,'alpha'),(2,'beta'),(3,'gamma');
`

func TestInspect_WordPressDump(t *testing.T) {
	t.Parallel()
	r, err := Inspect(context.Background(), strings.NewReader(wpDump))
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}

	if r.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, ReportSchemaVersion)
	}
	if !r.IsWordPress {
		t.Errorf("IsWordPress = false, want true (wp_options present, prefix wp_)")
	}
	if r.TablePrefix != "wp_" {
		t.Errorf("TablePrefix = %q, want %q", r.TablePrefix, "wp_")
	}
	if r.Charset != "utf8mb4" {
		t.Errorf("Charset = %q, want %q", r.Charset, "utf8mb4")
	}
	if r.Collation != "utf8mb4_unicode_ci" {
		t.Errorf("Collation = %q, want %q", r.Collation, "utf8mb4_unicode_ci")
	}
	if r.SiteURL != "https://example.com" {
		t.Errorf("SiteURL = %q, want %q", r.SiteURL, "https://example.com")
	}
	if r.HomeURL != "https://example.com/blog" {
		t.Errorf("HomeURL = %q, want %q", r.HomeURL, "https://example.com/blog")
	}
	if r.WPVersion != "58975" {
		t.Errorf("WPVersion = %q, want %q", r.WPVersion, "58975")
	}

	if len(r.Tables) != 2 {
		t.Fatalf("Tables = %d, want 2 (%+v)", len(r.Tables), r.Tables)
	}
	byName := map[string]Table{}
	for _, tbl := range r.Tables {
		byName[tbl.Name] = tbl
	}
	opt, ok := byName["wp_options"]
	if !ok {
		t.Fatalf("wp_options missing from Tables")
	}
	if opt.Rows != 4 {
		t.Errorf("wp_options Rows = %d, want 4", opt.Rows)
	}
	if opt.AutoIncrement != 42 {
		t.Errorf("wp_options AutoIncrement = %d, want 42", opt.AutoIncrement)
	}
	if opt.Charset != "utf8mb4" {
		t.Errorf("wp_options Charset = %q, want utf8mb4", opt.Charset)
	}
	if opt.HasFK {
		t.Errorf("wp_options HasFK = true, want false")
	}

	users, ok := byName["wp_users"]
	if !ok {
		t.Fatalf("wp_users missing from Tables")
	}
	if users.Rows != 1 {
		t.Errorf("wp_users Rows = %d, want 1", users.Rows)
	}
	if !users.HasFK {
		t.Errorf("wp_users HasFK = false, want true (CONSTRAINT FOREIGN KEY)")
	}
	if users.AutoIncrement != 2 {
		t.Errorf("wp_users AutoIncrement = %d, want 2", users.AutoIncrement)
	}

	if r.DumpBytes <= 0 {
		t.Errorf("DumpBytes = %d, want > 0", r.DumpBytes)
	}
	if r.Truncated {
		t.Errorf("Truncated = true on a complete dump; want false")
	}
	if r.Source != "" {
		t.Errorf("Source = %q at parser layer; the handler should stamp this, not Inspect", r.Source)
	}
}

func TestInspect_NonWordPressDump(t *testing.T) {
	t.Parallel()
	r, err := Inspect(context.Background(), strings.NewReader(nonWPDump))
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}

	if r.IsWordPress {
		t.Errorf("IsWordPress = true, want false (no <prefix>options table)")
	}
	if r.SiteURL != "" {
		t.Errorf("SiteURL = %q, want empty (no wp_options)", r.SiteURL)
	}
	if r.HomeURL != "" {
		t.Errorf("HomeURL = %q, want empty (no wp_options)", r.HomeURL)
	}
	if r.WPVersion != "" {
		t.Errorf("WPVersion = %q, want empty (no wp_options)", r.WPVersion)
	}

	if len(r.Tables) != 1 {
		t.Fatalf("Tables = %d, want 1 (%+v)", len(r.Tables), r.Tables)
	}
	foo := r.Tables[0]
	if foo.Name != "foo" {
		t.Errorf("Tables[0].Name = %q, want %q", foo.Name, "foo")
	}
	if foo.Rows != 3 {
		t.Errorf("foo Rows = %d, want 3", foo.Rows)
	}
	if foo.Charset != "latin1" {
		t.Errorf("foo Charset = %q, want latin1", foo.Charset)
	}

	// TablePrefix: "foo" has no underscore → "" is the documented contract.
	if r.TablePrefix != "" {
		t.Errorf("TablePrefix = %q, want empty (foo has no underscore)", r.TablePrefix)
	}
}

// TestCountTuples_QuoteAware exercises the tuple counter against tricky
// payloads that would trip a naive paren counter. Each row in the table is
// the VALUES portion of a hypothetical INSERT line.
func TestCountTuples_QuoteAware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{
			name: "simple multi-row",
			in:   "(1,'a'),(2,'b'),(3,'c')",
			want: 3,
		},
		{
			name: "paren inside single-quoted string",
			in:   "(1,'has (paren) inside'),(2,'no problem')",
			want: 2,
		},
		{
			name: "escaped quote inside string",
			in:   `(1,'O\'Reilly'),(2,'plain')`,
			want: 2,
		},
		{
			name: "paren inside double-quoted string",
			in:   `(1,"with (paren)"),(2,"plain")`,
			want: 2,
		},
		{
			name: "empty",
			in:   ``,
			want: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := countTuples(tc.in)
			if got != tc.want {
				t.Errorf("countTuples(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
