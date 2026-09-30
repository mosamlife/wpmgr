package abilities

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// AgentClient is the one agent call the service makes.
type AgentClient interface {
	AbilityRun(ctx context.Context, siteID uuid.UUID, siteURL string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error)
}

// Service runs inventory refreshes.
type Service struct {
	repo   Repo
	agent  AgentClient
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds the service. agent may be nil on an install without a
// signing key; refreshes then refuse.
func NewService(repo Repo, agent AgentClient, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, agent: agent, logger: logger, now: time.Now}
}

var agentVersionShape = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

// AgentMeetsFloor reports whether v is at or above
// agentcmd.MinAgentVersionForAbilityEngine. An empty or malformed version
// does not.
func AgentMeetsFloor(v string) bool {
	v = strings.TrimSpace(v)
	if !agentVersionShape.MatchString(v) {
		return false
	}
	return wpversion.Compare(v, agentcmd.MinAgentVersionForAbilityEngine) >= 0
}

// callTimeout bounds one agent call.
const callTimeout = 20 * time.Second

// RefreshResult summarises one refresh.
type RefreshResult struct {
	Stored       int
	SkippedNames int
	Truncated    bool
}

// Refresh asks the site for its abilities through wpmgr/abilities-inventory
// and replaces its cached inventory.
func (s *Service) Refresh(ctx context.Context, tenantID, siteID uuid.UUID, scheduled bool) (RefreshResult, error) {
	if s.agent == nil {
		return RefreshResult{}, domain.ServiceUnavailable("ability_engine_unavailable", "the ability engine is not available on this install")
	}
	target, err := s.repo.GetSiteTarget(ctx, tenantID, siteID)
	if err != nil {
		return RefreshResult{}, err
	}
	if !target.Enrolled || (target.ConnectionState != "connected" && target.ConnectionState != "degraded") {
		return RefreshResult{}, fmt.Errorf("%w: site is not connected", ErrNotSendable)
	}
	if scheduled && target.Paused {
		return RefreshResult{}, fmt.Errorf("%w: monitoring is paused", ErrNotSendable)
	}
	if !AgentMeetsFloor(target.AgentVersion) {
		return RefreshResult{}, fmt.Errorf("%w: agent below %s", ErrNotSendable, agentcmd.MinAgentVersionForAbilityEngine)
	}
	row, err := s.repo.CatalogueEntryByName(ctx, tenantID, NameInventory)
	if err != nil {
		return RefreshResult{}, err
	}
	entry, sum, err := SendableEntry(row)
	if err != nil {
		return RefreshResult{}, err
	}

	checkedAt := s.now().UTC()
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	resp, err := s.agent.AbilityRun(callCtx, siteID, target.URL, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeRead, RequestID: uuid.New(),
		Entry: entry, EntrySHA256: sum, Input: []byte(`{}`),
	})
	cancel()
	if err != nil {
		return RefreshResult{}, err
	}
	if resp.Ability != NameInventory {
		return RefreshResult{}, fmt.Errorf("%w: reply names a different ability", ErrInvalidInventory)
	}
	res, err := ValidateInventory(resp.Output)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := s.repo.ReplaceInventory(ctx, tenantID, siteID, checkedAt, uuid.New(), res); err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{Stored: len(res.Rows), SkippedNames: res.SkippedNames, Truncated: res.Truncated}, nil
}
