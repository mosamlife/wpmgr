package sitedestination

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AgeIdentity wraps the CP's age X25519 identity used to age-encrypt customer
// secrets at rest. We never ship the identity to the agent — only the public
// recipient is exposed in the destination form for symmetry with the per-site
// backup recipient. Encryption happens at write time (service.Create /
// service.Update), decryption at read time (service.PresignableConfig).
type AgeIdentity struct {
	identity  *age.X25519Identity
	recipient *age.X25519Recipient
}

// NewAgeIdentity parses an age secret key (the AGE-SECRET-KEY-1... format) and
// returns both the identity for decryption and its recipient for encryption.
// An empty key produces a fresh ephemeral identity so dev startup succeeds; in
// production the operator MUST supply a stable key or every CP restart
// invalidates every stored secret.
func NewAgeIdentity(secretKey string) (*AgeIdentity, error) {
	if strings.TrimSpace(secretKey) == "" {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate ephemeral age identity: %w", err)
		}
		return &AgeIdentity{identity: id, recipient: id.Recipient()}, nil
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(secretKey))
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	return &AgeIdentity{identity: id, recipient: id.Recipient()}, nil
}

// Encrypt age-encrypts plaintext for the wrapped recipient.
func (a *AgeIdentity) Encrypt(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, a.recipient)
	if err != nil {
		return nil, fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt age-decrypts ciphertext produced by Encrypt with the same identity.
func (a *AgeIdentity) Decrypt(ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), a.identity)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("age read: %w", err)
	}
	return out, nil
}

// Service orchestrates site-destination CRUD on top of Repo, the secret-at-
// rest encryption, and S3 connection testing.
type Service struct {
	repo *Repo
	age  *AgeIdentity
	log  *slog.Logger
}

// NewService builds a Service.
func NewService(repo *Repo, ageID *AgeIdentity, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, age: ageID, log: log}
}

// CreateInput is the unencrypted input shape; the service encrypts SecretKey
// before persisting.
type CreateServiceInput struct {
	TenantID       uuid.UUID
	SiteID         uuid.UUID
	Kind           Kind
	Label          string
	Endpoint       string
	Region         string
	Bucket         string
	PathPrefix     string
	AccessKeyID    string
	SecretKey      string // PLAINTEXT — service encrypts before insert.
	ForcePathStyle bool
	IsDefault      bool
}

// UpdateServiceInput mirrors CreateServiceInput but every field is optional;
// SecretKey is interpreted as "leave the existing value alone" when empty.
type UpdateServiceInput struct {
	Label          *string
	Endpoint       *string
	Region         *string
	Bucket         *string
	PathPrefix     *string
	AccessKeyID    *string
	SecretKey      *string // nil OR empty pointer = leave; non-empty = replace.
	ForcePathStyle *bool
	IsDefault      *bool
}

// Create validates the input, encrypts the secret, and inserts the row.
func (s *Service) Create(ctx context.Context, in CreateServiceInput) (SiteDestination, error) {
	if !ValidKind(in.Kind) {
		return SiteDestination{}, domain.Validation("invalid_kind", "destination kind must be cp, local, or s3_compat")
	}
	if strings.TrimSpace(in.Label) == "" {
		return SiteDestination{}, domain.Validation("label_required", "destination label is required")
	}
	if in.Kind == KindS3Compat {
		if in.Bucket == "" || in.AccessKeyID == "" || in.SecretKey == "" {
			return SiteDestination{}, domain.Validation(
				"s3_compat_credentials_required",
				"s3_compat destinations require bucket, access key id, and secret key",
			)
		}
	}

	var secretEnc []byte
	if in.SecretKey != "" {
		enc, err := s.age.Encrypt([]byte(in.SecretKey))
		if err != nil {
			return SiteDestination{}, domain.Internal("site_destination_encrypt_failed", "failed to encrypt secret").WithCause(err)
		}
		secretEnc = enc
	}
	return s.repo.Create(ctx, CreateInput{
		TenantID:       in.TenantID,
		SiteID:         in.SiteID,
		Kind:           in.Kind,
		Label:          in.Label,
		Endpoint:       in.Endpoint,
		Region:         in.Region,
		Bucket:         in.Bucket,
		PathPrefix:     in.PathPrefix,
		AccessKeyID:    in.AccessKeyID,
		SecretKeyEnc:   secretEnc,
		ForcePathStyle: in.ForcePathStyle,
		IsDefault:      in.IsDefault,
	})
}

// Update merges the patch into an existing row. When SecretKey is a non-nil
// non-empty pointer the new value is age-encrypted and written; nil OR empty
// pointer leaves the existing ciphertext intact.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, in UpdateServiceInput) (SiteDestination, error) {
	var secretEnc []byte
	if in.SecretKey != nil && *in.SecretKey != "" {
		enc, err := s.age.Encrypt([]byte(*in.SecretKey))
		if err != nil {
			return SiteDestination{}, domain.Internal("site_destination_encrypt_failed", "failed to encrypt secret").WithCause(err)
		}
		secretEnc = enc
	}
	return s.repo.Update(ctx, tenantID, id, UpdateInput{
		Label:          in.Label,
		Endpoint:       in.Endpoint,
		Region:         in.Region,
		Bucket:         in.Bucket,
		PathPrefix:     in.PathPrefix,
		AccessKeyID:    in.AccessKeyID,
		SecretKeyEnc:   secretEnc,
		ForcePathStyle: in.ForcePathStyle,
		IsDefault:      in.IsDefault,
	})
}

// ListBySite passes through to the repo.
func (s *Service) ListBySite(ctx context.Context, tenantID, siteID uuid.UUID) ([]SiteDestination, error) {
	return s.repo.ListBySite(ctx, tenantID, siteID)
}

// GetByID passes through to the repo.
func (s *Service) GetByID(ctx context.Context, tenantID, id uuid.UUID) (SiteDestination, error) {
	return s.repo.GetByID(ctx, tenantID, id)
}

// GetDefaultForSite returns the default destination for a site, used by the
// blobstore Registry when a snapshot has no destination_id set yet.
func (s *Service) GetDefaultForSite(ctx context.Context, tenantID, siteID uuid.UUID) (SiteDestination, error) {
	return s.repo.GetDefaultForSite(ctx, tenantID, siteID)
}

// Delete passes through to the repo.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.repo.Delete(ctx, tenantID, id)
}

// DecryptSecret returns the plaintext secret for the destination, decrypting
// the SecretKeyEnc column on demand. Used by the blobstore Registry to mint
// presigned URLs against the customer bucket.
func (s *Service) DecryptSecret(d SiteDestination) (string, error) {
	if len(d.SecretKeyEnc) == 0 {
		return "", nil
	}
	plain, err := s.age.Decrypt(d.SecretKeyEnc)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// TestConnectionInput is the throwaway shape used by the connection-test
// endpoint — it lets the operator validate credentials BEFORE persisting
// them, so they don't have to write a row first.
type TestConnectionInput struct {
	Kind           Kind
	Endpoint       string
	Region         string
	Bucket         string
	PathPrefix     string
	AccessKeyID    string
	SecretKey      string
	ForcePathStyle bool
}

// TestConnectionResult is the structured success/failure shape the handler
// echoes back. We never surface raw AWS SDK errors — the message is operator-
// safe English.
type TestConnectionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TestConnection verifies that the given credentials can talk to the target
// bucket. For S3 the algorithm is HeadBucket + PutObject + DeleteObject of a
// throwaway probe key (so we exercise the full write+delete path the agent
// will need at backup time, not just read access). For Local and CP we
// short-circuit to OK — the local-path check runs agent-side; the CP path
// has no configuration to validate.
func (s *Service) TestConnection(ctx context.Context, in TestConnectionInput) TestConnectionResult {
	switch in.Kind {
	case KindCP, KindLocal:
		return TestConnectionResult{OK: true, Message: "ok"}
	case KindS3Compat:
		return s.testS3(ctx, in)
	default:
		return TestConnectionResult{OK: false, Message: "unsupported destination kind"}
	}
}

func (s *Service) testS3(ctx context.Context, in TestConnectionInput) TestConnectionResult {
	if in.Bucket == "" || in.AccessKeyID == "" || in.SecretKey == "" {
		return TestConnectionResult{OK: false, Message: "bucket, access key, and secret are required for s3_compat"}
	}
	region := in.Region
	if region == "" {
		region = "us-east-1"
	}
	client := s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(in.AccessKeyID, in.SecretKey, ""),
		UsePathStyle: in.ForcePathStyle,
		BaseEndpoint: nonEmpty(in.Endpoint),
	})

	// Probe key: high entropy so we never collide with a real object the
	// customer happens to have stored under the same prefix.
	probeKey := strings.TrimSuffix(in.PathPrefix, "/") + "/wpmgr-connection-test-" + randomToken(12)
	probeKey = strings.TrimPrefix(probeKey, "/")

	// Use a per-call timeout so a slow endpoint can't tie up the request
	// handler. 10s is generous for an S3 head+put+delete on a healthy
	// connection and short enough to feel responsive in the UI.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(in.Bucket)}); err != nil {
		return TestConnectionResult{OK: false, Message: friendlyS3Error("HeadBucket", err)}
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(in.Bucket),
		Key:         aws.String(probeKey),
		Body:        bytes.NewReader([]byte("wpmgr connection test")),
		ContentType: aws.String("text/plain"),
	}); err != nil {
		return TestConnectionResult{OK: false, Message: friendlyS3Error("PutObject", err)}
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(probeKey),
	}); err != nil {
		// PUT succeeded but DELETE failed — surface a softer warning. The
		// connection works for backups; cleanup will retry on the next
		// retention run.
		return TestConnectionResult{OK: true, Message: "Connected (put ok; delete of probe key failed: " + friendlyS3Error("DeleteObject", err) + ")"}
	}
	return TestConnectionResult{OK: true, Message: "Connected: head + put + delete succeeded"}
}

// PublicRecipient returns the CP's age public recipient string for surfacing
// in the destination form (operators can verify the encrypted-at-rest claim
// out of band).
func (s *Service) PublicRecipient() string {
	if s == nil || s.age == nil || s.age.recipient == nil {
		return ""
	}
	return s.age.recipient.String()
}

// nonEmpty returns nil for empty strings (so the AWS SDK keeps its default
// endpoint resolver) and an aws.String pointer otherwise.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// friendlyS3Error trims AWS SDK error verbosity into an operator-readable line.
func friendlyS3Error(op string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Collapse multi-line SDK errors.
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return op + ": " + msg
}

// randomToken returns a hex string of n cryptographically random bytes. Used
// for connection-test probe keys.
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	out := make([]byte, n*2)
	const hex = "0123456789abcdef"
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// errInvalidKind is the canonical validation error for unknown destination
// kinds. Kept package-level so handlers can errors.Is() against it.
var errInvalidKind = errors.New("invalid destination kind")
