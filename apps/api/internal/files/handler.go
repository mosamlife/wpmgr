package files

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing File Manager routes under
// /api/v1/sites/{siteId}/files/*.
//
// Route shape mirrors internal/perf/handler.go:66.
// Every route runs behind:
//   - authz.RequireSiteAccess("siteId") on the group
//   - authz.RequirePermission(authz.PermSiteFilesRead) per route
//   - per-site opt-in flag check inside the service
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds the file manager handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the routes on the authenticated /api/v1 group. The per-site
// group carries RequireSiteAccess so every sub-route inherits the collaborator
// allowlist gate (belt-and-braces in front of the m82 RLS).
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))

	// P1: Settings
	g.GET("/files/settings", h.getSettings)
	g.PUT("/files/settings", authz.RequirePermission(authz.PermSiteFilesManage), h.putSettings)

	// P1: Read-only
	g.GET("/files", authz.RequirePermission(authz.PermSiteFilesRead), h.listDir)
	g.GET("/files/content", authz.RequirePermission(authz.PermSiteFilesRead), h.readContent)
	g.POST("/files/download", authz.RequirePermission(authz.PermSiteFilesRead), h.download)

	// P2: Write (all require PermSiteFilesWrite + files_write_enabled flag)
	g.PUT("/files/content", authz.RequirePermission(authz.PermSiteFilesWrite), h.writeContent)
	g.POST("/files/mkdir", authz.RequirePermission(authz.PermSiteFilesWrite), h.mkdir)
	g.POST("/files/rename", authz.RequirePermission(authz.PermSiteFilesWrite), h.rename)
	g.POST("/files/chmod", authz.RequirePermission(authz.PermSiteFilesWrite), h.chmod)

	// P2: Delete — requires BOTH PermSiteFilesWrite AND PermSiteFilesDelete (owner)
	g.POST("/files/delete", authz.RequirePermission(authz.PermSiteFilesWrite), h.deleteFile)

	// P2: Upload — two-step: prepare (mints presigned PUTs) + apply (agent fetches + swaps)
	g.POST("/files/upload", authz.RequirePermission(authz.PermSiteFilesWrite), h.prepareUpload)
	g.POST("/files/upload/apply", authz.RequirePermission(authz.PermSiteFilesWrite), h.applyUpload)

	// P3: Advanced ops
	// archive: read-gated (creates a ZIP of listed paths → presigned GET to download)
	g.POST("/files/archive", authz.RequirePermission(authz.PermSiteFilesRead), h.createArchive)
	// extract: write-gated + files_write_enabled (ZIP → dest_path; confirm flags require WriteCode/owner)
	g.POST("/files/extract", authz.RequirePermission(authz.PermSiteFilesWrite), h.extract)
	// search: read-gated (?path=&q=&mode=name|content&cursor=)
	g.GET("/files/search", authz.RequirePermission(authz.PermSiteFilesRead), h.search)
	// version history: read-gated (?path=)
	g.GET("/files/versions", authz.RequirePermission(authz.PermSiteFilesRead), h.listVersions)
	// version restore: write-gated + files_write_enabled
	g.POST("/files/versions/restore", authz.RequirePermission(authz.PermSiteFilesWrite), h.restoreVersion)
}

// ---------------------------------------------------------------------------
// GET /sites/:siteId/files/settings
// ---------------------------------------------------------------------------

// getSettings returns the current file manager settings for the site.
// Gated by RequireSiteAccess only — any site member can see whether the feature
// is enabled so the web UI can render the correct state.
func (h *Handler) getSettings(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	settings, err := h.svc.GetSettings(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":       settings.Enabled,
		"write_enabled": settings.WriteEnabled,
		"root_jail":     settings.RootJail,
	})
}

// ---------------------------------------------------------------------------
// PUT /sites/:siteId/files/settings
// ---------------------------------------------------------------------------

// updateSettingsBody is the request body for PUT /files/settings.
type updateSettingsBody struct {
	// Enabled enables or disables the file manager (read) for the site.
	Enabled bool `json:"enabled"`
	// WriteEnabled enables or disables the write mode (P2). Separate opt-in
	// so read and write can be toggled independently. Default false.
	WriteEnabled bool `json:"write_enabled"`
}

// putSettings enables or disables the file manager and/or the write mode.
// Requires PermSiteFilesManage (admin+). Records an audit entry on every call.
func (h *Handler) putSettings(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body updateSettingsBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	settings, err := h.svc.UpdateSettings(c.Request.Context(), p.TenantID, siteID, body.Enabled, body.WriteEnabled)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesSettingsChanged, siteID, map[string]any{
		"enabled":       settings.Enabled,
		"write_enabled": settings.WriteEnabled,
	})

	c.JSON(http.StatusOK, gin.H{
		"enabled":       settings.Enabled,
		"write_enabled": settings.WriteEnabled,
		"root_jail":     settings.RootJail,
	})
}

// ---------------------------------------------------------------------------
// GET /sites/:siteId/files
// ---------------------------------------------------------------------------

// listDir returns one page of directory entries for the given path.
// Query params:
//
//	path   — site-relative directory path (default "/")
//	cursor — opaque resume cursor from a prior truncated response
func (h *Handler) listDir(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	dirPath := c.Query("path")
	if dirPath == "" {
		dirPath = "/"
	}
	var cursor *string
	if raw := c.Query("cursor"); raw != "" {
		cursor = &raw
	}

	result, err := h.svc.ListDir(c.Request.Context(), p.TenantID, siteID, dirPath, cursor)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesRead, siteID, map[string]any{
		"op":           "list",
		"path":         dirPath,
		"entry_count":  len(result.Entries),
		"truncated":    result.Truncated,
	})

	c.JSON(http.StatusOK, gin.H{
		"path":      result.Path,
		"entries":   result.Entries,
		"total":     result.Total,
		"truncated": result.Truncated,
		"cursor":    result.Cursor,
	})
}

// ---------------------------------------------------------------------------
// GET /sites/:siteId/files/content
// ---------------------------------------------------------------------------

// readContent returns the base64-encoded content of a small file (≤ 256 KiB).
// Query params:
//
//	path              — site-relative file path (required)
//	confirm_sensitive — "true" to acknowledge reading a sensitive file (owner only)
//
// Security: when path is a sensitive file, confirm_sensitive must be "true"
// AND the caller must hold owner-level permission. The elevated-severity audit
// entry is written regardless of whether the read succeeds.
func (h *Handler) readContent(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	filePath := c.Query("path")
	if filePath == "" {
		httpx.Error(c, domain.Validation("missing_path", "path query parameter is required"))
		return
	}

	confirmSensitive := c.Query("confirm_sensitive") == "true"
	isSensitive := IsSensitivePath(filePath)

	// Gate: sensitive files require confirm AND owner-level permission.
	if isSensitive {
		if !confirmSensitive {
			// Log the denied attempt (T9: log denials too).
			h.record(c, p, ActionSiteFilesSensitiveDenied, siteID, map[string]any{
				"op":     "read",
				"path":   filePath,
				"reason": "confirm_sensitive_missing",
			})
			httpx.Error(c, domain.Forbidden("confirm_sensitive_required",
				"this file is sensitive; set confirm_sensitive=true to confirm you intend to read it"))
			return
		}
		if !h.allows(c, authz.PermSiteFilesReadSensitive) {
			h.record(c, p, ActionSiteFilesSensitiveDenied, siteID, map[string]any{
				"op":     "read",
				"path":   filePath,
				"reason": "insufficient_permission",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"reading sensitive files requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.ReadFile(c.Request.Context(), p.TenantID, siteID, filePath, confirmSensitive)
	if err != nil {
		// Log the denied/failed attempt at elevated severity for sensitive paths.
		if isSensitive {
			h.record(c, p, ActionSiteFilesSensitiveDenied, siteID, map[string]any{
				"op":     "read",
				"path":   filePath,
				"reason": err.Error(),
			})
		}
		httpx.Error(c, err)
		return
	}

	// Audit: sensitive reads get an elevated action; ordinary reads use the
	// standard read action. Both carry the full path (required by T6).
	if isSensitive {
		h.record(c, p, ActionSiteFilesSensitiveRead, siteID, map[string]any{
			"op":        "read",
			"path":      filePath,
			"size":      result.Size,
			"truncated": result.Truncated,
		})
	} else {
		h.record(c, p, ActionSiteFilesRead, siteID, map[string]any{
			"op":        "read",
			"path":      filePath,
			"size":      result.Size,
			"truncated": result.Truncated,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"path":           result.Path,
		"size":           result.Size,
		"mtime":          result.Mtime,
		"mode":           result.Mode,
		"encoding":       "base64",
		"content_base64": result.ContentBase64,
		"truncated":      result.Truncated,
	})
}

// ---------------------------------------------------------------------------
// POST /sites/:siteId/files/download
// ---------------------------------------------------------------------------

// downloadBody is the request body for POST /files/download.
type downloadBody struct {
	// Path is the site-relative path to stage for download (required).
	Path string `json:"path"`
}

// download stages a file for browser download:
//  1. CP mints presigned PUTs into its own object-storage staging area.
//  2. CP issues file_download_prepare; the agent uploads chunks.
//  3. CP mints a presigned GET URL for the browser (≤5 min TTL).
//  4. CP persists a file_transfers row (bookkeeping).
//
// The presigned GET URL is returned to the caller; the browser fetches it
// directly from object storage without going through the CP again. This keeps
// large files entirely off the CP's response path (same rule as backups).
//
// Sensitive paths require confirm_sensitive in the body AND owner-level
// permission (same gate as readContent).
func (h *Handler) download(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body downloadBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}

	isSensitive := IsSensitivePath(body.Path)
	if isSensitive {
		if !h.allows(c, authz.PermSiteFilesReadSensitive) {
			h.record(c, p, ActionSiteFilesSensitiveDenied, siteID, map[string]any{
				"op":     "download",
				"path":   body.Path,
				"reason": "insufficient_permission",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"downloading sensitive files requires owner-level permission"))
			return
		}
	}

	createdBy := p.GetUserID()
	result, err := h.svc.PrepareDownload(c.Request.Context(), p.TenantID, siteID, body.Path, createdBy)
	if err != nil {
		if isSensitive {
			h.record(c, p, ActionSiteFilesSensitiveDenied, siteID, map[string]any{
				"op":     "download",
				"path":   body.Path,
				"reason": err.Error(),
			})
		}
		httpx.Error(c, err)
		return
	}

	if isSensitive {
		h.record(c, p, ActionSiteFilesSensitiveRead, siteID, map[string]any{
			"op":          "download",
			"path":        body.Path,
			"transfer_id": result.TransferID.String(),
			"size_bytes":  result.SizeBytes,
		})
	} else {
		h.record(c, p, ActionSiteFilesRead, siteID, map[string]any{
			"op":          "download",
			"path":        body.Path,
			"transfer_id": result.TransferID.String(),
			"size_bytes":  result.SizeBytes,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"transfer_id":  result.TransferID.String(),
		"download_url": result.DownloadURL,
		"size_bytes":   result.SizeBytes,
		"chunk_count":  result.ChunkCount,
		"expires_at":   result.ExpiresAt.UTC().Unix(),
	})
}

// ---------------------------------------------------------------------------
// P2: PUT /sites/:siteId/files/content  (write small text file)
// ---------------------------------------------------------------------------

// writeContentBody is the request body for PUT /files/content.
type writeContentBody struct {
	// Path is the site-relative file path to create or overwrite (required).
	Path string `json:"path"`
	// ContentBase64 is the base64-encoded file content (≤ 256 KiB).
	ContentBase64 string `json:"content_base64"`
	// ConfirmExecutableWrite must be true when the target path matches the
	// executable-extension deny-list or is inside a PHP-executable web dir.
	// Requires PermSiteFilesWriteCode (owner) — rejected otherwise.
	ConfirmExecutableWrite bool `json:"confirm_executable_write"`
	// ConfirmSensitive must be true when the target path matches the
	// sensitive-file deny-list. Also requires PermSiteFilesWriteCode (owner).
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// writeContent creates or overwrites a small text file on the site.
// Elevated guards (T1/T6): if confirm_executable_write or confirm_sensitive is
// set, the caller must additionally hold PermSiteFilesWriteCode (owner).
func (h *Handler) writeContent(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body writeContentBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}

	// Elevated-permission gate: executable/sensitive writes require owner.
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			reason := "executable"
			if body.ConfirmSensitive {
				reason = "sensitive"
			}
			h.record(c, p, ActionSiteFilesWriteCodeDenied, siteID, map[string]any{
				"op":     "write",
				"path":   body.Path,
				"reason": reason,
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"writing executable or sensitive files requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.WriteFile(c.Request.Context(), p.TenantID, siteID, body.Path, body.ContentBase64, body.ConfirmExecutableWrite, body.ConfirmSensitive)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Record elevated audit for executable/sensitive writes (T1/T6).
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		reason := "executable"
		if body.ConfirmSensitive {
			reason = "sensitive"
		}
		h.record(c, p, ActionSiteFilesWriteCode, siteID, map[string]any{
			"op":     "write",
			"path":   result.Path,
			"size":   result.Size,
			"reason": reason,
		})
	} else {
		h.record(c, p, ActionSiteFilesWrite, siteID, map[string]any{
			"path": result.Path,
			"size": result.Size,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"path":  result.Path,
		"size":  result.Size,
		"mtime": result.Mtime,
		"mode":  result.Mode,
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/mkdir
// ---------------------------------------------------------------------------

// mkdirBody is the request body for POST /files/mkdir.
type mkdirBody struct {
	Path string `json:"path"`
}

// mkdir creates a directory on the site.
func (h *Handler) mkdir(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body mkdirBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}

	result, err := h.svc.Mkdir(c.Request.Context(), p.TenantID, siteID, body.Path)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesMkdir, siteID, map[string]any{
		"path": result.Path,
	})

	c.JSON(http.StatusOK, gin.H{
		"path": result.Path,
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/rename
// ---------------------------------------------------------------------------

// renameBody is the request body for POST /files/rename.
type renameBody struct {
	Src                   string `json:"src"`
	Dst                   string `json:"dst"`
	ConfirmExecutableWrite bool   `json:"confirm_executable_write"`
	ConfirmSensitive      bool   `json:"confirm_sensitive"`
}

// rename renames or moves a file/directory within the jail.
// Elevated-permission gate mirrors writeContent.
func (h *Handler) rename(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body renameBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Src == "" || body.Dst == "" {
		httpx.Error(c, domain.Validation("missing_path", "src and dst are required"))
		return
	}

	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			reason := "executable"
			if body.ConfirmSensitive {
				reason = "sensitive"
			}
			h.record(c, p, ActionSiteFilesWriteCodeDenied, siteID, map[string]any{
				"op":     "rename",
				"src":    body.Src,
				"dst":    body.Dst,
				"reason": reason,
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"renaming to an executable or sensitive path requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.Rename(c.Request.Context(), p.TenantID, siteID, body.Src, body.Dst, body.ConfirmExecutableWrite, body.ConfirmSensitive)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		reason := "executable"
		if body.ConfirmSensitive {
			reason = "sensitive"
		}
		h.record(c, p, ActionSiteFilesWriteCode, siteID, map[string]any{
			"op":     "rename",
			"src":    result.Src,
			"dst":    result.Dst,
			"reason": reason,
		})
	} else {
		h.record(c, p, ActionSiteFilesRename, siteID, map[string]any{
			"src": result.Src,
			"dst": result.Dst,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"src": result.Src,
		"dst": result.Dst,
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/delete
// ---------------------------------------------------------------------------

// deleteFileBody is the request body for POST /files/delete.
type deleteFileBody struct {
	// Path is the site-relative path to delete (required).
	Path string `json:"path"`
	// Recursive, when true, deletes a non-empty directory and all contents.
	Recursive bool `json:"recursive"`
	// Confirm must be exactly "DELETE" — the typed confirmation token (T12/T13).
	Confirm string `json:"confirm"`
}

// deleteFile deletes a file or directory.
// Security gates:
//  1. PermSiteFilesWrite (admin+) — inherited from route middleware.
//  2. PermSiteFilesDelete (owner) — checked here; denial is audited.
//  3. confirm="DELETE" — typed token; missing → 400 with clear code.
func (h *Handler) deleteFile(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body deleteFileBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}

	// Gate 1: typed confirm token (T12/T13).
	if body.Confirm != "DELETE" {
		h.record(c, p, ActionSiteFilesDeleteDenied, siteID, map[string]any{
			"path":   body.Path,
			"reason": "confirm_token_missing",
		})
		httpx.Error(c, domain.Validation("confirm_required",
			`delete requires confirm="DELETE" in the request body`))
		return
	}

	// Gate 2: owner permission required (PermSiteFilesDelete).
	if !h.allows(c, authz.PermSiteFilesDelete) {
		h.record(c, p, ActionSiteFilesDeleteDenied, siteID, map[string]any{
			"path":   body.Path,
			"reason": "insufficient_permission",
		})
		httpx.Error(c, domain.Forbidden("insufficient_permission",
			"deleting files requires owner-level permission"))
		return
	}

	result, err := h.svc.Delete(c.Request.Context(), p.TenantID, siteID, body.Path, body.Recursive)
	if err != nil {
		h.record(c, p, ActionSiteFilesDeleteDenied, siteID, map[string]any{
			"path":   body.Path,
			"reason": err.Error(),
		})
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesDelete, siteID, map[string]any{
		"path":      result.Path,
		"recursive": body.Recursive,
		"deleted":   result.Deleted,
	})

	c.JSON(http.StatusOK, gin.H{
		"path":    result.Path,
		"deleted": result.Deleted,
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/chmod
// ---------------------------------------------------------------------------

// chmodBody is the request body for POST /files/chmod.
type chmodBody struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// chmod sets the permission mode of a file or directory.
// The agent validates the mode against a safe allowlist (no setuid, no world-write).
func (h *Handler) chmod(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body chmodBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" || body.Mode == "" {
		httpx.Error(c, domain.Validation("missing_field", "path and mode are required"))
		return
	}

	result, err := h.svc.Chmod(c.Request.Context(), p.TenantID, siteID, body.Path, body.Mode)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesChmod, siteID, map[string]any{
		"path": result.Path,
		"mode": result.Mode,
	})

	c.JSON(http.StatusOK, gin.H{
		"path": result.Path,
		"mode": result.Mode,
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/upload  (prepare — mint presigned PUTs)
// ---------------------------------------------------------------------------

// prepareUploadBody is the request body for POST /files/upload.
type prepareUploadBody struct {
	// Path is the site-relative target path for the uploaded file (required).
	Path string `json:"path"`
	// PartCount is the number of chunks the browser will PUT. Must be 1–32.
	// Default: 1 (single-chunk for files ≤ 5 MiB).
	PartCount int `json:"part_count"`
	// ConfirmExecutableWrite must be true when Path matches the executable-
	// extension deny-list. Requires PermSiteFilesWriteCode (owner).
	ConfirmExecutableWrite bool `json:"confirm_executable_write"`
	// ConfirmSensitive must be true when Path matches the sensitive-file
	// deny-list. Requires PermSiteFilesWriteCode (owner).
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// prepareUpload mints presigned S3 PUT URLs for the browser to push chunks.
// The browser PUTs the chunks, then calls /files/upload/apply to trigger the
// agent to fetch, reassemble, validate, and atomic-swap the file into place.
func (h *Handler) prepareUpload(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body prepareUploadBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}
	if body.PartCount < 1 {
		body.PartCount = 1
	}

	// Elevated-permission gate for executable/sensitive target paths.
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			reason := "executable"
			if body.ConfirmSensitive {
				reason = "sensitive"
			}
			h.record(c, p, ActionSiteFilesWriteCodeDenied, siteID, map[string]any{
				"op":     "upload_prepare",
				"path":   body.Path,
				"reason": reason,
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"uploading to an executable or sensitive path requires owner-level permission"))
			return
		}
	}

	createdBy := p.GetUserID()
	result, err := h.svc.PrepareUpload(c.Request.Context(), p.TenantID, siteID, body.Path, body.PartCount, createdBy)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Return the presigned PUTs to the browser. Never log the URLs (T8).
	puts := make([]map[string]any, len(result.PresignedPuts))
	for i, pt := range result.PresignedPuts {
		puts[i] = map[string]any{
			"index": pt.Index,
			"url":   pt.URL,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"transfer_id":    result.TransferID.String(),
		"object_key":     result.ObjectKey,
		"presigned_puts": puts,
		"expires_at":     result.ExpiresAt.UTC().Unix(),
	})
}

// ---------------------------------------------------------------------------
// P2: POST /sites/:siteId/files/upload/apply  (apply staged upload)
// ---------------------------------------------------------------------------

// applyUploadBody is the request body for POST /files/upload/apply.
type applyUploadBody struct {
	// Path is the site-relative target path (must match the prepare call).
	Path string `json:"path"`
	// ObjectKey is the S3 staging key prefix returned by the prepare step.
	ObjectKey string `json:"object_key"`
	// PartCount is the number of chunks staged (must match the prepare call).
	PartCount int `json:"part_count"`
	// TotalSize is the expected assembled file size in bytes.
	TotalSize int64 `json:"total_size"`
	// SHA256 is the hex-encoded SHA-256 of the assembled file content.
	// The agent validates this after reassembly before the atomic swap.
	SHA256 string `json:"sha256"`
	// ConfirmExecutableWrite mirrors the prepare call (passed to the agent).
	ConfirmExecutableWrite bool `json:"confirm_executable_write"`
	// ConfirmSensitive mirrors the prepare call (passed to the agent).
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// applyUpload mints presigned GET URLs for the staged chunks and issues
// file_upload_apply to the agent.
func (h *Handler) applyUpload(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body applyUploadBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" || body.ObjectKey == "" || body.SHA256 == "" {
		httpx.Error(c, domain.Validation("missing_field", "path, object_key, and sha256 are required"))
		return
	}
	if body.PartCount < 1 {
		body.PartCount = 1
	}

	// Elevated-permission gate mirrors the prepare step.
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			reason := "executable"
			if body.ConfirmSensitive {
				reason = "sensitive"
			}
			h.record(c, p, ActionSiteFilesWriteCodeDenied, siteID, map[string]any{
				"op":     "upload_apply",
				"path":   body.Path,
				"reason": reason,
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"uploading to an executable or sensitive path requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.ApplyUpload(c.Request.Context(), p.TenantID, siteID, body.Path, body.ObjectKey, body.SHA256, body.PartCount, body.TotalSize, body.ConfirmExecutableWrite, body.ConfirmSensitive)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Elevated audit for executable/sensitive uploads.
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		reason := "executable"
		if body.ConfirmSensitive {
			reason = "sensitive"
		}
		h.record(c, p, ActionSiteFilesWriteCode, siteID, map[string]any{
			"op":         "upload",
			"path":       result.Path,
			"size_bytes": result.Size,
			"reason":     reason,
		})
	} else {
		h.record(c, p, ActionSiteFilesUpload, siteID, map[string]any{
			"path":       result.Path,
			"size_bytes": result.Size,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":    true,
		"path":  result.Path,
		"size":  result.Size,
		"mtime": result.Mtime,
	})
}

// ---------------------------------------------------------------------------
// P3: POST /sites/:siteId/files/archive
// ---------------------------------------------------------------------------

// archiveBody is the request body for POST /files/archive.
type archiveBody struct {
	// Paths is the list of site-relative paths to include in the archive
	// (required; at least one path).
	Paths []string `json:"paths"`
	// ConfirmSensitive must be true when any path in Paths matches the
	// sensitive-file deny-list (wp-config.php, .env*, *.pem, …).
	// Also requires PermSiteFilesReadSensitive (owner); a non-owner or a caller
	// that omits this flag is rejected at the CP (agent never called) and the
	// denial is audited at elevated severity (T9).
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// createArchive zips the requested paths on the agent, stages the archive in
// object storage, and returns a short-TTL presigned GET URL for the browser.
// Gate: PermSiteFilesRead + files_enabled (read-only operation; archive is a
// download convenience, not a write).
//
// Sensitive-path gate (F1 — CRITICAL): if any path in Paths is sensitive,
// the caller must hold PermSiteFilesReadSensitive (owner) AND set
// confirm_sensitive=true. Both checks must pass; a partial match is denied.
// The denial is audited at elevated severity regardless of whether it was a
// permission failure or a missing flag.
func (h *Handler) createArchive(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body archiveBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if len(body.Paths) == 0 {
		httpx.Error(c, domain.Validation("missing_paths", "at least one path is required"))
		return
	}

	// Sensitive-path gate: scan all paths in the request.
	hasSensitive := false
	for _, p2 := range body.Paths {
		if IsSensitivePath(p2) {
			hasSensitive = true
			break
		}
	}

	if hasSensitive {
		if !body.ConfirmSensitive {
			h.record(c, p, ActionSiteFilesArchiveSensitiveDenied, siteID, map[string]any{
				"op":     "archive",
				"paths":  body.Paths,
				"reason": "confirm_sensitive_missing",
			})
			httpx.Error(c, domain.Forbidden("confirm_sensitive_required",
				"one or more paths are sensitive files; set confirm_sensitive=true to confirm you intend to archive them"))
			return
		}
		if !h.allows(c, authz.PermSiteFilesReadSensitive) {
			h.record(c, p, ActionSiteFilesArchiveSensitiveDenied, siteID, map[string]any{
				"op":     "archive",
				"paths":  body.Paths,
				"reason": "insufficient_permission",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"archiving sensitive files requires owner-level permission"))
			return
		}
	}

	createdBy := p.GetUserID()
	result, err := h.svc.CreateArchive(c.Request.Context(), p.TenantID, siteID, body.Paths, createdBy, body.ConfirmSensitive)
	if err != nil {
		if hasSensitive {
			h.record(c, p, ActionSiteFilesArchiveSensitiveDenied, siteID, map[string]any{
				"op":     "archive",
				"paths":  body.Paths,
				"reason": err.Error(),
			})
		}
		httpx.Error(c, err)
		return
	}

	// Elevated audit for sensitive archives (T6); standard audit otherwise.
	if hasSensitive {
		h.record(c, p, ActionSiteFilesArchiveSensitiveRead, siteID, map[string]any{
			"paths":       body.Paths,
			"transfer_id": result.TransferID.String(),
			"size_bytes":  result.SizeBytes,
			"chunk_count": result.ChunkCount,
		})
	} else {
		h.record(c, p, ActionSiteFilesArchive, siteID, map[string]any{
			"paths":       body.Paths,
			"transfer_id": result.TransferID.String(),
			"size_bytes":  result.SizeBytes,
			"chunk_count": result.ChunkCount,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"transfer_id":  result.TransferID.String(),
		"download_url": result.DownloadURL,
		"size_bytes":   result.SizeBytes,
		"chunk_count":  result.ChunkCount,
		"expires_at":   result.ExpiresAt.UTC().Unix(),
	})
}

// ---------------------------------------------------------------------------
// P3: POST /sites/:siteId/files/extract
// ---------------------------------------------------------------------------

// extractBody is the request body for POST /files/extract.
type extractBody struct {
	// ArchivePath is the site-relative path to the ZIP archive (required).
	ArchivePath string `json:"archive_path"`
	// DestPath is the site-relative directory to extract into (required).
	DestPath string `json:"dest_path"`
	// ConfirmExecutableWrite must be true when any archive entry would land in
	// an executable-extension path. Requires PermSiteFilesWriteCode (owner).
	// A non-owner passing this is rejected at the handler with an elevated-
	// severity denial audit entry — the agent is never called.
	ConfirmExecutableWrite bool `json:"confirm_executable_write"`
	// ConfirmSensitive must be true when any archive entry would land in a
	// sensitive path. Same owner gate as ConfirmExecutableWrite.
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// extract issues a file_extract command after gating on write mode and the
// elevated-permission check for the confirm flags.
//
// Security: zip_slip / zip_bomb map to 422 (the archive is structurally valid
// but malicious); bad_archive / not_archive map to 400. The agent is the
// authoritative enforcer of the zip-slip/zip-bomb guard; the CP propagates the
// confirm flags after verifying the owner gate — never before.
func (h *Handler) extract(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body extractBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.ArchivePath == "" {
		httpx.Error(c, domain.Validation("missing_archive_path", "archive_path is required"))
		return
	}
	if body.DestPath == "" {
		httpx.Error(c, domain.Validation("missing_dest_path", "dest_path is required"))
		return
	}

	// Elevated-permission gate: confirm flags require owner (PermSiteFilesWriteCode).
	// A non-owner passing either flag is DENIED before the agent is called and the
	// denial is audited at elevated severity (T9). This is the same gate as
	// writeContent / rename — extract is a de-facto multi-file write.
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			reason := "executable"
			if body.ConfirmSensitive {
				reason = "sensitive"
			}
			h.record(c, p, ActionSiteFilesExtractDenied, siteID, map[string]any{
				"archive_path": body.ArchivePath,
				"dest_path":    body.DestPath,
				"reason":       reason + "_requires_owner",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"extracting to an executable or sensitive path requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.Extract(c.Request.Context(), p.TenantID, siteID, body.ArchivePath, body.DestPath, body.ConfirmExecutableWrite, body.ConfirmSensitive)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Record elevated audit when confirm flags were set (mirrors writeContent).
	if body.ConfirmExecutableWrite || body.ConfirmSensitive {
		reason := "executable"
		if body.ConfirmSensitive {
			reason = "sensitive"
		}
		h.record(c, p, ActionSiteFilesExtract, siteID, map[string]any{
			"archive_path": body.ArchivePath,
			"dest_path":    result.DestPath,
			"extracted":    result.Extracted,
			"elevated":     reason,
		})
	} else {
		h.record(c, p, ActionSiteFilesExtract, siteID, map[string]any{
			"archive_path": body.ArchivePath,
			"dest_path":    result.DestPath,
			"extracted":    result.Extracted,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"dest_path": result.DestPath,
		"extracted": result.Extracted,
	})
}

// ---------------------------------------------------------------------------
// P3: GET /sites/:siteId/files/search
// ---------------------------------------------------------------------------

// search issues a file_search command to the agent.
// Query params:
//
//	path   — site-relative root to search under (default "/")
//	q      — search term (required)
//	mode   — "name" or "content" (default "name")
//	cursor — opaque resume cursor from a prior truncated response
func (h *Handler) search(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	searchPath := c.Query("path")
	if searchPath == "" {
		searchPath = "/"
	}
	query := c.Query("q")
	if query == "" {
		httpx.Error(c, domain.Validation("missing_query", "q query parameter is required"))
		return
	}
	mode := c.Query("mode")
	if mode == "" {
		mode = "name"
	}
	var cursor *string
	if raw := c.Query("cursor"); raw != "" {
		cursor = &raw
	}

	result, err := h.svc.Search(c.Request.Context(), p.TenantID, siteID, searchPath, query, mode, cursor)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesSearch, siteID, map[string]any{
		"path":        searchPath,
		"query":       query,
		"mode":        mode,
		"match_count": len(result.Matches),
		"truncated":   result.Truncated,
	})

	c.JSON(http.StatusOK, gin.H{
		"matches":   result.Matches,
		"truncated": result.Truncated,
		"cursor":    result.Cursor,
	})
}

// ---------------------------------------------------------------------------
// P3: GET /sites/:siteId/files/versions
// ---------------------------------------------------------------------------

// listVersions returns the version history for a single file.
// Query params:
//
//	path — site-relative file path (required)
//
// Sensitive-path gate (F3 — HIGH): if path is a sensitive file, the caller must
// hold PermSiteFilesReadSensitive (owner) to list its version history. A
// non-owner is denied (403) and the denial is audited at elevated severity. The
// gate prevents leaking that sensitive backups exist to non-owners.
func (h *Handler) listVersions(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	filePath := c.Query("path")
	if filePath == "" {
		httpx.Error(c, domain.Validation("missing_path", "path query parameter is required"))
		return
	}

	// Sensitive-path gate: listing versions of a sensitive file reveals that
	// backups exist (information leak to non-owners — deny at the CP layer).
	if IsSensitivePath(filePath) {
		if !h.allows(c, authz.PermSiteFilesReadSensitive) {
			h.record(c, p, ActionSiteFilesVersionsListDenied, siteID, map[string]any{
				"op":     "versions_list",
				"path":   filePath,
				"reason": "insufficient_permission",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"listing version history for sensitive files requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.ListVersions(c.Request.Context(), p.TenantID, siteID, filePath)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.record(c, p, ActionSiteFilesVersionsList, siteID, map[string]any{
		"path":          filePath,
		"version_count": len(result.Versions),
	})

	c.JSON(http.StatusOK, gin.H{
		"versions": result.Versions,
	})
}

// ---------------------------------------------------------------------------
// P3: POST /sites/:siteId/files/versions/restore
// ---------------------------------------------------------------------------

// restoreVersionBody is the request body for POST /files/versions/restore.
type restoreVersionBody struct {
	// Path is the site-relative path of the file to restore (required).
	Path string `json:"path"`
	// VersionID is the opaque version identifier returned by listVersions (required).
	VersionID string `json:"version_id"`
	// ConfirmSensitive must be true when Path matches the sensitive-file
	// deny-list (wp-config.php, .env*, *.pem, …). Also requires
	// PermSiteFilesWriteCode (owner); a non-owner or a caller that omits this
	// flag is rejected at the CP (agent never called) and the denial is
	// audited at elevated severity (T9).
	ConfirmSensitive bool `json:"confirm_sensitive"`
}

// restoreVersion restores a file to a prior version issued by file_version_restore.
// Gate: PermSiteFilesWrite + files_write_enabled (same as mkdir/rename/chmod).
// Audited with path + version_id on both success and any failure that reaches
// the agent (transport errors are logged by the service-level error mapper).
//
// Sensitive-path gate (F3 — HIGH): if Path is a sensitive file, the caller must
// additionally hold PermSiteFilesWriteCode (owner) AND set confirm_sensitive=true.
// Both checks must pass. The denial is audited at elevated severity (T9). The
// agent receives confirm_sensitive only after the gate passes, and independently
// re-checks the path — the agent is the last line of defense.
func (h *Handler) restoreVersion(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body restoreVersionBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.Path == "" {
		httpx.Error(c, domain.Validation("missing_path", "path is required"))
		return
	}
	if body.VersionID == "" {
		httpx.Error(c, domain.Validation("missing_version_id", "version_id is required"))
		return
	}

	// Sensitive-path gate: restoring wp-config.php or any sensitive file from a
	// stale version is a high-risk operation — require owner + explicit confirm.
	isSensitive := IsSensitivePath(body.Path)
	if isSensitive {
		if !body.ConfirmSensitive {
			h.record(c, p, ActionSiteFilesVersionRestoreDenied, siteID, map[string]any{
				"op":         "version_restore",
				"path":       body.Path,
				"version_id": body.VersionID,
				"reason":     "confirm_sensitive_missing",
			})
			httpx.Error(c, domain.Forbidden("confirm_sensitive_required",
				"this file is sensitive; set confirm_sensitive=true to confirm you intend to restore it"))
			return
		}
		if !h.allows(c, authz.PermSiteFilesWriteCode) {
			h.record(c, p, ActionSiteFilesVersionRestoreDenied, siteID, map[string]any{
				"op":         "version_restore",
				"path":       body.Path,
				"version_id": body.VersionID,
				"reason":     "insufficient_permission",
			})
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"restoring sensitive files requires owner-level permission"))
			return
		}
	}

	result, err := h.svc.RestoreVersion(c.Request.Context(), p.TenantID, siteID, body.Path, body.VersionID, body.ConfirmSensitive)
	if err != nil {
		if isSensitive {
			h.record(c, p, ActionSiteFilesVersionRestoreDenied, siteID, map[string]any{
				"op":         "version_restore",
				"path":       body.Path,
				"version_id": body.VersionID,
				"reason":     err.Error(),
			})
		}
		httpx.Error(c, err)
		return
	}

	// Elevated audit for sensitive version restores (T6); standard audit otherwise.
	if isSensitive {
		h.record(c, p, ActionSiteFilesVersionRestoreSensitive, siteID, map[string]any{
			"path":       result.Path,
			"version_id": body.VersionID,
			"size":       result.Size,
			"mtime":      result.Mtime,
		})
	} else {
		h.record(c, p, ActionSiteFilesVersionRestore, siteID, map[string]any{
			"path":       result.Path,
			"version_id": body.VersionID,
			"size":       result.Size,
			"mtime":      result.Mtime,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"path":       result.Path,
		"size":       result.Size,
		"mtime":      result.Mtime,
		"version_id": body.VersionID,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseSiteID(c *gin.Context) (uuid.UUID, bool) {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, false
	}
	return siteID, true
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20)) // 1 MiB body cap
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

// allows answers the in-handler permission question for conditional behaviour
// (a field the caller may not see, a branch it may not take). It MUST route
// through authz.PrincipalAllows, never authz.Allows: the latter takes only the
// role, so a capability key — whose role is a placeholder and whose authority
// is the explicit set — would be handed its full role rank here. That was the
// bypass #510 exists to close, and TestNoDirectAllowsOutsideAuthz keeps it shut.
func (h *Handler) allows(c *gin.Context, perm authz.Permission) bool {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		return false
	}
	return authz.PrincipalAllows(p, perm)
}

func (h *Handler) record(c *gin.Context, p domain.Principal, action string, siteID uuid.UUID, meta map[string]any) {
	if h.audit == nil {
		return
	}
	actorType := audit.ActorUser
	if p.Type == domain.PrincipalAPIKey {
		actorType = audit.ActorAPIKey
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType,
		ActorID:    p.ActorID(),
		Action:     action,
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   meta,
	})
}
