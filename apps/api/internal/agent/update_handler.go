package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ADR-042 — CP-driven WordPress agent self-update.
//
// GET /agent/v1/update/manifest returns a SIGNED release manifest the agent
// verifies before letting WordPress install a new plugin zip. The manifest is a
// detached-Ed25519-signed JSON blob (NOT a JWT — see agentcmd.SignManifest)
// carrying a short-lived presigned GCS download URL plus the package sha256 +
// size. The agent re-checks aud/cmd/slug/iat/exp/jti, enforces a downgrade
// guard, host-allowlists the URL, and verifies the streamed sha256 before
// WP_Upgrader swaps any files (ADR-042 §2).
//
// State is stateless from object storage: the release pipeline writes
// agent-releases/latest.json + the versioned package; this handler only reads.

// updateManifestKey is the well-known object key the release pipeline
// (make agent-release) writes the pointer manifest to.
const updateManifestKey = "agent-releases/latest.json"

// updatePackagePrefix bounds which object keys this handler will presign. A
// malformed latest.json cannot make the CP mint a presigned URL for an
// arbitrary object outside the release area.
const updatePackagePrefix = "agent-releases/"

// expectedAgentSlug pins the plugin this channel serves. A latest.json with any
// other slug is rejected (defense against a mis-uploaded manifest).
const expectedAgentSlug = "wpmgr-agent"

// maxLatestJSONBytes caps how much of latest.json we read (it is tiny).
const maxLatestJSONBytes = 64 << 10

// releaseManifest is the subset of agent-releases/latest.json we consume.
type releaseManifest struct {
	Slug             string            `json:"slug"`
	Plugin           string            `json:"plugin"`
	Version          string            `json:"version"`
	MinVersion       string            `json:"min_version"`
	PackageObjectKey string            `json:"package_object_key"`
	PackageSHA256    string            `json:"package_sha256"`
	PackageSize      int64             `json:"package_size"`
	Requires         string            `json:"requires"`
	RequiresPHP      string            `json:"requires_php"`
	Tested           string            `json:"tested"`
	Sections         map[string]string `json:"sections"`
}

// signedManifestClaims is the exact payload the CP signs and the agent verifies.
// Field order is fixed by struct order so the marshalled bytes are deterministic
// for a given input; the agent verifies the signature over the bytes verbatim
// (it base64url-decodes the `manifest` field) and never re-serializes, so JSON
// canonicalization differences cannot break verification.
type signedManifestClaims struct {
	Aud           string            `json:"aud"` // target site id — pins install to one site
	Cmd           string            `json:"cmd"` // agentcmd.CmdUpdateManifest
	Slug          string            `json:"slug"`
	Version       string            `json:"version"`
	MinVersion    string            `json:"min_version"`
	PackageURL    string            `json:"package_url"`    // short-lived presigned GET
	PackageSHA256 string            `json:"package_sha256"` // lowercase hex
	PackageSize   int64             `json:"package_size"`
	Requires      string            `json:"requires,omitempty"`
	RequiresPHP   string            `json:"requires_php,omitempty"`
	Tested        string            `json:"tested,omitempty"`
	Sections      map[string]string `json:"sections,omitempty"`
	Iat           int64             `json:"iat"`
	Exp           int64             `json:"exp"`
	Jti           string            `json:"jti"`
}

// ManifestStore is the object-storage subset the handler needs (satisfied by
// *blobstore.Store). GetViaPresign reads latest.json through a presigned URL
// (a live SDK GetObject 403s against GCS — see blobstore.GetViaPresign) and
// returns blobstore.ErrNotFound when it is absent; PresignGet mints a
// short-lived GET URL for the package object the agent downloads.
type ManifestStore interface {
	GetViaPresign(ctx context.Context, key string) (io.ReadCloser, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ManifestSigner mints the detached signature over the manifest payload
// (satisfied by *agentcmd.Signer).
type ManifestSigner interface {
	SignManifest(payload []byte) string
}

// UpdateHandler serves GET /agent/v1/update/manifest.
type UpdateHandler struct {
	store      ManifestStore
	signer     ManifestSigner
	presignTTL time.Duration
}

// NewUpdateHandler wires the self-update manifest handler. presignTTL bounds how
// long the minted package URL stays valid; it is also used as the manifest's own
// exp window. ADR-042 caps it at 300s.
func NewUpdateHandler(store ManifestStore, signer ManifestSigner, presignTTL time.Duration) *UpdateHandler {
	if presignTTL <= 0 || presignTTL > 5*time.Minute {
		presignTTL = 5 * time.Minute
	}
	return &UpdateHandler{store: store, signer: signer, presignTTL: presignTTL}
}

// Register mounts the route on the agent-authenticated group.
func (h *UpdateHandler) Register(r *gin.RouterGroup) {
	r.GET("/update/manifest", h.manifest)
}

func (h *UpdateHandler) manifest(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	if h.store == nil || h.signer == nil {
		httpx.Error(c, domain.Unavailable("update_unwired", "self-update channel not wired"))
		return
	}

	rel, err := h.readLatest(c.Request.Context())
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			// No published release — nothing to offer. 204 keeps the agent quiet.
			c.Status(http.StatusNoContent)
			return
		}
		if errors.Is(err, errInvalidManifest) {
			slog.ErrorContext(c.Request.Context(), "ADR-042 manifest invalid", slog.String("site_id", id.SiteID.String()))
			httpx.Error(c, domain.Internal("update_manifest_invalid", "published release manifest is malformed"))
			return
		}
		slog.ErrorContext(c.Request.Context(), "ADR-042 manifest read failed", slog.String("err", err.Error()), slog.String("site_id", id.SiteID.String()))
		httpx.Error(c, domain.Internal("update_manifest_read_failed", "failed to read release manifest").WithCause(err))
		return
	}

	// Mint a fresh, short-lived presigned GET for the package. The agent fetches
	// this manifest fresh at install time and downloads within the TTL.
	pkgURL, err := h.store.PresignGet(c.Request.Context(), rel.PackageObjectKey, h.presignTTL)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "ADR-042 presign failed", slog.String("err", err.Error()), slog.String("key", rel.PackageObjectKey), slog.String("site_id", id.SiteID.String()))
		httpx.Error(c, domain.Internal("update_presign_failed", "failed to presign release package").WithCause(err))
		return
	}

	jti, err := randomJTI()
	if err != nil {
		httpx.Error(c, domain.Internal("update_jti_failed", "failed to mint manifest id").WithCause(err))
		return
	}

	now := time.Now()
	claims := signedManifestClaims{
		Aud:           id.SiteID.String(),
		Cmd:           agentcmd.CmdUpdateManifest,
		Slug:          rel.Slug,
		Version:       rel.Version,
		MinVersion:    rel.MinVersion,
		PackageURL:    pkgURL,
		PackageSHA256: rel.PackageSHA256,
		PackageSize:   rel.PackageSize,
		Requires:      rel.Requires,
		RequiresPHP:   rel.RequiresPHP,
		Tested:        rel.Tested,
		Sections:      rel.Sections,
		Iat:           now.Unix(),
		Exp:           now.Add(h.presignTTL).Unix(),
		Jti:           jti,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		httpx.Error(c, domain.Internal("update_marshal_failed", "failed to encode manifest").WithCause(err))
		return
	}
	sig := h.signer.SignManifest(payload)

	// The agent base64url-decodes `manifest` to recover the EXACT signed bytes,
	// verifies `signature` over them, then JSON-decodes. Never log package_url.
	c.JSON(http.StatusOK, gin.H{
		"manifest":  base64.RawURLEncoding.EncodeToString(payload),
		"signature": sig,
	})
}

// errInvalidManifest signals a published latest.json that failed validation.
var errInvalidManifest = errors.New("agent: invalid release manifest")

// readLatest loads + validates agent-releases/latest.json.
func (h *UpdateHandler) readLatest(ctx context.Context) (releaseManifest, error) {
	rc, err := h.store.GetViaPresign(ctx, updateManifestKey)
	if err != nil {
		return releaseManifest{}, err
	}
	defer rc.Close()

	body, err := io.ReadAll(io.LimitReader(rc, maxLatestJSONBytes))
	if err != nil {
		return releaseManifest{}, err
	}
	var rel releaseManifest
	if uerr := json.Unmarshal(body, &rel); uerr != nil {
		return releaseManifest{}, errInvalidManifest
	}
	if rel.Slug != expectedAgentSlug ||
		rel.Version == "" ||
		!isHex64(rel.PackageSHA256) ||
		rel.PackageSize <= 0 {
		return releaseManifest{}, errInvalidManifest
	}
	// Pin the presignable object to the single deterministic key for this
	// version (security review T8/finding 3): the CP must presign EXACTLY
	// agent-releases/<version>/wpmgr-agent.zip, never any other object that
	// happens to share the agent-releases/ prefix. A latest.json naming a
	// different in-prefix key (stale zip, latest.json itself, …) is rejected.
	if rel.PackageObjectKey != updatePackagePrefix+rel.Version+"/wpmgr-agent.zip" {
		return releaseManifest{}, errInvalidManifest
	}
	if rel.MinVersion == "" {
		rel.MinVersion = "0.0.0"
	}
	return rel, nil
}

// isHex64 reports whether s is exactly 64 lowercase hex chars (a sha256 digest).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// randomJTI returns a 128-bit hex anti-replay id (mirrors agentcmd.Mint's jti).
func randomJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
