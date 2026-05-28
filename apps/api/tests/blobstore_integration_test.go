package tests

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

// startBlobstore spins up an ephemeral MinIO (S3-compatible) container and
// returns a Store bound to a fresh bucket. This exercises the real
// aws-sdk-go-v2 path-style + custom-endpoint code against an actual object
// store (the same code path used against SeaweedFS in production).
func startBlobstore(t *testing.T) *blobstore.Store {
	t.Helper()
	ctx := context.Background()

	container, err := minio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z",
		minio.WithUsername("wpmgr"),
		minio.WithPassword("wpmgr-dev-secret"),
	)
	if err != nil {
		t.Skipf("skipping: cannot start minio container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}

	store, err := blobstore.New(blobstore.Config{
		Endpoint:       "http://" + endpoint,
		Region:         "us-east-1",
		Bucket:         "wpmgr-backups",
		AccessKey:      container.Username,
		SecretKey:      container.Password,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("blobstore new: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return store
}

// TestBlobstoreRoundTrip exercises Put/Get/Head/Delete/List and the presigned
// PUT/GET URLs against a real S3-compatible endpoint (MinIO via testcontainers).
func TestBlobstoreRoundTrip(t *testing.T) {
	store := startBlobstore(t)
	ctx := context.Background()
	key := "chunks/tenant-x/abc123"
	payload := []byte("ciphertext-chunk-bytes")

	// Put.
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Head: exists + size.
	exists, size, err := store.Head(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if !exists || size != int64(len(payload)) {
		t.Fatalf("head = (exists=%v size=%d), want (true %d)", exists, size, len(payload))
	}

	// Head on a missing key: not exists, no error.
	exists, _, err = store.Head(ctx, "chunks/tenant-x/does-not-exist")
	if err != nil {
		t.Fatalf("head missing: %v", err)
	}
	if exists {
		t.Fatal("head on missing key reported exists=true")
	}

	// Get.
	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("get = %q, want %q", got, payload)
	}

	// Get missing → ErrNotFound.
	if _, err := store.Get(ctx, "chunks/tenant-x/nope"); err != blobstore.ErrNotFound {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}

	// List by prefix.
	keys, err := store.List(ctx, "chunks/tenant-x/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("list = %v, want [%s]", keys, key)
	}

	// Delete then confirm gone.
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	exists, _, _ = store.Head(ctx, key)
	if exists {
		t.Fatal("object still exists after delete")
	}
}

// TestBlobstorePresignRoundTrip mints a presigned PUT URL, uploads via plain
// HTTP (no AWS creds — proving the URL itself authorizes), then mints a
// presigned GET and downloads, proving the presign flow the agent uses works
// end-to-end against a real S3-compatible endpoint.
func TestBlobstorePresignRoundTrip(t *testing.T) {
	store := startBlobstore(t)
	ctx := context.Background()
	key := "chunks/tenant-y/deadbeef"
	payload := []byte("agent-uploaded-ciphertext")

	putURL, err := store.PresignPut(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("presigned PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("presigned PUT status = %d", resp.StatusCode)
	}

	getURL, err := store.PresignGet(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	gresp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("presigned GET: %v", err)
	}
	defer func() { _ = gresp.Body.Close() }()
	got, _ := io.ReadAll(gresp.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("presigned GET body = %q, want %q", got, payload)
	}
}
