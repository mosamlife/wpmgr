package blobstore

// blobstore_test.go — ADR-036 P1 (GH #146 security review) unit tests for
// Store.PathPrefix: an s3_compat destination's configured key prefix must be
// applied CONSISTENTLY to both the backup PUT and the restore GET presign, so
// the two sides always agree on the same effective object key. Presigning is
// computed entirely offline (SigV4 needs no network round trip), so these
// tests need no real S3 endpoint.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStore_PresignPutAndGet_ApplyPathPrefixConsistently(t *testing.T) {
	store, err := New(Config{
		Bucket:         "test-bucket",
		Endpoint:       "http://127.0.0.1:9000",
		ForcePathStyle: true,
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		Region:         "us-east-1",
		PathPrefix:     "/clientA/backups/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.PathPrefix() != "clientA/backups" {
		t.Fatalf("PathPrefix() = %q, want %q (leading/trailing slashes normalised)", store.PathPrefix(), "clientA/backups")
	}

	putURL, err := store.PresignPut(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	getURL, err := store.PresignGet(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	const wantSegment = "/test-bucket/clientA/backups/chunks/tenant-x/hash123"
	if !strings.Contains(putURL, wantSegment) {
		t.Errorf("PresignPut URL = %s, want to contain %q", putURL, wantSegment)
	}
	if !strings.Contains(getURL, wantSegment) {
		t.Errorf("PresignGet URL = %s, want to contain %q", getURL, wantSegment)
	}

	// Backup PUT and restore GET must agree on the SAME effective key (the
	// path component; the query string legitimately differs — different verb,
	// different signature/expiry).
	putPath := strings.SplitN(putURL, "?", 2)[0]
	getPath := strings.SplitN(getURL, "?", 2)[0]
	if putPath != getPath {
		t.Errorf("PUT and GET presign paths differ: put=%q get=%q", putPath, getPath)
	}
}

func TestStore_PresignPut_NoPathPrefix_BucketRootUnchanged(t *testing.T) {
	store, err := New(Config{
		Bucket:         "test-bucket",
		Endpoint:       "http://127.0.0.1:9000",
		ForcePathStyle: true,
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		Region:         "us-east-1",
		// PathPrefix intentionally empty — the CP-global default Store's
		// pre-existing, unchanged behaviour.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.PathPrefix() != "" {
		t.Fatalf("PathPrefix() = %q, want empty (managed/legacy default)", store.PathPrefix())
	}

	url, err := store.PresignPut(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	const wantSegment = "/test-bucket/chunks/tenant-x/hash123"
	if !strings.Contains(url, wantSegment) {
		t.Errorf("PresignPut URL = %s, want to contain %q (bucket-root, no prefix)", url, wantSegment)
	}
	// The prefixed segment (with any client-side prefix) must NOT appear —
	// belt-and-suspenders against an accidental leading-slash double segment.
	if strings.Contains(url, "//chunks") {
		t.Errorf("PresignPut URL has a spurious double slash: %s", url)
	}
}
