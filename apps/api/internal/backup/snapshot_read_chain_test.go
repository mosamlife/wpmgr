package backup

// snapshot_read_chain_test.go — guards the ADR-048 incremental-visibility read
// path. The CRITICAL bug this protects against: the sqlc-generated
// GetBackupSnapshot / ListBackupSnapshotsForSite queries expanded SELECT * to
// the PRE-m44 column list, so is_incremental / generation / chain_id /
// parent_snapshot_id / base_snapshot_id were never selected and every snapshot
// serialized as is_incremental=false / generation=0 — the badge could never
// show incremental.
//
// GetSnapshot / GetSnapshotScoped / ListSnapshotsForSite now read via
// snapshotSelectColumns + scanSnapshotWithChainFields. This test feeds a row
// carrying is_incremental=true, generation=2, chain_id set through that scan
// helper (the read path) into Snapshot and then through toAPISnapshot (the API
// path), asserting the chain fields survive end to end. The SELECT column
// ordering is locked to scanSnapshotWithChainFields' Scan() arg order via the
// shared snapshotSelectColumns constant.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// fakeSnapshotRow mimics a pgx row for the snapshotSelectColumns projection. It
// assigns the supplied column values to the Scan destinations in the exact
// order scanSnapshotWithChainFields expects, so a mismatch in column order or
// destination type surfaces here just as it would against a real database.
type fakeSnapshotRow struct {
	cols []any
}

func (r *fakeSnapshotRow) Scan(dest ...any) error {
	if len(dest) != len(r.cols) {
		// Surfaces a column/Scan-arg count drift immediately.
		return errScanArity{want: len(r.cols), got: len(dest)}
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *uuid.UUID:
			*p = r.cols[i].(uuid.UUID)
		case *pgtype.UUID:
			*p = r.cols[i].(pgtype.UUID)
		case *string:
			*p = r.cols[i].(string)
		case *bool:
			*p = r.cols[i].(bool)
		case *int64:
			*p = r.cols[i].(int64)
		case *int:
			*p = r.cols[i].(int)
		case *[]byte:
			*p = r.cols[i].([]byte)
		case *pgtype.Timestamptz:
			*p = r.cols[i].(pgtype.Timestamptz)
		case *time.Time:
			*p = r.cols[i].(time.Time)
		default:
			return errScanType{idx: i}
		}
	}
	return nil
}

type errScanArity struct{ want, got int }

func (e errScanArity) Error() string { return "fakeSnapshotRow: scan arity mismatch" }

type errScanType struct{ idx int }

func (e errScanType) Error() string { return "fakeSnapshotRow: unsupported scan dest type" }

func TestIncrementalSnapshotReadRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	id := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()
	parentID := uuid.New()
	baseID := uuid.New()

	// Column values in the EXACT order projected by snapshotSelectColumns and
	// consumed by scanSnapshotWithChainFields.
	row := &fakeSnapshotRow{cols: []any{
		id,                   // id
		tenantID,             // tenant_id
		siteID,               // site_id
		pgtype.UUID{},        // created_by (null)
		"full",               // kind
		"completed",          // status
		"age1recipient",      // age_recipient
		int64(123),           // total_size
		int64(4),             // chunk_count
		"",                   // error
		false,                // archived
		[]byte("{}"),         // progress
		pgtype.Timestamptz{}, // progress_updated_at (null)
		pgtype.Timestamptz{Time: now, Valid: true}, // started_at
		pgtype.Timestamptz{Time: now, Valid: true}, // finished_at
		now,  // created_at
		now,  // updated_at
		true, // is_incremental
		pgtype.UUID{Bytes: parentID, Valid: true}, // parent_snapshot_id
		pgtype.UUID{Bytes: baseID, Valid: true},   // base_snapshot_id
		pgtype.UUID{Bytes: chainID, Valid: true},  // chain_id
		2,                                         // generation
		int64(10),                                 // cycle_files_scanned
		int64(3),                                  // cycle_files_changed
		int64(1),                                  // cycle_files_deleted
		int64(2048),                               // cycle_bytes_uploaded
		false,                                     // locked (m49 Track C)
		pgtype.UUID{},                             // destination_id (null; M7/ADR-036 P1)
	}}

	snap, err := scanSnapshotWithChainFields(row)
	if err != nil {
		t.Fatalf("scanSnapshotWithChainFields: %v", err)
	}

	// Read path → Snapshot model.
	if !snap.IsIncremental {
		t.Errorf("Snapshot.IsIncremental = false, want true")
	}
	if snap.Generation != 2 {
		t.Errorf("Snapshot.Generation = %d, want 2", snap.Generation)
	}
	if snap.ChainID == nil || *snap.ChainID != chainID {
		t.Errorf("Snapshot.ChainID = %v, want %v", snap.ChainID, chainID)
	}
	if snap.ParentSnapshotID == nil || *snap.ParentSnapshotID != parentID {
		t.Errorf("Snapshot.ParentSnapshotID = %v, want %v", snap.ParentSnapshotID, parentID)
	}
	if snap.BaseSnapshotID == nil || *snap.BaseSnapshotID != baseID {
		t.Errorf("Snapshot.BaseSnapshotID = %v, want %v", snap.BaseSnapshotID, baseID)
	}
	if snap.CycleFilesScanned != 10 || snap.CycleBytesUploaded != 2048 {
		t.Errorf("cycle stats = %d/%d, want 10/2048", snap.CycleFilesScanned, snap.CycleBytesUploaded)
	}

	// API path → gen.BackupSnapshot DTO.
	api := toAPISnapshot(snap)
	if !api.IsIncremental.Value {
		t.Errorf("api.IsIncremental = false, want true")
	}
	if api.Generation.Value != 2 {
		t.Errorf("api.Generation = %d, want 2", api.Generation.Value)
	}
	if !api.ChainID.Set || api.ChainID.Value != chainID {
		t.Errorf("api.ChainID set=%v value=%v, want set/%v", api.ChainID.Set, api.ChainID.Value, chainID)
	}
	if !api.ParentSnapshotID.Set || api.ParentSnapshotID.Value != parentID {
		t.Errorf("api.ParentSnapshotID set=%v value=%v, want set/%v", api.ParentSnapshotID.Set, api.ParentSnapshotID.Value, parentID)
	}
	if !api.BaseSnapshotID.Set || api.BaseSnapshotID.Value != baseID {
		t.Errorf("api.BaseSnapshotID set=%v value=%v, want set/%v", api.BaseSnapshotID.Set, api.BaseSnapshotID.Value, baseID)
	}
}

// TestFullSnapshotReadRoundTrip locks the full/legacy case: a generation-0,
// non-incremental row with null chain pointers must serialize with
// IsIncremental=false, Generation=0 and the chain pointers UNSET (so the badge
// renders nothing — see FIX 2).
func TestFullSnapshotReadRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	row := &fakeSnapshotRow{cols: []any{
		uuid.New(),           // id
		uuid.New(),           // tenant_id
		uuid.New(),           // site_id
		pgtype.UUID{},        // created_by
		"full",               // kind
		"completed",          // status
		"age1recipient",      // age_recipient
		int64(0),             // total_size
		int64(0),             // chunk_count
		"",                   // error
		false,                // archived
		[]byte("{}"),         // progress
		pgtype.Timestamptz{}, // progress_updated_at
		pgtype.Timestamptz{}, // started_at
		pgtype.Timestamptz{}, // finished_at
		now,                  // created_at
		now,                  // updated_at
		false,                // is_incremental
		pgtype.UUID{},        // parent_snapshot_id (null)
		pgtype.UUID{},        // base_snapshot_id (null)
		pgtype.UUID{},        // chain_id (null)
		0,                    // generation
		int64(0),             // cycle_files_scanned
		int64(0),             // cycle_files_changed
		int64(0),             // cycle_files_deleted
		int64(0),             // cycle_bytes_uploaded
		false,                // locked (m49 Track C)
		pgtype.UUID{},        // destination_id (null; M7/ADR-036 P1)
	}}

	snap, err := scanSnapshotWithChainFields(row)
	if err != nil {
		t.Fatalf("scanSnapshotWithChainFields: %v", err)
	}
	if snap.IsIncremental || snap.Generation != 0 {
		t.Errorf("full snapshot read incremental=%v generation=%d, want false/0", snap.IsIncremental, snap.Generation)
	}
	if snap.ChainID != nil || snap.ParentSnapshotID != nil || snap.BaseSnapshotID != nil {
		t.Errorf("full snapshot chain pointers should be nil, got chain=%v parent=%v base=%v", snap.ChainID, snap.ParentSnapshotID, snap.BaseSnapshotID)
	}

	api := toAPISnapshot(snap)
	if api.IsIncremental.Value {
		t.Errorf("api.IsIncremental = true, want false")
	}
	if api.ChainID.Set || api.ParentSnapshotID.Set || api.BaseSnapshotID.Set {
		t.Errorf("api chain pointers should be unset for a full snapshot")
	}
}
