// Package settings serves the instance-level SMTP configuration UI (ADR-045
// Phase 1): GET (masked), PUT (age-encrypts the password on write), and a
// send-test that reuses the mailer's SSRF-guarded transport. The single
// smtp_settings row is instance-global, so reads/writes run under app.agent='on'
// (Pool.InAgentTx); the real access control is the instance-authority HTTP gate
// on every route (see Handler).
package settings

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"log/slog"

	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	mailerpkg "github.com/mosamlife/wpmgr/apps/api/internal/mailer"
)

// SMTPSettings is the masked, API-safe view of the SMTP config. The password is
// NEVER included; password_set tells the UI whether one is stored.
type SMTPSettings struct {
	Enabled          bool      `json:"enabled"`
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	Username         string    `json:"username"`
	FromAddress      string    `json:"from_address"`
	FromName         string    `json:"from_name"`
	TLSMode          string    `json:"tls_mode"`
	AllowInsecureTLS bool      `json:"allow_insecure_tls"`
	PasswordSet      bool      `json:"password_set"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SMTPUpdate is the PUT body. Password is write-only: nil/empty leaves the
// stored ciphertext unchanged (nil-sentinel).
type SMTPUpdate struct {
	Enabled          bool    `json:"enabled"`
	Host             string  `json:"host"`
	Port             int     `json:"port"`
	Username         string  `json:"username"`
	FromAddress      string  `json:"from_address"`
	FromName         string  `json:"from_name"`
	TLSMode          string  `json:"tls_mode"`
	AllowInsecureTLS bool    `json:"allow_insecure_tls"`
	Password         *string `json:"password"`
}

// Repo is the smtp_settings data access layer (instance-global, app.agent path).
type Repo struct{ pool *db.Pool }

// NewRepo builds a Repo.
func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

// Get returns the singleton row (ok=false when none exists yet).
func (r *Repo) Get(ctx context.Context) (sqlc.SmtpSetting, bool, error) {
	var row sqlc.SmtpSetting
	var ok bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		s, err := sqlc.New(tx).GetSMTPSettings(ctx)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		row, ok = s, true
		return nil
	})
	return row, ok, err
}

// Upsert writes the singleton row.
func (r *Repo) Upsert(ctx context.Context, p sqlc.UpsertSMTPSettingsParams) (sqlc.SmtpSetting, error) {
	var row sqlc.SmtpSetting
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		s, err := sqlc.New(tx).UpsertSMTPSettings(ctx, p)
		if err != nil {
			return err
		}
		row = s
		return nil
	})
	return row, err
}

// Service holds the SMTP config business logic.
type Service struct {
	repo   *Repo
	age    *cryptbox.AgeIdentity
	mailer *mailerpkg.Service
	log    *slog.Logger
}

// NewService builds the Service.
func NewService(repo *Repo, age *cryptbox.AgeIdentity, m *mailerpkg.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, age: age, mailer: m, log: log}
}

// Get returns the masked settings, falling back to sane defaults when unset so
// the form always renders.
func (s *Service) Get(ctx context.Context) (SMTPSettings, error) {
	row, ok, err := s.repo.Get(ctx)
	if err != nil {
		return SMTPSettings{}, domain.Internal("smtp_read", "could not read SMTP settings")
	}
	if !ok {
		return SMTPSettings{Port: 587, TLSMode: "starttls"}, nil
	}
	return toDTO(row), nil
}

// Update validates + persists the settings, age-encrypting a newly supplied
// password and preserving the stored ciphertext otherwise.
func (s *Service) Update(ctx context.Context, in SMTPUpdate, updatedBy uuid.UUID) (SMTPSettings, error) {
	if err := validate(in); err != nil {
		return SMTPSettings{}, err
	}
	params := sqlc.UpsertSMTPSettingsParams{
		Enabled:          in.Enabled,
		Host:             strings.TrimSpace(in.Host),
		Port:             int32(in.Port),
		Username:         strings.TrimSpace(in.Username),
		FromAddress:      strings.TrimSpace(in.FromAddress),
		FromName:         strings.TrimSpace(in.FromName),
		TlsMode:          in.TLSMode,
		AllowInsecureTls: in.AllowInsecureTLS,
		UpdatedBy:        pgUUID(updatedBy),
		SetPassword:      false,
	}
	if in.Password != nil && *in.Password != "" {
		enc, err := s.age.Encrypt([]byte(*in.Password))
		if err != nil {
			return SMTPSettings{}, domain.Internal("smtp_encrypt", "could not encrypt the SMTP password")
		}
		params.SetPassword = true
		params.PasswordEnc = enc
	}
	row, err := s.repo.Upsert(ctx, params)
	if err != nil {
		return SMTPSettings{}, domain.Internal("smtp_write", "could not save SMTP settings")
	}
	return toDTO(row), nil
}

// SendTest sends the branded test email through the STORED, enabled config. It
// returns an operator-safe (scrubbed) error suitable for display.
func (s *Service) SendTest(ctx context.Context, to string) error {
	if _, err := mail.ParseAddress(to); err != nil {
		return errors.New("enter a valid recipient email address")
	}
	row, ok, err := s.repo.Get(ctx)
	if err != nil {
		return errors.New("could not read the stored SMTP settings")
	}
	if !ok || !row.Enabled || strings.TrimSpace(row.Host) == "" {
		return errors.New("save an enabled SMTP configuration before sending a test email")
	}
	t := mailerpkg.Transport{
		Host:             row.Host,
		Port:             int(row.Port),
		Username:         row.Username,
		From:             row.FromAddress,
		FromName:         row.FromName,
		TLSMode:          row.TlsMode,
		AllowInsecureTLS: row.AllowInsecureTls,
	}
	if len(row.PasswordEnc) > 0 {
		plain, derr := s.age.Decrypt(row.PasswordEnc)
		if derr != nil {
			return errors.New("could not read the stored SMTP password; re-enter and save it")
		}
		t.Password = string(plain)
	}
	return s.mailer.SendTest(ctx, t, to)
}

// ---- helpers ---------------------------------------------------------------

func toDTO(row sqlc.SmtpSetting) SMTPSettings {
	return SMTPSettings{
		Enabled:          row.Enabled,
		Host:             row.Host,
		Port:             int(row.Port),
		Username:         row.Username,
		FromAddress:      row.FromAddress,
		FromName:         row.FromName,
		TLSMode:          row.TlsMode,
		AllowInsecureTLS: row.AllowInsecureTls,
		PasswordSet:      len(row.PasswordEnc) > 0,
		UpdatedAt:        row.UpdatedAt,
	}
}

func validate(in SMTPUpdate) error {
	switch in.TLSMode {
	case "starttls", "tls", "none":
	default:
		return domain.Validation("invalid_tls_mode", "tls_mode must be one of starttls, tls, none")
	}
	if in.Port < 1 || in.Port > 65535 {
		return domain.Validation("invalid_port", "port must be between 1 and 65535")
	}
	// Only enforce the required fields when the operator is enabling delivery, so
	// a half-filled draft can be saved disabled.
	if in.Enabled {
		if strings.TrimSpace(in.Host) == "" {
			return domain.Validation("host_required", "host is required when SMTP is enabled")
		}
		if strings.TrimSpace(in.FromAddress) == "" {
			return domain.Validation("from_required", "a From address is required when SMTP is enabled")
		}
	}
	if a := strings.TrimSpace(in.FromAddress); a != "" {
		if _, err := mail.ParseAddress(a); err != nil {
			return domain.Validation("invalid_from", "from_address is not a valid email address")
		}
	}
	return nil
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}
