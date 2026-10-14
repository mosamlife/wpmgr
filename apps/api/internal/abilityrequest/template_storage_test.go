package abilityrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const templateApprovalCard = `{"kind":"page_create","builder":{"builder":"elementor","format":"classic","version":"4.3.4","layout":"containers"},"template":{"type":"header","shows_on":["include/general","exclude/singular/page/42"],"rule_titles":[{"id":42,"title":"Precheck target"}],"also_live":[{"id":8,"title":"Precheck live","shows_on":null,"shows_on_problem":"unreadable"}]}}`

type templateOutcomeTx struct {
	pgx.Tx
	tenant, id       uuid.UUID
	updated, unknown bool
	event            sqlc.AuditLog
}

func (tx *templateOutcomeTx) Exec(_ context.Context, statement string, args ...any) (pgconn.CommandTag, error) {
	switch queryName(statement) {
	case "RecordAbilityRequestOutcome":
		tx.updated = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case "MarkAbilityRequestOutcomeUnknown":
		tx.unknown = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	if strings.Contains(statement, "pg_advisory_xact_lock") {
		return pgconn.NewCommandTag("SELECT 1"), nil
	}
	return pgconn.CommandTag{}, fmt.Errorf("unexpected exec %s", statement)
}

func (tx *templateOutcomeTx) QueryRow(_ context.Context, statement string, args ...any) pgx.Row {
	switch queryName(statement) {
	case "GetLastAuditHash":
		return columnsRow{cols: []any{""}}
	case "InsertAuditEntry":
		tx.event = sqlc.AuditLog{ID: uuid.New(), TenantID: args[0].(uuid.UUID), ActorType: args[1].(string), ActorID: args[2].(string), Action: args[3].(string), TargetType: args[4].(string), TargetID: args[5].(string), Metadata: args[6].([]byte), PrevHash: args[7].(string), Hash: args[8].(string), CreatedAt: args[9].(time.Time)}
		return columnsRow{cols: fieldValues(tx.event)}
	}

	return columnsRow{err: fmt.Errorf("unexpected query %s", statement)}
}

func TestTemplateReviewOutcomeWritesOnlyPageFacts(t *testing.T) {
	now := time.Now()
	tenant, id := uuid.New(), uuid.New()
	svc := &Service{logger: slog.Default(), audit: audit.NewRecorder(nil, domain.FixedClock{T: now})}
	template, ok := outcomeFromStored(mcp.AbilityPageCreate, []byte(templateOutcome), now)
	if !ok {
		t.Fatal("outcome refused")
	}
	var original json.RawMessage
	for _, oc := range []writeOutcome{createdOutcome(9, now), template} {
		tx := &templateOutcomeTx{tenant: tenant, id: id}
		if err := svc.writeOutcomeTx(context.Background(), tx, sqlc.New(tx), tenant, id, oc); err != nil {
			t.Fatal(err)
		}
		if !tx.updated || tx.unknown {
			t.Fatal("outcome was not recorded")
		}
		if original == nil {
			original = tx.event.Metadata
		} else if string(original) != string(tx.event.Metadata) {
			t.Fatalf("template outcome writes extra metadata: %s", tx.event.Metadata)
		}
		var md map[string]json.RawMessage
		if json.Unmarshal(tx.event.Metadata, &md) != nil || len(md) != 3 || md["template_created"] != nil {
			t.Fatalf("unexpected outcome metadata: %s", tx.event.Metadata)
		}
	}
}

func TestTemplateReviewOutcomeMismatchUsesUnknownPath(t *testing.T) {
	now := time.Now()
	tenant, id := uuid.New(), uuid.New()
	svc := &Service{logger: slog.Default(), audit: audit.NewRecorder(nil, domain.FixedClock{T: now})}
	oc, ok := outcomeFromStored(mcp.AbilityPageCreate, []byte(templateOutcome), now)
	if !ok {
		t.Fatal("outcome refused")
	}
	row := sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageCreate, CardFacts: []byte(strings.Replace(templateApprovalCard, "include/general", "include/archive", 1))}
	oc = outcomeMatchingApproval(row, oc)
	tx := &templateOutcomeTx{tenant: tenant, id: id}
	if err := svc.writeOutcomeTx(context.Background(), tx, sqlc.New(tx), tenant, id, oc); err != nil {
		t.Fatal(err)
	}
	if tx.updated || !tx.unknown || tx.event.Action != audit.ActionAbilityRequestFailed || strings.Contains(string(tx.event.Metadata), "title") {
		t.Fatalf("mismatch did not take the ordinary unknown path: %s", tx.event.Metadata)
	}
}
