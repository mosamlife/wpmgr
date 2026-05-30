package backup

// restore_run_dto_test.go — unit tests for the restoreRunDTO builder and the
// triggered_by_email / triggered_by_name resolution logic. No database needed.

import (
	"testing"

	"github.com/google/uuid"
)

// testRun returns a minimal RestoreRun for DTO tests.
func testRun(triggeredBy string) RestoreRun {
	return RestoreRun{
		ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		SiteID:      uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		SnapshotID:  uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		Mode:        "full",
		Components:  []string{"files", "db"},
		Status:      "completed",
		TriggeredBy: triggeredBy,
	}
}

// TestToRestoreRunDTOWithActor verifies that triggered_by_email and
// triggered_by_name are populated/null correctly.
func TestToRestoreRunDTOWithActor(t *testing.T) {
	run := testRun("11111111-1111-1111-1111-111111111111")

	t.Run("email and name populated when resolved", func(t *testing.T) {
		dto := toRestoreRunDTOWithActor(run, "alice@example.com", "Alice")
		if dto.TriggeredByEmail == nil || *dto.TriggeredByEmail != "alice@example.com" {
			t.Errorf("TriggeredByEmail: want alice@example.com, got %v", dto.TriggeredByEmail)
		}
		if dto.TriggeredByName == nil || *dto.TriggeredByName != "Alice" {
			t.Errorf("TriggeredByName: want Alice, got %v", dto.TriggeredByName)
		}
		if dto.TriggeredBy != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("raw TriggeredBy must be preserved: got %q", dto.TriggeredBy)
		}
	})

	t.Run("email and name null when not resolved (empty strings)", func(t *testing.T) {
		dto := toRestoreRunDTOWithActor(run, "", "")
		if dto.TriggeredByEmail != nil {
			t.Errorf("TriggeredByEmail should be nil, got %v", dto.TriggeredByEmail)
		}
		if dto.TriggeredByName != nil {
			t.Errorf("TriggeredByName should be nil, got %v", dto.TriggeredByName)
		}
		// Raw triggered_by still preserved.
		if dto.TriggeredBy != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("raw TriggeredBy: got %q", dto.TriggeredBy)
		}
	})

	t.Run("run with no triggered_by leaves both null", func(t *testing.T) {
		r := testRun("")
		dto := toRestoreRunDTOWithActor(r, "", "")
		if dto.TriggeredBy != "" {
			t.Errorf("TriggeredBy should be empty, got %q", dto.TriggeredBy)
		}
		if dto.TriggeredByEmail != nil {
			t.Errorf("TriggeredByEmail should be nil, got %v", dto.TriggeredByEmail)
		}
	})

	t.Run("components nil becomes empty slice", func(t *testing.T) {
		r := testRun("")
		r.Components = nil
		dto := toRestoreRunDTOWithActor(r, "", "")
		if dto.Components == nil {
			t.Error("components should be empty slice, not nil")
		}
		if len(dto.Components) != 0 {
			t.Errorf("components should be empty, got %v", dto.Components)
		}
	})
}

// TestToRestoreRunDTO verifies that the zero-actor wrapper delegates correctly.
func TestToRestoreRunDTO(t *testing.T) {
	run := testRun("some-id")
	dto := toRestoreRunDTO(run)
	// No resolution wired → both should be nil.
	if dto.TriggeredByEmail != nil {
		t.Errorf("TriggeredByEmail should be nil, got %v", dto.TriggeredByEmail)
	}
	if dto.TriggeredByName != nil {
		t.Errorf("TriggeredByName should be nil, got %v", dto.TriggeredByName)
	}
	if dto.TriggeredBy != "some-id" {
		t.Errorf("raw TriggeredBy: got %q", dto.TriggeredBy)
	}
}
