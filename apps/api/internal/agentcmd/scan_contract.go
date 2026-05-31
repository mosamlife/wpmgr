package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the S3
// Malware / File-Integrity Scan feature. The wp-agent-engineer mirrors these
// shapes in the agent's class-scan-command.php and class-get-file-command.php.
// Field names are JSON wire names; do not rename without updating both sides.
//
// Transport — scan:
//   POST {site_url}/wp-json/wpmgr/v1/command/scan
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="scan", aud=<siteId>
//   Body:   application/json — ScanRequest below.
//   Response: 200 with ScanResponse; ok=false = semantic failure.
//
// Transport — get_file:
//   POST {site_url}/wp-json/wpmgr/v1/command/get_file
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="get_file", aud=<siteId>
//   Body:   application/json — GetFileRequest below.
//   Response: 200 with GetFileResponse.

// ScanCursor is the resume cursor returned by the agent when kind=partial.
// It encodes where the DFS traversal left off so the next call can continue
// from exactly the same position without re-scanning already-visited files.
//
//	dir                 — absolute path of the directory being traversed.
//	traversal_stack     — encoded DFS stack: each element is [name, offset].
//	folder_offset       — offset within the current directory listing.
type ScanCursor struct {
	Dir            string          `json:"dir"`
	TraversalStack [][]interface{} `json:"traversal_stack"` // [[name, offset], ...]
	FolderOffset   int             `json:"folder_offset"`
}

// ScanRequest is the POST body for the `scan` command.
//
//	run_id                  — CP-generated UUID for this scan run (idempotency key).
//	kind                    — "core"|"files"|"full" (phase-1 ships core only).
//	include_md5             — true always; wire field kept for forward compat.
//	time_budget_s           — max seconds the agent may spend scanning per call.
//	paths_limit             — max number of file hash rows per response.
//	batch_size              — max files processed per directory batch.
//	traversal_stack_max_size — max DFS stack depth.
//	resume_cursor           — null on first call; the previous ScanResponse.next_cursor on subsequent.
type ScanRequest struct {
	RunID                 string      `json:"run_id"`
	Kind                  string      `json:"kind"`
	IncludeMD5            bool        `json:"include_md5"`
	TimeBudgetS           int         `json:"time_budget_s"`
	PathsLimit            int         `json:"paths_limit"`
	BatchSize             int         `json:"batch_size"`
	TraversalStackMaxSize int         `json:"traversal_stack_max_size"`
	ResumeCursor          *ScanCursor `json:"resume_cursor"`
}

// ScanHashEntry is one file hash row returned inline in ScanResponse.
// A file that could not be read is represented with Error="NOT_READABLE"
// and all other fields zero/empty — we still record its path as unreadable.
//
// All numeric fields use flexInt64 / flexInt decoding in the CP because
// PHP json_encode on wpdb ARRAY_A may produce quoted numeric strings.
type ScanHashEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`   // 32 hex chars or "" for unreadable
	Mtime  int64  `json:"mtime"` // Unix seconds
	Mode   int    `json:"mode"`
	IsLink bool   `json:"is_link"`
	Error  string `json:"error,omitempty"` // "NOT_READABLE" when set
}

// ScanResponse is the agent's response to the `scan` command.
//
//	ok            — false when a hard failure occurred (not "time budget used").
//	run_id        — echoed back for idempotency verification.
//	kind          — echoed back.
//	status        — "partial" (more files remain) or "done" (traversal complete).
//	files_scanned — cumulative count of files processed in this call.
//	next_cursor   — nil when status="done"; resume point when status="partial".
//	links         — paths of symlinks encountered (recorded but never followed).
//	hashes        — the inline file hash batch for this call.
type ScanResponse struct {
	OK           bool            `json:"ok"`
	RunID        string          `json:"run_id"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"` // "partial"|"done"
	FilesScanned int64           `json:"files_scanned"`
	NextCursor   *ScanCursor     `json:"next_cursor"`
	Links        []string        `json:"links"`
	Hashes       []ScanHashEntry `json:"hashes"`
}

// GetFileRequest is the POST body for the `get_file` command.
//
//	path      — absolute server-side path to fetch (must be an existing finding).
//	max_bytes — cap on bytes returned as base64; 262144 (256 KiB) by default.
type GetFileRequest struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
}

// GetFileResponse is the agent's response to the `get_file` command.
//
//	ok              — false when the agent refused the request (dir/symlink/over-cap/not-found).
//	path            — echoed back.
//	size            — file size in bytes (0 if ok=false).
//	is_dir          — true if the path is a directory (agent refuses, ok=false).
//	is_link         — true if the path is a symlink (agent refuses, ok=false).
//	content_base64  — base64-encoded file content (nil/absent when ok=false).
//	error           — short reason string when ok=false.
type GetFileResponse struct {
	OK            bool   `json:"ok"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	IsDir         bool   `json:"is_dir"`
	IsLink        bool   `json:"is_link"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Error         string `json:"error,omitempty"`
}
