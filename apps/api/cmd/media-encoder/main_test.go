package main

// main_test.go — GH #205 regression lock: the media-encoder MUST refuse to
// boot on the API's default/public River schema, since this binary runs
// workers and is therefore a leadership candidate on whatever schema it
// connects to. Both assertions run before run() touches the DB or S3, so
// they need no external services.

import (
	"strings"
	"testing"
)

// TestRun_DefaultSchemaRefused verifies run() returns a clear envError before
// ever dialing the database or object storage when WPMGR_RIVER_MEDIA_SCHEMA
// resolves to the default/public schema (unset, empty, or "public").
func TestRun_DefaultSchemaRefused(t *testing.T) {
	for _, schema := range []string{"", "public", "PUBLIC"} {
		t.Run("schema="+schema, func(t *testing.T) {
			t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", schema)
			err := run()
			if err == nil {
				t.Fatal("run() = nil error, want a refusal for the default/public River schema")
			}
			if !strings.Contains(err.Error(), "WPMGR_RIVER_MEDIA_SCHEMA") || !strings.Contains(err.Error(), "GH #205") {
				t.Fatalf("run() error = %q, want the GH #205 dedicated-schema refusal", err.Error())
			}
		})
	}
}

// TestRun_DedicatedSchemaPassesGate verifies a real dedicated schema clears
// the GH #205 gate: run() proceeds past the schema check into the next
// startup guard (S3 configuration) instead of refusing on schema grounds.
func TestRun_DedicatedSchemaPassesGate(t *testing.T) {
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "media_encoder")
	t.Setenv("WPMGR_S3_BUCKET", "")
	err := run()
	if err == nil {
		t.Fatal("run() = nil error, want the (unrelated) S3-required error in this unconfigured test env")
	}
	if strings.Contains(err.Error(), "GH #205") {
		t.Fatalf("run() error = %q, want the schema gate to have passed (expected the S3 error instead)", err.Error())
	}
	if !strings.Contains(err.Error(), "WPMGR_S3_BUCKET") {
		t.Fatalf("run() error = %q, want the S3-required error", err.Error())
	}
}
