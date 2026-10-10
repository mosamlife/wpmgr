package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// sendableTx answers policySendableSQL with fixed values. Any other use of
// the transaction calls the nil embedded pgx.Tx and panics, which fails the
// test: policyNotSendable reads one row and nothing else.
type sendableTx struct {
	pgx.Tx
	queries int
	sql     string
	args    []any
	// setting, grant and class are the three values the statement returns:
	// the site's mode still allows the approval, the connection still runs
	// by the site's setting, and the stored class is still the one it was
	// approved under.
	setting, grant, class bool
	err                   error
}

func (f *sendableTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queries++
	f.sql, f.args = sql, args
	return sendableRow{f}
}

type sendableRow struct{ f *sendableTx }

func (r sendableRow) Scan(dest ...any) error {
	if r.f.err != nil {
		return r.f.err
	}
	vals := []bool{r.f.setting, r.f.grant, r.f.class}
	if len(dest) != len(vals) {
		return fmt.Errorf("scan into %d values, want %d", len(dest), len(vals))
	}
	for i, d := range dest {
		p, ok := d.(*bool)
		if !ok {
			return fmt.Errorf("scan target %d is %T, want *bool", i, d)
		}
		*p = vals[i]
	}
	return nil
}

var (
	sTenant = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")
	sSite   = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	sGrant  = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000003")
	sEntry  = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000004")
)

// approvedBySetting is a REST write the site's setting approved as an AI
// draft edit under mode version 3.
func approvedBySetting() sqlc.AssistantAbilityRequest {
	version := int64(3)
	class, base, route := "ai_draft", "by_target_status", "wp-v2-pages-update-fields"
	return sqlc.AssistantAbilityRequest{
		TenantID: sTenant, SiteID: sSite, ProposedByGrantID: sGrant, EntryID: sEntry,
		State: "approved", ApprovalSource: "policy", ApprovalModeVersion: &version,
		ChangeClass: &class, BaseChangeClass: &base, RouteID: &route,
	}
}

// TestPolicyNotSendable proves the re-check at the reservation: a person's
// approval is sent as approved; an approval by a setting is sent only while
// the site's mode still allows it at the version it was approved under, the
// connection still runs by the site's setting, and the stored class is
// unchanged; anything this build does not recognise is not sent.
//
// Mutation: drop the connection's term from the setting_changed case
// ("case !settingOK:"); "connection set to never" is then sent.
func TestPolicyNotSendable(t *testing.T) {
	scanErr := errors.New("connection reset")
	for _, tc := range []struct {
		name                  string
		row                   func(r *sqlc.AssistantAbilityRequest)
		setting, grant, class bool
		err                   error
		want                  string
		wantErr               bool
		wantQuery             bool
	}{
		{name: "approved by a person", row: func(r *sqlc.AssistantAbilityRequest) { r.ApprovalSource = "person" }, want: ""},
		{name: "approved by a session in a build without sessions", row: func(r *sqlc.AssistantAbilityRequest) { r.ApprovalSource = "session" }, want: ReasonSettingChanged},
		{name: "no approval source", row: func(r *sqlc.AssistantAbilityRequest) { r.ApprovalSource = "" }, want: ReasonSettingChanged},
		{name: "no mode version recorded", row: func(r *sqlc.AssistantAbilityRequest) { r.ApprovalModeVersion = nil }, want: ReasonSettingChanged},
		{name: "no class recorded", row: func(r *sqlc.AssistantAbilityRequest) { r.ChangeClass = nil }, want: ReasonSettingChanged},
		{name: "no stored class recorded", row: func(r *sqlc.AssistantAbilityRequest) { r.BaseChangeClass = nil }, want: ReasonSettingChanged},
		{name: "everything still holds", setting: true, grant: true, class: true, want: "", wantQuery: true},
		{name: "site mode lowered or moved", setting: false, grant: true, class: true, want: ReasonSettingChanged, wantQuery: true},
		{name: "connection set to never", setting: true, grant: false, class: true, want: ReasonSettingChanged, wantQuery: true},
		{name: "entry re-classed", setting: true, grant: true, class: false, want: ReasonClassChanged, wantQuery: true},
		{name: "mode moved and entry re-classed", setting: false, grant: true, class: false, want: ReasonSettingChanged, wantQuery: true},
		{name: "re-check could not be read", setting: true, grant: true, class: true, err: scanErr, wantErr: true, wantQuery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := approvedBySetting()
			if tc.row != nil {
				tc.row(&r)
			}
			tx := &sendableTx{setting: tc.setting, grant: tc.grant, class: tc.class, err: tc.err}
			got, err := policyNotSendable(context.Background(), tx, r)
			if tc.wantErr {
				if !errors.Is(err, scanErr) || got != "" {
					t.Fatalf("got (%q, %v), want the read's error and no reason", got, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
			if tc.wantQuery != (tx.queries == 1) || tx.queries > 1 {
				t.Fatalf("re-check read %d times, want the database read: %v", tx.queries, tc.wantQuery)
			}
			if !tc.wantQuery {
				return
			}
			if tx.sql != policySendableSQL {
				t.Fatalf("ran another statement than policySendableSQL")
			}
			// The statement's parameters, in order: the request's own
			// tenant, site, the mode version and class it was approved
			// under, its connection, its route or entry, and the stored
			// class it was approved under.
			want := []any{sTenant, sSite, int64(3), "ai_draft", sGrant, r.RouteID, sEntry, "by_target_status"}
			if !reflect.DeepEqual(tx.args, want) {
				t.Fatalf("parameters %v, want %v", tx.args, want)
			}
		})
	}
}
