package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the M4
// incremental backup & restore feature. The wp-agent-engineer mirrors these
// shapes in apps/agent/includes/commands/class-backup-command.php and
// class-restore-command.php. Field names are JSON wire names; do not rename
// without updating both sides.
//
// Transport: POST {site_url}/wp-json/wpmgr/v1/command/{command}
//   command ∈ {"backup", "restore"}  (in addition to M3 "update"/"rollback")
//   Header:  Authorization: Bearer <minted EdDSA JWT>  (see jwt.go; aud == the
//            target site's enrollment UUID, cmd == "backup"|"restore")
//   Body:    application/json — the request structs below.
//   Response: 200 with the response structs below; non-200 ⇒ command failed.
//
// =====================  ENCRYPTION & TRUST MODEL  ============================
// Encryption is CLIENT-SIDE on the AGENT. The control plane and S3 store ONLY
// ciphertext. The CP command payload carries an `age_recipient` (an age PUBLIC
// X25519 recipient, "age1...") and NEVER a decryption key/identity. The agent:
//
//   BACKUP:
//     1. Streams each file (and `wp db export --single-transaction` for db) and
//        splits it into ~4 MiB plaintext chunks.
//     2. Encrypts each chunk to `age_recipient` with age (armor off; binary).
//     3. Computes blake3 = lowercase hex BLAKE3-256 of the CIPHERTEXT chunk.
//     4. The s3 object is content-addressed by that ciphertext hash.
//     5. Asks the CP (this `backup` command's response, presign_url field, or
//        the dedicated agent endpoint POST /agent/v1/backups/presign) which of
//        its chunk hashes are NOT already stored, then PUTs only those ciphertext
//        chunks to the returned presigned URLs.
//     6. Submits the manifest (per-path ordered ciphertext-chunk-hash lists) to
//        the CP (POST /agent/v1/backups/{snapshot}/manifest). The CP records the
//        snapshot, manifest, and chunk rows (incrementing refcounts; storing only
//        not-yet-stored chunks).
//
//   RESTORE:
//     1. The CP issues presigned GET URLs for every ciphertext chunk in the
//        (possibly partial) manifest, plus the ordered manifest itself.
//     2. The agent downloads each ciphertext chunk, VERIFIES blake3 over the
//        downloaded ciphertext, decrypts with its age IDENTITY (held by the
//        operator/agent — NOT the CP), reassembles files in chunk order, and
//        either writes files or imports the db dump (`wp db import`).
//
// The age IDENTITY (private key) is held by the operator/agent only. Operator
// escrow is explicitly OUT OF SCOPE for V0; the CP stores only the recipient.
// ============================================================================

// Backup snapshot kinds (CP <-> agent).
const (
	BackupKindFiles = "files"
	BackupKindDB    = "db"
	BackupKindFull  = "full"
)

// Backup manifest entry kinds.
const (
	EntryKindFile = "file"
	EntryKindDB   = "db"
)

// ChunkBytes is the target plaintext chunk size (~4 MiB) the agent splits files
// into before encrypting. The CP advertises it so the agent and CP agree; the
// agent MAY use a smaller final chunk. The CIPHERTEXT chunk is what is hashed
// and stored.
const ChunkBytes = 4 << 20

// BackupRequest is the POST body for the `backup` command.
//
//	snapshot_id   the CP-assigned snapshot UUID the agent reports the manifest
//	              against (string form). The agent echoes it in the manifest
//	              submission.
//	kind          "files" | "db" | "full".
//	age_recipient the age PUBLIC recipient ("age1...") the agent MUST encrypt
//	              every chunk to. NEVER a private key. If empty the agent MUST
//	              refuse (a backup the operator could never decrypt is useless;
//	              the CP guarantees this is set before dispatch).
//	chunk_bytes   target plaintext chunk size in bytes (CP advertises ChunkBytes).
//	presign_endpoint the agent->CP endpoint to request presigned PUT URLs for
//	              not-yet-stored ciphertext chunk hashes. Absolute URL on the CP.
//	manifest_endpoint the agent->CP endpoint to submit the completed manifest.
//	agent_auth    how the agent authenticates the above two callbacks (it reuses
//	              its M2 Ed25519 signed-request scheme; this field is advisory).
type BackupRequest struct {
	SnapshotID       string `json:"snapshot_id"`
	Kind             string `json:"kind"`
	AgeRecipient     string `json:"age_recipient"`
	ChunkBytes       int    `json:"chunk_bytes"`
	PresignEndpoint  string `json:"presign_endpoint"`
	ManifestEndpoint string `json:"manifest_endpoint"`
}

// BackupResponse is the agent's immediate ack of the `backup` command. The
// heavy lifting (chunking, encrypting, uploading, manifest submission) proceeds
// against the CP callbacks; this response signals the agent accepted the job.
//
//	ok       the agent accepted the backup job.
//	detail   short human-readable note (e.g. "queued" or an early refusal reason).
type BackupResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ChunkRef is one ordered ciphertext chunk of a manifest entry.
//
//	blake3   lowercase hex BLAKE3-256 of the CIPHERTEXT chunk.
//	size     ciphertext byte length (what S3 stores).
type ChunkRef struct {
	Blake3 string `json:"blake3"`
	Size   int64  `json:"size"`
}

// ManifestEntry is one file (or db dump) in a snapshot.
//
//	path        site-relative file path; "database.sql" for a db dump.
//	entry_kind  "file" | "db".
//	table_name  set for db entries to support partial restore-by-table (empty
//	            for file entries).
//	mode        unix file mode bits (0 for db).
//	size        total PLAINTEXT size of the path.
//	chunks      ordered ciphertext chunks reassembling the path.
type ManifestEntry struct {
	Path      string     `json:"path"`
	EntryKind string     `json:"entry_kind"`
	TableName string     `json:"table_name,omitempty"`
	Mode      uint32     `json:"mode"`
	Size      int64      `json:"size"`
	Chunks    []ChunkRef `json:"chunks"`
}

// PresignChunksRequest is the agent->CP request (to PresignEndpoint) asking which
// ciphertext chunk hashes are NOT yet stored, with presigned PUT URLs for them.
//
//	snapshot_id  the in-flight snapshot.
//	hashes       candidate ciphertext chunk hashes the agent produced.
type PresignChunksRequest struct {
	SnapshotID string   `json:"snapshot_id"`
	Hashes     []string `json:"hashes"`
}

// PresignChunksResponse returns, for each NOT-yet-stored hash, a presigned PUT
// URL. Hashes already stored for the tenant are omitted (dedup): the agent skips
// uploading them. URLs are bearer credentials with a short TTL.
//
//	uploads   blake3 -> presigned PUT URL for hashes that must be uploaded.
//	ttl_seconds the presign validity window (advisory; the URL embeds expiry).
type PresignChunksResponse struct {
	Uploads    map[string]string `json:"uploads"`
	TTLSeconds int               `json:"ttl_seconds"`
}

// SubmitManifestRequest is the agent->CP submission (to ManifestEndpoint) of the
// completed manifest after all not-yet-stored ciphertext chunks were uploaded.
//
//	snapshot_id  the snapshot the manifest belongs to.
//	age_recipient echo of the recipient the chunks were encrypted to (provenance).
//	entries      every file/db entry with its ordered ciphertext chunk list.
type SubmitManifestRequest struct {
	SnapshotID   string          `json:"snapshot_id"`
	AgeRecipient string          `json:"age_recipient"`
	Entries      []ManifestEntry `json:"entries"`
}

// SubmitManifestResponse is the CP's ack of a submitted manifest.
type SubmitManifestResponse struct {
	OK          bool  `json:"ok"`
	ChunkCount  int64 `json:"chunk_count"`
	StoredCount int64 `json:"stored_count"`
}

// RestoreChunk is one presigned-GET-able ciphertext chunk in a restore plan.
//
//	blake3   ciphertext hash the agent re-verifies after download.
//	get_url  presigned GET URL (bearer credential; never logged).
//	size     ciphertext byte length.
type RestoreChunk struct {
	Blake3 string `json:"blake3"`
	GetURL string `json:"get_url"`
	Size   int64  `json:"size"`
}

// RestoreEntry is one file/db entry to restore: its path/mode plus the ORDERED
// presigned ciphertext chunks. The agent downloads each, verifies blake3,
// decrypts with its age identity, and reassembles in order.
type RestoreEntry struct {
	Path      string         `json:"path"`
	EntryKind string         `json:"entry_kind"`
	TableName string         `json:"table_name,omitempty"`
	Mode      uint32         `json:"mode"`
	Size      int64          `json:"size"`
	Chunks    []RestoreChunk `json:"chunks"`
}

// RestoreRequest is the POST body for the `restore` command. The CP has already
// resolved the (possibly partial) selection into the concrete ordered entries +
// presigned GET URLs; the agent just executes them. NO decryption key is present
// — the agent decrypts with the age identity it alone holds.
//
//	snapshot_id  the snapshot being restored from.
//	kind         the snapshot kind (informational).
//	entries      the resolved entries with presigned ciphertext chunks.
type RestoreRequest struct {
	SnapshotID string         `json:"snapshot_id"`
	Kind       string         `json:"kind"`
	Entries    []RestoreEntry `json:"entries"`
}

// RestoreResponse is the agent's response to the `restore` command.
//
//	ok               whether the restore succeeded.
//	restored_entries number of entries reassembled/imported.
//	verified         true if every downloaded ciphertext chunk matched its blake3.
//	log              short human-readable detail.
type RestoreResponse struct {
	OK              bool   `json:"ok"`
	RestoredEntries int    `json:"restored_entries"`
	Verified        bool   `json:"verified"`
	Log             string `json:"log,omitempty"`
}
