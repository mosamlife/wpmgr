package site

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeRepo is an in-memory Repo for unit-testing the service without a DB.
type fakeRepo struct {
	createErr error
	created   CreateInput
	getErr    error
	listErr   error
	deleteErr error
}

func (f *fakeRepo) Create(_ context.Context, in CreateInput) (Site, error) {
	f.created = in
	if f.createErr != nil {
		return Site{}, f.createErr
	}
	return Site{ID: uuid.New(), TenantID: in.TenantID, URL: in.URL, Name: in.Name, Status: orDefault(in.Status)}, nil
}

func (f *fakeRepo) Get(_ context.Context, tenantID, id uuid.UUID) (Site, error) {
	if f.getErr != nil {
		return Site{}, f.getErr
	}
	return Site{ID: id, TenantID: tenantID}, nil
}

func (f *fakeRepo) List(_ context.Context, in ListInput) ([]Site, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []Site{{TenantID: in.TenantID}}, nil
}

func (f *fakeRepo) Delete(_ context.Context, _, _ uuid.UUID) error {
	return f.deleteErr
}

// M2 methods — unit tests below exercise Create/Get/List/Delete only; the
// enrollment/agent/health paths are covered by the integration tests.
func (f *fakeRepo) SetTags(_ context.Context, in SetTagsInput) (Site, error) {
	return Site{ID: in.SiteID, TenantID: in.TenantID, Tags: in.Tags}, nil
}

func (f *fakeRepo) SetAgeRecipient(_ context.Context, tenantID, siteID uuid.UUID, recipient string) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID, AgeRecipient: recipient}, nil
}

func (f *fakeRepo) CreatePairingCode(_ context.Context, in CreatePairingCodeInput, codeHash string, expiresAt time.Time) (PairingCode, error) {
	return PairingCode{ID: uuid.New(), TenantID: in.TenantID, ExpiresAt: expiresAt}, nil
}

func (f *fakeRepo) Enroll(_ context.Context, _ string, in EnrollInput) (Site, error) {
	return Site{ID: uuid.New(), URL: in.URL, AgentPublicKey: in.AgentPublicKey}, nil
}

func (f *fakeRepo) GetByAgentKey(_ context.Context, key string) (Site, error) {
	return Site{ID: uuid.New(), AgentPublicKey: key}, nil
}

func (f *fakeRepo) UpdateMetadata(_ context.Context, tenantID, siteID uuid.UUID, _ Metadata, _ []byte) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID}, nil
}

func (f *fakeRepo) TouchSeen(_ context.Context, _, _ uuid.UUID) error { return nil }

func (f *fakeRepo) RecordNonce(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return true, nil
}

func (f *fakeRepo) ListEnrolled(_ context.Context) ([]EnrolledSite, error) { return nil, nil }

func (f *fakeRepo) MarkUnreachable(_ context.Context, _ uuid.UUID) (bool, error) { return true, nil }

func (f *fakeRepo) PruneNonces(_ context.Context, _ time.Time) (int64, error) { return 0, nil }

// ---- M21 connection-lifecycle Repo methods (in-memory no-ops) ----

func (f *fakeRepo) CreatePending(_ context.Context, tenantID uuid.UUID, url, name string, tags []string) (Site, error) {
	return Site{ID: uuid.New(), TenantID: tenantID, URL: url, Name: name, Tags: tags, ConnectionState: StatePendingEnrollment}, nil
}

func (f *fakeRepo) MintSiteBoundCode(_ context.Context, in CreatePairingCodeInput, siteID uuid.UUID, codeHash string, expiresAt time.Time) (PairingCode, error) {
	return PairingCode{ID: uuid.New(), TenantID: in.TenantID, ExpiresAt: expiresAt}, nil
}

func (f *fakeRepo) Transition(_ context.Context, in TransitionInput) (TransitionResult, error) {
	return TransitionResult{
		Site: Site{ID: in.SiteID, TenantID: in.TenantID, ConnectionState: in.To},
		From: StateConnected,
	}, nil
}

func (f *fakeRepo) ConsumeSiteBoundCode(_ context.Context, codeHash, consumedFromIP string, in EnrollInput) (ConsumeResult, error) {
	return ConsumeResult{
		Site:      Site{ID: uuid.New(), AgentPublicKey: in.AgentPublicKey, ConnectionState: StateConnected},
		SiteBound: true,
	}, nil
}

func (f *fakeRepo) Heartbeat(_ context.Context, tenantID, siteID uuid.UUID) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID, ConnectionState: StateConnected}, nil
}

func (f *fakeRepo) ListToDegrade(_ context.Context, _ time.Time) ([]SiteRef, error) { return nil, nil }

func (f *fakeRepo) ListToDisconnect(_ context.Context, _ time.Time) ([]SiteRef, error) {
	return nil, nil
}

func (f *fakeRepo) ResolveTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (f *fakeRepo) PairingCodeSiteID(_ context.Context, _ string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func orDefault(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

func newSvc(repo Repo) *Service {
	return NewService(repo, domain.NewValidator(), domain.FixedClock{T: time.Unix(0, 0)})
}

func TestServiceCreate(t *testing.T) {
	tenant := uuid.New()
	tests := []struct {
		name     string
		in       CreateInput
		repoErr  error
		wantKind domain.Kind
		wantErr  bool
	}{
		{
			name: "valid",
			in:   CreateInput{TenantID: tenant, URL: "https://example.com", Name: "Example"},
		},
		{
			name:     "missing tenant",
			in:       CreateInput{URL: "https://example.com", Name: "Example"},
			wantErr:  true,
			wantKind: domain.KindForbidden,
		},
		{
			name:     "missing url",
			in:       CreateInput{TenantID: tenant, Name: "Example"},
			wantErr:  true,
			wantKind: domain.KindValidation,
		},
		{
			name:     "invalid url",
			in:       CreateInput{TenantID: tenant, URL: "not a url", Name: "Example"},
			wantErr:  true,
			wantKind: domain.KindValidation,
		},
		{
			name:     "invalid status",
			in:       CreateInput{TenantID: tenant, URL: "https://example.com", Name: "Example", Status: "bogus"},
			wantErr:  true,
			wantKind: domain.KindValidation,
		},
		{
			name:     "repo conflict propagates",
			in:       CreateInput{TenantID: tenant, URL: "https://example.com", Name: "Example"},
			repoErr:  domain.Conflict("site_url_exists", "dup"),
			wantErr:  true,
			wantKind: domain.KindConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{createErr: tt.repoErr}
			_, err := newSvc(repo).Create(context.Background(), tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				de, ok := domain.AsDomain(err)
				if !ok {
					t.Fatalf("expected domain error, got %T", err)
				}
				if de.Kind != tt.wantKind {
					t.Fatalf("kind = %v, want %v", de.Kind, tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo.created.Status != "" && repo.created.Status != tt.in.Status {
				t.Fatalf("status mutated unexpectedly")
			}
		})
	}
}

func TestServiceGetRequiresTenant(t *testing.T) {
	_, err := newSvc(&fakeRepo{}).Get(context.Background(), uuid.Nil, uuid.New())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("want forbidden, got %v", err)
	}
}

func TestServiceListNormalizesPaging(t *testing.T) {
	repo := &fakeRepo{}
	svc := newSvc(repo)
	got, err := svc.List(context.Background(), ListInput{TenantID: uuid.New(), Limit: 0, Offset: -5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 site, got %d", len(got))
	}
}

func TestServiceDeletePropagatesNotFound(t *testing.T) {
	repo := &fakeRepo{deleteErr: domain.NotFound("site_not_found", "nope")}
	err := newSvc(repo).Delete(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, repo.deleteErr) {
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound {
			t.Fatalf("want not found, got %v", err)
		}
	}
}
