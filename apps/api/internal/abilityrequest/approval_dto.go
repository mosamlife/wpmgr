package abilityrequest

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// How a request was approved, and how WPMgr classed it, on the wire
// (ADR-065). Every value comes from the request row as recorded when it was
// decided, never re-read from the site's current setting, so a card stays
// true after the setting changes.

// ApprovalDTO is how an approved request was approved: by a person from the
// dashboard, or by the site's setting with no person deciding it.
type ApprovalDTO struct {
	Source string `json:"source"`
	// Setting is the setting an approval by a setting relied on. Null for a
	// person's approval.
	Setting *ApprovalSettingDTO `json:"setting"`
}

// ApprovalSettingDTO is the site setting an approval by a setting relied on,
// copied onto the request when it was approved. SetByName is a person's
// name: render it as text.
type ApprovalSettingDTO struct {
	Mode                string     `json:"mode"`
	Source              string     `json:"source"`
	SetByUserID         *uuid.UUID `json:"set_by_user_id"`
	SetByName           *string    `json:"set_by_name"`
	SetByAccountDeleted bool       `json:"set_by_account_deleted"`
	SetAt               *time.Time `json:"set_at"`
}

// approvedStates is the approved family: a request in one of them was
// approved, by a person or by a setting.
var approvedStates = map[string]struct{}{
	"approved": {}, "dispatched": {}, "done": {}, "failed": {}, "not_sent": {}, "outcome_unknown": {},
}

// The approval sources the contract names.
const (
	approvalSourcePerson = "person"
	approvalSourcePolicy = "policy"
)

// setterNames is the display names of the people recorded as setters on a
// page of requests. read is false when the names could not be read; a card
// then names nobody and claims no account was deleted.
type setterNames struct {
	byID map[uuid.UUID]string
	read bool
}

// approvalOf is the request's approval on the wire, or nil while it has not
// been approved and for a request that never will be (declined, withdrawn,
// expired). An approval by a setting whose recorded setting is incomplete
// is reported with no setting rather than a guessed one.
func approvalOf(r sqlc.AssistantAbilityRequest, names setterNames) *ApprovalDTO {
	if _, ok := approvedStates[r.State]; !ok {
		return nil
	}
	switch r.ApprovalSource {
	case approvalSourcePerson:
		return &ApprovalDTO{Source: approvalSourcePerson}
	case approvalSourcePolicy:
		return &ApprovalDTO{Source: approvalSourcePolicy, Setting: approvalSettingOf(r, names)}
	}
	return nil
}

func approvalSettingOf(r sqlc.AssistantAbilityRequest, names setterNames) *ApprovalSettingDTO {
	if r.ApprovalSiteMode == nil || r.ApprovalModeSource == nil {
		return nil
	}
	mode := aipolicy.Mode(*r.ApprovalSiteMode)
	if mode != aipolicy.ModeAIDrafts && mode != aipolicy.ModeFull {
		return nil
	}
	switch *r.ApprovalModeSource {
	case "launch_default", "enable_default", "person":
	default:
		return nil
	}
	out := &ApprovalSettingDTO{Mode: string(mode), Source: *r.ApprovalModeSource, SetAt: ts(r.ApprovalSetterSetAt)}
	if r.ApprovalSetterUserID.Valid {
		id := uuid.UUID(r.ApprovalSetterUserID.Bytes)
		out.SetByUserID = &id
		if names.read {
			if name, ok := names.byID[id]; ok {
				out.SetByName = &name
			} else {
				out.SetByAccountDeleted = true
			}
		}
	}
	return out
}

// changeClassOf is the effective class WPMgr decided, or nil before it
// decided and for a request made before classes existed.
func changeClassOf(r sqlc.AssistantAbilityRequest) *string {
	if r.ChangeClass == nil || !aipolicy.Class(*r.ChangeClass).Effective() {
		return nil
	}
	c := *r.ChangeClass
	return &c
}

// changeKindNameOf is WPMgr's name for the class as copy uses it, or nil
// when there is no class or the class is always_ask.
func changeKindNameOf(r sqlc.AssistantAbilityRequest) *string {
	c := changeClassOf(r)
	if c == nil {
		return nil
	}
	name := aipolicy.Class(*c).KindName()
	if name == "" {
		return nil
	}
	return &name
}

// askReasonOf is why the request was left for a person, when the reason is
// one the contract names; nil otherwise.
func askReasonOf(r sqlc.AssistantAbilityRequest) *string {
	if r.AskReason == nil {
		return nil
	}
	switch reason := aipolicy.AskReason(*r.AskReason); reason {
	case aipolicy.AskKindAlwaysAsks, aipolicy.AskUnknownTargetState, aipolicy.AskSiteModeAsk,
		aipolicy.AskKindNotInMode, aipolicy.AskSetterLacksPermission, aipolicy.AskOverChangeBudget,
		aipolicy.AskOverSiteCap, aipolicy.AskConnectionNeverAuto, aipolicy.AskConnectionSetterInvalid,
		aipolicy.AskNotChecked:
		s := string(reason)
		return &s
	}
	return nil
}

// readSetterNames reads the display names of the setters recorded on rows
// the caller has already read, in the caller's own tenant transaction. read
// is false when the read failed; the cards then name no one.
func (s *Service) readSetterNames(ctx context.Context, p domain.Principal, rows []sqlc.AssistantAbilityRequest) setterNames {
	out := setterNames{byID: map[uuid.UUID]string{}, read: true}
	seen := map[uuid.UUID]struct{}{}
	ids := make([]uuid.UUID, 0)
	for _, r := range rows {
		if r.ApprovalSource != approvalSourcePolicy || !r.ApprovalSetterUserID.Valid {
			continue
		}
		id := uuid.UUID(r.ApprovalSetterUserID.Bytes)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out
	}
	err := s.runAsCaller(ctx, p, func(q *sqlc.Queries, _ pgx.Tx) error {
		found, err := q.ListUserNamesByIDs(ctx, ids)
		if err != nil {
			return err
		}
		for _, u := range found {
			out.byID[u.ID] = u.Name
		}
		return nil
	})
	if err != nil {
		s.logger.WarnContext(ctx, "ability request: setter names not read; cards name no one",
			slog.Any("error", err))
		return setterNames{byID: map[uuid.UUID]string{}}
	}
	return out
}
