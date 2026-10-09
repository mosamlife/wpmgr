package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

// s3TestImage is the object store every integration test runs against: the
// SeaweedFS S3 gateway, the server production runs. It is pinned by digest (the
// tag is for the reader, the digest is what is pulled).
//
// This one line is the only place the repository names the image. The workflow
// .github/workflows/api-integration.yml pre-pulls whatever
// scripts/test-s3-image.sh reads out of it, and scripts/test-s3-image_test.sh
// is that script's regression suite. Keep it a single-line const declaration:
// the script anchors on the declaration, not on a substring.
const s3TestImage = "chrislusf/seaweedfs:3.80@sha256:1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a"

const (
	// The credentials the tests sign with. They match infra/seaweedfs/s3.json,
	// the identity the dev stack mounts, and guard nothing: the container exists
	// for one test and is never published.
	s3TestAccessKey = "wpmgr"
	s3TestSecretKey = "wpmgr-dev-secret"
	s3TestBucket    = "wpmgr-backups"

	// s3TestPort is the S3 gateway's port inside the container; the host side is
	// mapped by Docker.
	s3TestPort = "8333/tcp"
	// s3TestConfigPath is where the identity file is placed in the container.
	s3TestConfigPath = "/etc/seaweedfs/s3.json"

	// s3TestStartTimeout bounds the wait for the gateway to answer; a cold
	// start brings up a master, a volume server, a filer and the gateway.
	s3TestStartTimeout = 2 * time.Minute
	// s3TestWritableTimeout bounds the wait for the first object to be accepted.
	s3TestWritableTimeout = 60 * time.Second
	// s3TestProbeKey is written and removed to prove the store takes writes. It
	// sits at the bucket root so deleting it leaves no empty folder behind.
	s3TestProbeKey = "readiness-probe"
)

// s3TestConfig renders the gateway's identity file: one identity holding the
// test credentials, in the same shape as infra/seaweedfs/s3.json.
func s3TestConfig() ([]byte, error) {
	type credential struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	type identity struct {
		Name        string       `json:"name"`
		Credentials []credential `json:"credentials"`
		Actions     []string     `json:"actions"`
	}
	return json.Marshal(struct {
		Identities []identity `json:"identities"`
	}{
		Identities: []identity{{
			Name:        "wpmgr",
			Credentials: []credential{{AccessKey: s3TestAccessKey, SecretKey: s3TestSecretKey}},
			Actions:     []string{"Admin", "Read", "Write", "List", "Tagging"},
		}},
	})
}

// s3TestLogTail returns the end of a container's log, for the failure message
// of a store that never became usable. A start that fails inside a container
// says nothing useful from outside it.
func s3TestLogTail(ctx context.Context, c *testcontainers.DockerContainer) string {
	if c == nil {
		return "(no container)"
	}
	rc, err := c.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("(container log unavailable: %v)", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
	const keep = 4096
	if len(b) > keep {
		b = b[len(b)-keep:]
	}
	return string(b)
}

// awaitS3Writable returns once the store has accepted, and then removed, a real
// object, and fails the test if that does not happen within
// s3TestWritableTimeout.
//
// The wait strategy only proves the gateway answers HTTP. A write needs more:
// the bucket has to exist, and the volume layer behind the gateway has to have
// somewhere to put the bytes. EnsureBucket cannot be the check, because it is
// best-effort by design and returns nil whatever happened, so a failed create
// would only surface later as NoSuchBucket in some unrelated test. This goes
// through the same Store methods the tests use.
func awaitS3Writable(t *testing.T, ctx context.Context, store *blobstore.Store, c *testcontainers.DockerContainer) {
	t.Helper()
	deadline := time.Now().Add(s3TestWritableTimeout)
	var lastErr error
	for {
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = func() error {
			if err := store.EnsureBucket(attempt); err != nil {
				return fmt.Errorf("ensure bucket: %w", err)
			}
			if err := store.Put(attempt, s3TestProbeKey, strings.NewReader("ok"), 2); err != nil {
				return fmt.Errorf("probe put: %w", err)
			}
			if err := store.Delete(attempt, s3TestProbeKey); err != nil {
				return fmt.Errorf("probe delete: %w", err)
			}
			return nil
		}()
		cancel()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("seaweedfs: gateway answered but never accepted a write within %s: %v\n--- container log tail ---\n%s",
		s3TestWritableTimeout, lastErr, s3TestLogTail(ctx, c))
}

// startBlobstore spins up an ephemeral SeaweedFS (S3 gateway) container and
// returns a Store bound to a fresh bucket. This exercises the real
// aws-sdk-go-v2 path-style + custom-endpoint code against the same object-store
// server production runs.
func startBlobstore(t *testing.T) *blobstore.Store {
	t.Helper()
	ctx := context.Background()

	skipIfDockerUnavailable(t, ctx, "seaweedfs")

	cfg, err := s3TestConfig()
	if err != nil {
		t.Fatalf("seaweedfs: render S3 config: %v", err)
	}

	// The image's entrypoint turns "server" into `weed server` with its own data
	// directory and volume defaults; the flags after it come last and win.
	// Volumes are not preallocated and are capped small, so one fixture reserves
	// almost no disk (the defaults reserve a gibibyte per volume, several volumes
	// at the first write).
	container, err := testcontainers.Run(ctx, s3TestImage,
		testcontainers.WithExposedPorts(s3TestPort),
		testcontainers.WithCmd("server", "-s3", "-s3.config="+s3TestConfigPath,
			"-master.volumePreallocate=false", "-master.volumeSizeLimitMB=64"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            bytes.NewReader(cfg),
			ContainerFilePath: s3TestConfigPath,
			FileMode:          0o644,
		}),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/status").WithPort(s3TestPort).WithStartupTimeout(s3TestStartTimeout)),
	)
	// container can be non-nil even when err != nil (partial start); register
	// cleanup before the error check so a failure path cannot leak it (see
	// rls_integration_test.go's startPostgres).
	if container != nil {
		t.Cleanup(func() { _ = container.Terminate(ctx) })
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx,
			fmt.Errorf("%w\n--- container log tail ---\n%s", err, s3TestLogTail(ctx, container)),
			"seaweedfs: container start")
	}

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("seaweedfs container host: %v", err)
	}
	port, err := container.MappedPort(ctx, s3TestPort)
	if err != nil {
		t.Fatalf("seaweedfs mapped port: %v", err)
	}

	store, err := blobstore.New(blobstore.Config{
		Endpoint:       fmt.Sprintf("http://%s:%s", host, port.Port()),
		Region:         "us-east-1",
		Bucket:         s3TestBucket,
		AccessKey:      s3TestAccessKey,
		SecretKey:      s3TestSecretKey,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("blobstore new: %v", err)
	}
	awaitS3Writable(t, ctx, store, container)
	return store
}

// TestBlobstoreRoundTrip exercises Put/Get/Head/Delete/List and the presigned
// PUT/GET URLs against a real S3-compatible endpoint (SeaweedFS via testcontainers).
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
