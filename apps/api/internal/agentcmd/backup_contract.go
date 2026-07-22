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
	// EntryKindCore is the A2 WordPress-core archive component (ABSPATH:
	// wp-admin, wp-includes, root PHP files including wp-config.php). Emitted
	// only when include_core=true on the backup request.
	EntryKindCore = "core"
	// ADR-051 archive-delta increments emit two extra kinds of per-snapshot
	// manifest entry alongside the zip parts:
	//
	//   files-list:  ONE entry per snapshot (base full AND each increment),
	//                path="files.list", chunk_hashes = the chunks of a stream of
	//                `<relpath>\t<size>\t<mtime>\n` lines (one per PACKED file). It
	//                seeds the NEXT increment's diff and is fetched + presigned for
	//                the agent like any chunk. It is chunked + uploaded exactly
	//                like a zip part, so it dedups + GCs as a normal artifact.
	//
	//   tombstones:  ONE entry PER PATH whose deleted/re-added STATE CHANGED since
	//                the parent, path=<relpath>, chunk_hashes EMPTY. The `mode`
	//                field carries the state: mode=0 (TombstoneModeDelete) means the
	//                path was DELETED in this generation; mode=1 (TombstoneModeReadd)
	//                means a previously-deleted path was RE-ADDED (repacked) in this
	//                generation and its earlier tombstone must be CLEARED. This
	//                per-path delta — read by the restore planner via ListManifest in
	//                O(1) with NO CP-side chunk fetch — lets the overlay resolve the
	//                final deleted set with newest-wins un-delete: latest mention per
	//                path wins, a Readd cancels an earlier Delete.
	EntryKindFilesList  = "files-list"
	EntryKindTombstones = "tombstones"
)

// Tombstone state carried in a `tombstones` ManifestEntry's `mode` field
// (ADR-051). A tombstones entry is a per-path DELTA: the agent emits one only
// when a path's deleted state flips relative to the parent generation.
const (
	TombstoneModeDelete = 0 // path was deleted in this generation
	TombstoneModeReadd  = 1 // a previously-deleted path was repacked (un-delete)
)

// ChunkBytes is the target plaintext chunk size (~4 MiB) the agent splits files
// into before encrypting. The CP advertises it so the agent and CP agree; the
// agent MAY use a smaller final chunk. The CIPHERTEXT chunk is what is hashed
// and stored.
const ChunkBytes = 4 << 20

// ============================================================================
// ADR-036 P1 storage adapter (GH #146) — destination-routing wire contract.
// The CP decides WHERE a snapshot's chunks live and tells the agent via
// DestinationKind + DestinationConfig on BackupRequest/IncrementalBackupRequest
// (backup) and RestoreRequest (restore). Both sides (Go CP + PHP agent) use
// these field names and values verbatim. DO NOT rename without bumping both.
// ============================================================================

// Destination kind wire values.
const (
	// DestinationKindCP is the WPMgr-managed CP-global bucket — the historical
	// default. Absent/empty DestinationKind on an older CP build (or a
	// snapshot with no destination configured) means "cp".
	DestinationKindCP = "cp"
	// DestinationKindLocal means the agent writes ciphertext chunks straight
	// to disk (wp-content/wpmgr-backups by default, or DestinationConfig.
	// local_path_prefix when set) instead of uploading anywhere. The CP mints
	// NO presigned URLs for a local-kind backup/restore.
	DestinationKindLocal = "local"
	// DestinationKindS3Compat is a customer-owned S3-compatible bucket (AWS
	// S3, Wasabi, B2, DO Spaces, MinIO, …). The CP still presigns PUT/GET URLs
	// via the EXACT SAME PresignEndpoint/ManifestEndpoint/chunk-URL transport
	// as "cp" — just against the customer's bucket instead of the CP's — so
	// the agent needs no extra config for it and NEVER sees the customer's S3
	// credentials (the CP holds them, age-encrypted at rest).
	DestinationKindS3Compat = "s3_compat"
)

// DestinationConfig carries the per-destination-kind data the agent needs
// beyond DestinationKind itself. Only the fields relevant to the kind are
// populated; every other field stays zero/empty.
//
//   - DestinationKind == "cp" or "s3_compat": DestinationConfig is entirely
//     empty. The CP presigns PUT/GET (against the CP bucket or the customer's
//     bucket respectively) via the same transport as always; the agent needs
//     no additional configuration and, for s3_compat, never sees the
//     customer's credentials.
//   - DestinationKind == "local": LocalPathPrefix carries the on-disk path
//     (relative to the site's wp-content directory) the agent writes
//     ciphertext chunks under / reads them back from, instead of uploading
//     them anywhere. Empty means the agent's own default
//     ("wp-content/wpmgr-backups").
type DestinationConfig struct {
	LocalPathPrefix string `json:"local_path_prefix,omitempty"`
}

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

	// Track A (#187) — selective components + exclusions.
	// All fields are omitempty: absent means "use the agent default" so older
	// agents ignore them harmlessly.

	// Components is the set of components to archive. nil/empty = all (default).
	// Values: "plugin", "theme", "upload", "wp-content", "db", "core".
	// These are singular entry_kind values — the canonical vocabulary for the
	// CP<->agent contract and the manifest entry_kind column.
	Components []string `json:"components,omitempty"`
	// IncludeDB is the explicit DB-inclusion signal derived from the components
	// allowlist. When Components is non-empty: true if "db" is in the list,
	// false otherwise. When Components is empty (no filter), this field is
	// omitted (the agent defaults to including the DB per the snapshot kind).
	// This lets the agent skip runDumpDatabase when "db" is not selected,
	// without having to scan the Components list itself.
	IncludeDB *bool `json:"include_db,omitempty"`
	// IncludeCore, when true, adds the WordPress core source root (ABSPATH:
	// wp-admin, wp-includes, root PHP files incl. wp-config.php) as an
	// additional archive source. Emits entry_kind="core" in the manifest.
	IncludeCore bool `json:"include_core,omitempty"`
	// ExcludePaths is the list of path-segment names passed as $excludes to
	// FilesArchiver. Merged with the archiver's DEFAULT_EXCLUDES on the agent.
	ExcludePaths []string `json:"exclude_paths,omitempty"`
	// ExcludeExtensions is the list of lowercase extensions (without dot) the
	// agent skips during the files walk (e.g. ["log","bak"]).
	ExcludeExtensions []string `json:"exclude_extensions,omitempty"`
	// ExcludeFileSizeMB, when > 0, instructs the agent to skip files strictly
	// larger than this value (MiB). 0 = no filter (agent default).
	ExcludeFileSizeMB int32 `json:"exclude_file_size_mb,omitempty"`

	// DestinationKind + DestinationConfig (ADR-036 P1 storage adapter, GH
	// #146): which backend the agent should land ciphertext chunks against.
	// Absent/empty DestinationKind means "cp" — an older CP build, or a
	// snapshot with no destination configured, behaves exactly as before this
	// field existed.
	DestinationKind   string            `json:"destination_kind,omitempty"`
	DestinationConfig DestinationConfig `json:"destination_config,omitempty"`
}

// BackupResponse is the agent's immediate ack of the `backup` command. The
// heavy lifting (chunking, encrypting, uploading, manifest submission) proceeds
// against the CP callbacks; this response signals the agent accepted the job.
//
//	ok       the agent accepted the backup job.
//	detail   short human-readable note (e.g. "queued" or an early refusal reason).
//	code     GH #274: a STABLE machine-readable refusal code, set only when
//	         ok=false. Empty on older agents and on refusals without a known
//	         code — callers MUST treat an empty/unrecognized code as a normal
//	         terminal refusal and only special-case the codes they know about
//	         (e.g. "runner_in_flight"). Never parse `detail` text to recover
//	         this — it is a free-form human string and may change wording.
type BackupResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
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

	// ADR-051 archive-delta increment telemetry. All optional; default 0 for a
	// full backup. An increment reuses THIS request shape (no separate
	// IncrementalSubmitManifestRequest) and carries its per-cycle counters as
	// top-level fields the CP stamps onto the snapshot row.
	CycleFilesScanned  int64 `json:"cycle_files_scanned,omitempty"`
	CycleFilesChanged  int64 `json:"cycle_files_changed,omitempty"`
	CycleFilesDeleted  int64 `json:"cycle_files_deleted,omitempty"`
	CycleBytesUploaded int64 `json:"cycle_bytes_uploaded,omitempty"`
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

	// ADR-049: chain-restore additions. All omitempty; absent = non-chain restore.
	// Older agents receiving these fields ignore them harmlessly (unknown JSON
	// fields are discarded by PHP json_decode / Go json.Unmarshal when the target
	// type has no matching field).

	// IsChainRestore is true when the CP resolved a multi-generation chain.
	// The agent uses this to activate the tombstone-delete pass in stage_files.
	IsChainRestore bool `json:"is_chain_restore,omitempty"`

	// TargetGeneration is the chain generation being restored to (0 = base,
	// 1 = first incremental, etc.). Informational for the agent's progress log.
	TargetGeneration int `json:"target_generation,omitempty"`

	// EstimatedBytes is the CP-computed sum of FileSize for the winning file set
	// (plaintext bytes, advisory). The agent uses this in the preflight disk check
	// instead of totaling artifact sizes, which only covers the current snapshot.
	// 0 means unknown (pre-ADR-049 or non-chain path).
	EstimatedBytes int64 `json:"estimated_bytes,omitempty"`

	// TombstonePaths is the list of file paths that existed in the chain but
	// were deleted (tombstoned) by generation TargetGeneration. The agent MUST
	// delete these paths from the STAGING directory during the stage_files phase,
	// AFTER extracting the winning file set but BEFORE the atomic swap.
	// Every path has been sanitized by the CP (no ".." segments, no leading "/");
	// the agent MUST independently sanitize and realpath-verify each path before
	// any syscall (see tombstone-path-safety rules). Empty when IsChainRestore is
	// false OR when no files were deleted across the chain.
	TombstonePaths []string `json:"tombstone_paths,omitempty"`

	// DestinationKind + DestinationConfig (ADR-036 P1 storage adapter, GH
	// #146): which backend the agent should READ chunks from. "local" carries
	// NO chunk URLs in Manifest — RestoreChunk.Hash/Size are still populated
	// (so the agent can locate + verify the chunk file) but URL is empty; the
	// agent reads the chunk from its own local disk (DestinationConfig.
	// local_path_prefix) instead. Absent/empty DestinationKind means "cp" —
	// an older CP build, or a snapshot with no destination configured,
	// behaves exactly as before this field existed.
	DestinationKind   string            `json:"destination_kind,omitempty"`
	DestinationConfig DestinationConfig `json:"destination_config,omitempty"`
}

// RestoreResponse is the agent's response to the `restore` command.
//
//	ok               whether the restore succeeded.
//	restored_entries number of entries reassembled/imported.
//	verified         true if every downloaded ciphertext chunk matched its blake3.
//	log              short human-readable detail.
//	code             GH #274: a STABLE machine-readable refusal code, set only
//	                 when ok=false. Same contract as BackupResponse.Code — see
//	                 its doc comment.
type RestoreResponse struct {
	OK              bool   `json:"ok"`
	RestoredEntries int    `json:"restored_entries"`
	Verified        bool   `json:"verified"`
	Log             string `json:"log,omitempty"`
	Code            string `json:"code,omitempty"`
}

// ============================================================================
// ADR-048 — Incremental Backup V1 wire contract additions.
// Both sides (Go CP + PHP agent) use these field names verbatim.
// DO NOT rename without bumping both sides simultaneously.
// ============================================================================

// IncrementalBackupRequest extends BackupRequest for incremental runs.
// The CP sends this INSTEAD OF BackupRequest when is_incremental=true.
// When is_incremental=false the CP sends the existing BackupRequest unchanged.
type IncrementalBackupRequest struct {
	// Fields shared with BackupRequest (identical names, identical semantics).
	SnapshotID       string `json:"snapshot_id"`
	Kind             string `json:"kind"`
	AgeRecipient     string `json:"age_recipient"`
	ChunkBytes       int    `json:"chunk_bytes"`
	PresignEndpoint  string `json:"presign_endpoint"`
	ManifestEndpoint string `json:"manifest_endpoint"`
	ProgressEndpoint string `json:"progress_endpoint"`
	// Incremental-specific fields.
	IsIncremental    bool   `json:"is_incremental"`
	ParentSnapshotID string `json:"parent_snapshot_id"`
	BaseSnapshotID   string `json:"base_snapshot_id"`
	Generation       int    `json:"generation"`

	// ADR-051 archive-delta change detection: presigned GET URLs to the PARENT
	// snapshot's `files.list` chunks (the per-snapshot relpath\tsize\tmtime
	// snapshot the parent emitted). The agent concatenates these chunks in order
	// to rebuild the prev[rel]=>{size,mtime} map, then gates its archiving_files
	// walk inline (CHANGED iff !isset || size!=prev.size || mtime>prev.mtime).
	// Empty for a gen-0 base-increment (no parent → scan everything as new).
	// REPLACES the retired FileIndexEndpoint / NDJSON /file-index transport.
	PrevFilesListChunks []RestoreChunk `json:"prev_files_list_chunks,omitempty"`

	// Track A (#187) — selective components + exclusions (same semantics as
	// BackupRequest; omitempty so older agents ignore absent fields harmlessly).
	// Components uses singular entry_kind values ("plugin", "theme", "upload",
	// "wp-content", "db", "core") — the canonical CP<->agent vocabulary.
	Components []string `json:"components,omitempty"`
	// IncludeDB is the explicit DB-inclusion signal derived from Components.
	// When Components is non-empty: true if "db" is in the list, false otherwise.
	// Absent when Components is empty (no filter); the agent then follows the
	// snapshot kind for DB inclusion.
	IncludeDB         *bool    `json:"include_db,omitempty"`
	IncludeCore       bool     `json:"include_core,omitempty"`
	ExcludePaths      []string `json:"exclude_paths,omitempty"`
	ExcludeExtensions []string `json:"exclude_extensions,omitempty"`
	ExcludeFileSizeMB int32    `json:"exclude_file_size_mb,omitempty"`

	// DestinationKind + DestinationConfig (ADR-036 P1 storage adapter, GH
	// #146) — same semantics as BackupRequest.
	DestinationKind   string            `json:"destination_kind,omitempty"`
	DestinationConfig DestinationConfig `json:"destination_config,omitempty"`
}
