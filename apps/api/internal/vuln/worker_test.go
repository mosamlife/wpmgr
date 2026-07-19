package vuln_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// ---------------------------------------------------------------------------
// Feed-record parser tests (white-box via export_test.go)
// ---------------------------------------------------------------------------

// fixtureGoodRecord is a realistic real-shaped v3 Production record. It uses:
//   - "YYYY-MM-DD HH:MM:SS" date format (the real v3 feed format, not RFC3339)
//   - cvss as an object {vector,score,rating} (the real key is "cvss", not "cvss_obj")
//   - copyrights block with both defiant and mitre parties
const fixtureGoodRecord = `{
  "id": "848ccbdc-c6f1-480f-a272-cd459e706713",
  "title": "Example Plugin <= 1.2.3 - Stored XSS",
  "software": [
    {
      "type": "plugin",
      "name": "Example Plugin",
      "slug": "example",
      "affected_versions": {
        "1.0.0 - 1.2.3": {
          "from_version": "1.0.0",
          "from_inclusive": true,
          "to_version": "1.2.3",
          "to_inclusive": true
        }
      },
      "patched": true,
      "patched_versions": ["1.2.4"],
      "remediation": "Update to version 1.2.4, or a newer patched version"
    }
  ],
  "informational": false,
  "description": "An example vulnerability",
  "references": ["https://www.wordfence.com/threat-intel/vulnerabilities/example"],
  "cwe": {"id": 80, "name": "Basic XSS", "description": "..."},
  "cvss": {"vector": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:L/I:L/A:N", "score": 6.5, "rating": "Medium"},
  "cve": "CVE-1998-1000",
  "cve_link": "https://www.cve.org/CVERecord?id=CVE-1998-1000",
  "researchers": ["A. Researcher"],
  "published": "1998-01-09 00:00:00",
  "updated": "2022-08-05 20:14:05",
  "copyrights": {
    "message": "This record contains material that is subject to copyright",
    "defiant": {
      "notice": "Copyright 2012-2023 Defiant Inc.",
      "license": "Defiant grants you a personal, non-exclusive, non-transferable license.",
      "license_url": "https://www.wordfence.com/wti-community-edition-terms-and-conditions/"
    },
    "mitre": {
      "notice": "Copyright 1999-2022 The MITRE Corporation",
      "license": "CVE Usage: MITRE hereby grants you a perpetual, worldwide, non-exclusive.",
      "license_url": "https://www.cve.org/Legal/TermsOfUse"
    }
  }
}`

// fixtureBadDateRecord has a garbage "published" and null "updated". It must
// still ingest (nil dates) — this is the regression guard for the 0-records bug.
const fixtureBadDateRecord = `{
  "id": "11111111-2222-3333-4444-555555555555",
  "title": "Another Plugin - SQLi",
  "software": [
    {
      "type": "plugin",
      "name": "Another",
      "slug": "another",
      "affected_versions": {
        "* - 2.0.0": {
          "from_version": "*",
          "from_inclusive": true,
          "to_version": "2.0.0",
          "to_inclusive": false
        }
      },
      "patched": true,
      "patched_versions": ["2.0.1"]
    }
  ],
  "informational": false,
  "published": "not-a-date",
  "updated": null,
  "cve": "CVE-2020-1234"
}`

// fixtureScannerInfo is scanner-shaped: informational lives at the software
// entry level (not record level). Must surface at record level after parsing.
const fixtureScannerInfo = `{
  "id": "99999999-aaaa-bbbb-cccc-dddddddddddd",
  "title": "Info-only item",
  "software": [
    {
      "type": "plugin",
      "name": "Info",
      "slug": "infoplugin",
      "informational": true,
      "affected_versions": {
        "* - *": {
          "from_version": "*",
          "from_inclusive": true,
          "to_version": "*",
          "to_inclusive": true
        }
      },
      "patched": false,
      "patched_versions": []
    }
  ],
  "published": "2014-04-28 00:00:00"
}`

// TestParseFeedRecord_GoodRecord (Case A): asserts that a realistic v3 Production
// record parses fully — non-nil dates from "YYYY-MM-DD HH:MM:SS", one software
// row, cvss object score+rating, cve, and attribution notices.
func TestParseFeedRecord_GoodRecord(t *testing.T) {
	const vulnID = "848ccbdc-c6f1-480f-a272-cd459e706713"
	rec, defiantNotice, defiantLicense, mitreNotice, err := vuln.ParseFeedRecord(vulnID, json.RawMessage(fixtureGoodRecord))
	if err != nil {
		t.Fatalf("parseFeedRecord returned unexpected error: %v", err)
	}

	// VulnID is taken from the map key, not the record body.
	if rec.VulnID != vulnID {
		t.Errorf("VulnID = %q; want %q", rec.VulnID, vulnID)
	}

	// Dates: must parse "1998-01-09 00:00:00" and "2022-08-05 20:14:05" as UTC.
	if rec.Published == nil {
		t.Error("Published is nil; want non-nil (1998-01-09 00:00:00)")
	} else {
		got := rec.Published.UTC().Format("2006-01-02 15:04:05")
		if got != "1998-01-09 00:00:00" {
			t.Errorf("Published = %q; want %q", got, "1998-01-09 00:00:00")
		}
	}
	if rec.Updated == nil {
		t.Error("Updated is nil; want non-nil (2022-08-05 20:14:05)")
	} else {
		got := rec.Updated.UTC().Format("2006-01-02 15:04:05")
		if got != "2022-08-05 20:14:05" {
			t.Errorf("Updated = %q; want %q", got, "2022-08-05 20:14:05")
		}
	}

	// Software row.
	if len(rec.Software) != 1 {
		t.Fatalf("len(Software) = %d; want 1", len(rec.Software))
	}
	sw := rec.Software[0]
	if sw.Kind != "plugin" {
		t.Errorf("Software[0].Kind = %q; want %q", sw.Kind, "plugin")
	}
	if sw.Slug != "example" {
		t.Errorf("Software[0].Slug = %q; want %q", sw.Slug, "example")
	}

	// AffectedVersions must be stored as an object (not an array).
	avStr := string(sw.AffectedVersions)
	if len(avStr) == 0 || avStr[0] != '{' {
		t.Errorf("AffectedVersions = %q; want a JSON object starting with '{'", avStr)
	}
	// Must contain the from_version field.
	if avStr == "{}" {
		t.Error("AffectedVersions is empty object; want populated range data")
	}

	// CVSS: score 6.5, rating "Medium" — proves the cvss-object fix (real key "cvss").
	if rec.CVSSScore == nil {
		t.Error("CVSSScore is nil; want 6.5 (from cvss object)")
	} else if *rec.CVSSScore != 6.5 {
		t.Errorf("CVSSScore = %v; want 6.5", *rec.CVSSScore)
	}
	if rec.CVSSRating != "Medium" {
		t.Errorf("CVSSRating = %q; want %q", rec.CVSSRating, "Medium")
	}

	// CVE.
	if rec.CVE != "CVE-1998-1000" {
		t.Errorf("CVE = %q; want %q", rec.CVE, "CVE-1998-1000")
	}

	// Attribution.
	if defiantNotice == "" {
		t.Error("defiantNotice is empty; want non-empty")
	}
	if defiantLicense == "" {
		t.Error("defiantLicense is empty; want non-empty")
	}
	if mitreNotice == "" {
		t.Error("mitreNotice is empty; want non-empty")
	}
}

// TestParseFeedRecord_BadDate (Case B): regression guard for the 0-records bug.
// A garbage "published" and null "updated" must NOT drop the record — dates
// default to nil and the rest of the record ingests cleanly.
func TestParseFeedRecord_BadDate(t *testing.T) {
	const vulnID = "11111111-2222-3333-4444-555555555555"
	rec, _, _, _, err := vuln.ParseFeedRecord(vulnID, json.RawMessage(fixtureBadDateRecord))
	if err != nil {
		t.Fatalf("parseFeedRecord returned error %v; want nil (bad date must not drop record)", err)
	}

	// Dates must be nil (unparseable / null), not an error.
	if rec.Published != nil {
		t.Errorf("Published = %v; want nil (unparseable date)", rec.Published)
	}
	if rec.Updated != nil {
		t.Errorf("Updated = %v; want nil (null in JSON)", rec.Updated)
	}

	// The rest of the record must still be present.
	if rec.CVE != "CVE-2020-1234" {
		t.Errorf("CVE = %q; want %q", rec.CVE, "CVE-2020-1234")
	}
	if len(rec.Software) != 1 {
		t.Fatalf("len(Software) = %d; want 1", len(rec.Software))
	}
}

// TestParseFeedRecord_ScannerInformational (Case C): the scanner feed places
// "informational: true" inside each software entry rather than at the record
// level. Parsing must OR-up the software-level flag to record level.
func TestParseFeedRecord_ScannerInformational(t *testing.T) {
	const vulnID = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	rec, _, _, _, err := vuln.ParseFeedRecord(vulnID, json.RawMessage(fixtureScannerInfo))
	if err != nil {
		t.Fatalf("parseFeedRecord returned error %v; want nil", err)
	}

	if !rec.Informational {
		t.Error("Informational = false; want true (OR-ed up from software-level flag)")
	}

	// Published must parse from "2014-04-28 00:00:00".
	if rec.Published == nil {
		t.Error("Published is nil; want non-nil (2014-04-28 00:00:00)")
	} else {
		got := rec.Published.UTC().Format("2006-01-02 15:04:05")
		if got != "2014-04-28 00:00:00" {
			t.Errorf("Published = %q; want %q", got, "2014-04-28 00:00:00")
		}
	}
}

// TestParseFeedRecord_EndToEnd is the end-to-end guard that directly reproduces
// the production symptom: all three fixtures are wrapped in a root JSON object
// (the real feed structure) and decoded through the fetchFeed loop logic.
// The guard asserts len(records) == 3 — i.e. non-zero records — which was the
// failing condition in prod (0 ingested due to the date-parse hard-fail).
func TestParseFeedRecord_EndToEnd(t *testing.T) {
	root := fmt.Sprintf(`{
		"848ccbdc-c6f1-480f-a272-cd459e706713": %s,
		"11111111-2222-3333-4444-555555555555": %s,
		"99999999-aaaa-bbbb-cccc-dddddddddddd": %s
	}`, fixtureGoodRecord, fixtureBadDateRecord, fixtureScannerInfo)

	// Decode the root object exactly as fetchFeed does.
	dec := json.NewDecoder(strings.NewReader(root))

	// Read opening "{".
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tok.(json.Delim) != '{' {
		t.Fatalf("expected '{', got %v", tok)
	}

	var records []vuln.FeedRecord
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		vulnID, ok := keyTok.(string)
		if !ok {
			continue
		}
		var rawMsg json.RawMessage
		if err := dec.Decode(&rawMsg); err != nil {
			t.Fatalf("decode record %s: %v", vulnID, err)
		}
		rec, _, _, _, parseErr := vuln.ParseFeedRecord(vulnID, rawMsg)
		if parseErr != nil {
			if errors.Is(parseErr, vuln.ErrNoUsableSoftware) {
				// Expected for records with no usable software; do not count.
				continue
			}
			t.Errorf("parseFeedRecord(%s) returned unexpected error: %v", vulnID, parseErr)
			continue
		}
		records = append(records, rec)
	}

	// THE KEY ASSERTION: all three fixtures must produce records (non-zero).
	// This directly reproduces the prod symptom where 0 records ingested.
	if len(records) != 3 {
		t.Errorf("len(records) = %d; want 3 (all three fixtures must ingest)", len(records))
	}
}

// ---------------------------------------------------------------------------
// wfTime lenient parsing unit tests
// ---------------------------------------------------------------------------

func TestWfTime_Layouts(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantNil bool
		wantFmt string // expected output in "2006-01-02 15:04:05" if not nil
	}{
		{
			name:    "space_separated_real_format",
			input:   `"2014-04-28 00:00:00"`,
			wantNil: false,
			wantFmt: "2014-04-28 00:00:00",
		},
		{
			name:    "date_only",
			input:   `"2014-04-28"`,
			wantNil: false,
			wantFmt: "2014-04-28 00:00:00",
		},
		{
			name:    "rfc3339",
			input:   `"2014-04-28T00:00:00Z"`,
			wantNil: false,
			wantFmt: "2014-04-28 00:00:00",
		},
		{
			name:    "null_literal",
			input:   `null`,
			wantNil: true,
		},
		{
			name:    "empty_string",
			input:   `""`,
			wantNil: true,
		},
		{
			name:    "garbage_string",
			input:   `"not-a-date"`,
			wantNil: true,
		},
		{
			name:    "non_string_number",
			input:   `12345`,
			wantNil: true,
		},
	}

	// We test wfTime indirectly by encoding it inside a minimal wfRecord-like
	// struct and calling ParseFeedRecord with a record that uses the date.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a minimal valid record with a plugin software entry and the
			// tested date as "published".
			raw := json.RawMessage(fmt.Sprintf(`{
				"title": "test",
				"published": %s,
				"software": [{"type":"plugin","slug":"test-plugin","affected_versions":{},"patched":false,"patched_versions":[]}]
			}`, tc.input))
			rec, _, _, _, err := vuln.ParseFeedRecord("test-vuln-id", raw)
			if err != nil {
				t.Fatalf("parseFeedRecord returned error: %v", err)
			}
			if tc.wantNil {
				if rec.Published != nil {
					t.Errorf("Published = %v; want nil for input %s", rec.Published, tc.input)
				}
			} else {
				if rec.Published == nil {
					t.Fatalf("Published is nil; want non-nil for input %s", tc.input)
				}
				got := rec.Published.UTC().Format("2006-01-02 15:04:05")
				if got != tc.wantFmt {
					t.Errorf("Published = %q; want %q", got, tc.wantFmt)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractCVSS tests
// ---------------------------------------------------------------------------

func TestParseFeedRecord_CVSSObject(t *testing.T) {
	// Verify the cvss-object correction: "cvss" key carries an object, not a scalar.
	raw := json.RawMessage(`{
		"title": "CVSS Object Test",
		"software": [{"type":"plugin","slug":"plug","affected_versions":{},"patched":false,"patched_versions":[]}],
		"cvss": {"vector": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", "score": 9.8, "rating": "Critical"}
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("cvss-test", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if rec.CVSSScore == nil {
		t.Fatal("CVSSScore is nil; want 9.8")
	}
	if *rec.CVSSScore != 9.8 {
		t.Errorf("CVSSScore = %v; want 9.8", *rec.CVSSScore)
	}
	if rec.CVSSRating != "Critical" {
		t.Errorf("CVSSRating = %q; want Critical", rec.CVSSRating)
	}
}

// TestParseFeedRecord_CVSSRatingWithoutScore is the GH #245 follow-up
// hardening guard: an object like {"rating":"Critical"} with no score field
// must NOT be discarded. Before the fix, extractCVSS gated the whole object
// branch on obj.Score != nil, so a rating-without-score record silently lost
// its rating (and would have fallen back into SeverityFromRating's honest
// "unknown" bucket even though a real rating string was present on the wire).
func TestParseFeedRecord_CVSSRatingWithoutScore(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "Rating without score",
		"software": [{"type":"plugin","slug":"plug","affected_versions":{},"patched":false,"patched_versions":[]}],
		"cvss": {"rating": "Critical"}
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("rating-no-score", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if rec.CVSSScore != nil {
		t.Errorf("CVSSScore = %v; want nil (absent from the object)", rec.CVSSScore)
	}
	if rec.CVSSRating != "Critical" {
		t.Errorf("CVSSRating = %q; want %q (must survive a missing score)", rec.CVSSRating, "Critical")
	}
}

// TestParseFeedRecord_CVSSStringifiedScore is the second GH #245 follow-up
// hardening guard: a stringified score ("9.8" instead of 9.8) must not cause
// the WHOLE cvss object unmarshal to fail — json.Unmarshal aborts a struct
// decode on any field type mismatch, so a stringified score previously
// discarded a co-present rating too.
func TestParseFeedRecord_CVSSStringifiedScore(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "Stringified score",
		"software": [{"type":"plugin","slug":"plug","affected_versions":{},"patched":false,"patched_versions":[]}],
		"cvss": {"score": "9.8", "rating": "Critical"}
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("stringified-score", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if rec.CVSSScore == nil {
		t.Fatal("CVSSScore is nil; want 9.8 (stringified score must be tolerated)")
	}
	if *rec.CVSSScore != 9.8 {
		t.Errorf("CVSSScore = %v; want 9.8", *rec.CVSSScore)
	}
	if rec.CVSSRating != "Critical" {
		t.Errorf("CVSSRating = %q; want %q (must survive a stringified score sibling field)", rec.CVSSRating, "Critical")
	}
}

// TestParseFeedRecord_CVSSStringifiedScore_Unparseable verifies a garbage
// stringified score degrades to a nil score (never an error / dropped record)
// while still preserving a co-present rating.
func TestParseFeedRecord_CVSSStringifiedScore_Unparseable(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "Garbage stringified score",
		"software": [{"type":"plugin","slug":"plug","affected_versions":{},"patched":false,"patched_versions":[]}],
		"cvss": {"score": "not-a-number", "rating": "High"}
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("garbage-score", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if rec.CVSSScore != nil {
		t.Errorf("CVSSScore = %v; want nil (unparseable string)", rec.CVSSScore)
	}
	if rec.CVSSRating != "High" {
		t.Errorf("CVSSRating = %q; want %q", rec.CVSSRating, "High")
	}
}

func TestParseFeedRecord_CVSSNull(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "No CVSS",
		"software": [{"type":"plugin","slug":"plug","affected_versions":{},"patched":false,"patched_versions":[]}],
		"cvss": null
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("no-cvss", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if rec.CVSSScore != nil {
		t.Errorf("CVSSScore = %v; want nil for null cvss", rec.CVSSScore)
	}
	if rec.CVSSRating != "" {
		t.Errorf("CVSSRating = %q; want empty for null cvss", rec.CVSSRating)
	}
}

// ---------------------------------------------------------------------------
// Essential-field-rule tests
// ---------------------------------------------------------------------------

func TestParseFeedRecord_NoSoftware_ReturnsErrNoUsableSoftware(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "No software record",
		"software": []
	}`)
	_, _, _, _, err := vuln.ParseFeedRecord("no-sw", raw)
	if !errors.Is(err, vuln.ErrNoUsableSoftware) {
		t.Errorf("err = %v; want ErrNoUsableSoftware", err)
	}
}

func TestParseFeedRecord_UnknownType_SkipsRow(t *testing.T) {
	// A software entry with an unknown type must be skipped (not the record).
	// If ALL entries have unknown types, the record is skipped via errNoUsableSoftware.
	raw := json.RawMessage(`{
		"title": "Unknown type",
		"software": [
			{"type":"unknown-future-type","slug":"some-slug","affected_versions":{},"patched":false,"patched_versions":[]},
			{"type":"plugin","slug":"valid","affected_versions":{},"patched":false,"patched_versions":[]}
		]
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("unknown-type", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if len(rec.Software) != 1 {
		t.Errorf("len(Software) = %d; want 1 (only the valid plugin row)", len(rec.Software))
	}
	if rec.Software[0].Slug != "valid" {
		t.Errorf("Software[0].Slug = %q; want %q", rec.Software[0].Slug, "valid")
	}
}

func TestParseFeedRecord_EmptySlug_SkipsRow(t *testing.T) {
	// A software entry with an empty slug must be skipped.
	raw := json.RawMessage(`{
		"title": "Empty slug",
		"software": [
			{"type":"plugin","slug":"","affected_versions":{},"patched":false,"patched_versions":[]}
		]
	}`)
	_, _, _, _, err := vuln.ParseFeedRecord("empty-slug", raw)
	if !errors.Is(err, vuln.ErrNoUsableSoftware) {
		t.Errorf("err = %v; want ErrNoUsableSoftware (empty slug drops the only row)", err)
	}
}

// TestParseFeedRecord_SlugNormalised verifies that a mixed-case slug is
// lower-cased on ingest so it matches inventory lookups consistently.
func TestParseFeedRecord_SlugNormalised(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "Slug case test",
		"software": [{"type":"plugin","slug":"WooCommerce","affected_versions":{},"patched":false,"patched_versions":[]}]
	}`)
	rec, _, _, _, err := vuln.ParseFeedRecord("slug-norm", raw)
	if err != nil {
		t.Fatalf("parseFeedRecord error: %v", err)
	}
	if len(rec.Software) != 1 {
		t.Fatalf("len(Software) = %d; want 1", len(rec.Software))
	}
	if rec.Software[0].Slug != "woocommerce" {
		t.Errorf("Slug = %q; want %q", rec.Software[0].Slug, "woocommerce")
	}
}
