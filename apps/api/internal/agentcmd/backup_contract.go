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
	// ProgressEndpoint is M5.6 / ADR-032: the URL the agent's detached phpbu
	// runner POSTs phase progress to. The runner signs each POST with the same
	// Ed25519 scheme used by presign/manifest, so the CP authenticates it the
	// same way. Older agents (< 0.6.0) ignore the field harmlessly — they ship
	// no runner — so it is non-breaking to include unconditionally.
	ProgressEndpoint string `json:"progress_endpoint"`
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

// RestoreChunk is one presigned-GET-able PLAIN chunk of an artifact-part.
//
// ADR-033/ADR-034 v0.8.1 wire shape: chunks are PLAIN (no age envelope). The
// agent reassembles by concatenating chunks in order, then decrypts/extracts
// the resulting artifact-part with its own engine (the age identity is held by
// the operator/agent — not the CP).
//
//	hash   lowercase hex blake2b of the PLAIN chunk; optional verification.
//	url    presigned GET URL on the object store (bearer credential; never logged).
//	size   expected chunk byte length.
type RestoreChunk struct {
	Hash string `json:"hash"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// RestoreEntry is one artifact-part to restore: a logical filename plus the
// ORDERED presigned PLAIN chunks. The agent downloads each, optionally verifies
// the hash, reassembles by concatenation, and hands the result to its restore
// engine.
//
//	logical_path  artifact-part filename, e.g. "database.sql.gz" or
//	              "wp-content.part001.zip". The agent's restore engine maps
//	              this to its own on-disk layout.
//	chunks        ordered plain chunks reassembling the artifact-part.
type RestoreEntry struct {
	LogicalPath string         `json:"logical_path"`
	Chunks      []RestoreChunk `json:"chunks"`
}

// RestoreManifest wraps the ordered list of artifact-part entries the agent
// must reassemble. It is its own type so the wire JSON nests cleanly under
// `manifest.entries` (matching ADR-033 §4).
type RestoreManifest struct {
	Entries []RestoreEntry `json:"entries"`
}

// RestoreRequest is the v0.8.1 (ADR-034) restore wire contract: per-artifact
// manifest entries with presigned GET URLs for each chunk. Chunks are PLAIN
// (no age envelope) — the agent reassembles by concatenating in order.
//
//	snapshot_id        the snapshot being restored from.
//	restore_id         CP-generated UUID, unique per restore attempt; the
//	                   de-dup key the agent uses to ignore duplicate dispatches.
//	kind               "files" | "db" | "full".
//	progress_endpoint  REUSED — the agent POSTs restore phase events to the
//	                   same /agent/v1/backups/{snapshot}/progress endpoint as
//	                   backups.
//	manifest           ordered artifact-part entries with presigned chunks.
//	chunk_bytes        target chunk size hint (the agent reads chunks one at
//	                   a time; this is advisory only).
//	keep_old_files     M6 / Track 2: when true the agent keeps the pre-restore
//	                   wp-content tree at .wpmgr-old-files-<id>/ for 24 hours
//	                   as a manual rollback affordance. Older agents (< M6)
//	                   ignore the field harmlessly — false by default preserves
//	                   the pre-existing immediate-cleanup semantics.
type RestoreRequest struct {
	SnapshotID       string          `json:"snapshot_id"`
	RestoreID        string          `json:"restore_id"`
	Kind             string          `json:"kind"`
	ProgressEndpoint string          `json:"progress_endpoint"`
	Manifest         RestoreManifest `json:"manifest"`
	ChunkBytes       int             `json:"chunk_bytes,omitempty"`
	KeepOldFiles     bool            `json:"keep_old_files,omitempty"`

	// P0 URL rewriter (ADR-036): target_* URLs tell the agent's URL_REWRITE
	// phase what to rewrite siteurl / home / content / upload references to.
	// When unset the agent falls back to the live site's URL — so a same-
	// environment restore short-circuits the rewrite to a no-op. Required
	// for cross-environment restores (dev->prod, staging->prod, agency
	// handoffs). V1 simplification: when target_content_url / _upload_url
	// are empty the agent derives them from target_site_url
	// (`<site>/wp-content` and `<site>/wp-content/uploads` respectively).
	TargetSiteURL    string `json:"target_site_url,omitempty"`
	TargetHomeURL    string `json:"target_home_url,omitempty"`
	TargetContentURL string `json:"target_content_url,omitempty"`
	TargetUploadURL  string `json:"target_upload_url,omitempty"`

	// source_* URLs are the URLs the snapshot was taken under. Recorded at
	// backup time on backup_snapshots (P0 migration: m7_url_rewriter).
	// Used by the agent's URL_REWRITE phase as the FROM side of the rewrite
	// pairs. When omitted the agent extracts them from the dump's banner
	// comments — defense against a manifest that predates the source-URL
	// capture.
	SourceSiteURL    string `json:"source_site_url,omitempty"`
	SourceHomeURL    string `json:"source_home_url,omitempty"`
	SourceContentURL string `json:"source_content_url,omitempty"`
	SourceUploadURL  string `json:"source_upload_url,omitempty"`
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
