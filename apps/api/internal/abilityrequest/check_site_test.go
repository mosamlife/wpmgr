package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// fieldValues is a sqlc row struct's fields in declaration order, which is
// the order its generated query scans them.
func fieldValues(row any) []any {
	v := reflect.ValueOf(row)
	out := make([]any, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		out = append(out, v.Field(i).Interface())
	}
	return out
}

// scanColumns assigns cols to a generated Scan's destinations, refusing a
// count or type that does not line up, as a real row would.
func scanColumns(cols []any, dest []any) error {
	if len(dest) != len(cols) {
		return fmt.Errorf("scan: %d destinations for %d columns", len(dest), len(cols))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer || dv.Elem().Type() != reflect.TypeOf(cols[i]) {
			return fmt.Errorf("scan: column %d is %T, destination %T", i, cols[i], d)
		}
		dv.Elem().Set(reflect.ValueOf(cols[i]))
	}
	return nil
}

type columnsRow struct {
	cols []any
	err  error
}

func (r columnsRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanColumns(r.cols, dest)
}

type columnsRows struct {
	rows [][]any
	at   int
}

func (r *columnsRows) Close()                                       {}
func (r *columnsRows) Err() error                                   { return nil }
func (r *columnsRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT") }
func (r *columnsRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *columnsRows) Next() bool                                   { r.at++; return r.at <= len(r.rows) }
func (r *columnsRows) Scan(dest ...any) error                       { return scanColumns(r.rows[r.at-1], dest) }
func (r *columnsRows) Values() ([]any, error)                       { return r.rows[r.at-1], nil }
func (r *columnsRows) RawValues() [][]byte                          { return nil }
func (r *columnsRows) Conn() *pgx.Conn                              { return nil }

// dispatchDB answers the statements checkSite sends, by sqlc query name,
// and records each one with its arguments. A write affects no row, so the
// close never reaches the audit recorder (which this Service does not have).
type dispatchDB struct {
	row   sqlc.GetApprovedAbilityRequestForDispatchRow
	site  sqlc.Site
	entry sqlc.AbilityCatalogue
	sent  []sentStatement
}

type sentStatement struct {
	name string
	args []any
}

func queryName(sql string) string {
	f := strings.Fields(strings.SplitN(sql, "\n", 2)[0])
	if len(f) >= 3 && f[0] == "--" && f[1] == "name:" {
		return f[2]
	}
	return sql
}

func (d *dispatchDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.sent = append(d.sent, sentStatement{queryName(sql), args})
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func (d *dispatchDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	name := queryName(sql)
	d.sent = append(d.sent, sentStatement{name, args})
	if name != "ListSitesForMCPScope" {
		return nil, fmt.Errorf("unexpected query %s", name)
	}
	return &columnsRows{rows: [][]any{fieldValues(d.site)}}, nil
}

func (d *dispatchDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	name := queryName(sql)
	d.sent = append(d.sent, sentStatement{name, args})
	switch name {
	case "GetApprovedAbilityRequestForDispatch":
		r := d.row
		return columnsRow{cols: append(fieldValues(r.AssistantAbilityRequest),
			r.PastDeadline, r.EntryHashCurrent, r.EntryEnabled, r.RouteHashCurrent, r.RouteEnabled)}
	case "GetAbilityCatalogueEntry":
		return columnsRow{cols: fieldValues(d.entry)}
	}
	return columnsRow{err: fmt.Errorf("unexpected query %s", name)}
}

func (d *dispatchDB) names() []string {
	out := make([]string, 0, len(d.sent))
	for _, s := range d.sent {
		out = append(out, s.name)
	}
	return out
}

func (d *dispatchDB) find(name string) (sentStatement, bool) {
	for _, s := range d.sent {
		if s.name == name {
			return s, true
		}
	}
	return sentStatement{}, false
}

type allowAllRules struct{}

func (allowAllRules) ForbiddenByContext(context.Context, uuid.UUID, uuid.UUID, string) (string, bool, error) {
	return "", false, nil
}

type unusedAgent struct{}

func (unusedAgent) AbilityRun(context.Context, uuid.UUID, string, agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	return agentcmd.AbilityRunResponse{}, errors.New("checkSite never sends")
}

func nameOnlyEntry(e sqlc.AbilityCatalogue) ([]byte, string, error) {
	b := []byte(`{"name":"` + e.Name + `"}`)
	return b, agentcmd.SHA256Hex(b), nil
}

// TestCheckSite_AgentFloorClosesNotSent drives checkSite, the dispatch step
// before any reservation, over a stand-in transaction: an approved layout
// request whose site's plugin is below the layout floor closes
// not_sent/agent_outdated there, and the catalogue entry is never read. The
// same request on a plugin at the floor, and a text-only request on the
// older plugin, go on to the reservation with the site's address and the
// entry's bytes.
func TestCheckSite_AgentFloorClosesNotSent(t *testing.T) {
	below := "0.61.158"
	cases := []struct {
		name    string
		input   string
		version string
		closes  bool
	}{
		{"layout page below the layout floor", layoutInput, below, true},
		{"layout page on no reported version", layoutInput, "", true},
		{"layout page at the layout floor", layoutInput, agentcmd.MinAgentVersionForPageLayout, false},
		{"text-only page below the layout floor", textOnlyInput, below, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := DispatchArgs{TenantID: uuid.New(), RequestID: uuid.New(), SiteID: uuid.New(), GrantID: uuid.New()}
			entry := sqlc.AbilityCatalogue{EntryID: uuid.New(), Name: mcp.AbilityPageCreate, Source: "wpmgr", Class: "write", Enabled: true}
			_, sum, _ := nameOnlyEntry(entry)
			db := &dispatchDB{
				row: sqlc.GetApprovedAbilityRequestForDispatchRow{
					AssistantAbilityRequest: sqlc.AssistantAbilityRequest{
						ID: a.RequestID, TenantID: a.TenantID, SiteID: a.SiteID, ProposedByGrantID: a.GrantID,
						EntryID: entry.EntryID, EntrySha256: sum, AbilityName: mcp.AbilityPageCreate,
						InputJson: c.input, State: "approved",
					},
					EntryEnabled: true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true,
				},
				site: sqlc.Site{
					ID: a.SiteID, TenantID: a.TenantID, Url: "https://shop.example.com",
					AgentVersion: c.version, ConnectionState: "connected",
				},
				entry: entry,
			}
			s := &Service{logger: slog.Default(), now: time.Now}
			s.SetWriteToolsEnabled(true)
			s.SetSender(unusedAgent{}, nameOnlyEntry, allowAllRules{})
			run := func(_ context.Context, _ domain.Principal, siteID uuid.UUID, fn func(pgx.Tx, *sqlc.Queries) error) error {
				if siteID != a.SiteID {
					t.Fatalf("transaction scoped to site %s, the job names %s", siteID, a.SiteID)
				}
				return fn(nil, sqlc.New(db))
			}

			plan, done, err := s.checkSite(context.Background(), run, domain.Principal{}, a)
			if err != nil {
				t.Fatalf("checkSite: %v (sent %v)", err, db.names())
			}
			closed, didClose := db.find("CloseApprovedAbilityRequestNotSent")
			_, readEntry := db.find("GetAbilityCatalogueEntry")
			_, attempted := db.find("RecordAbilityRequestDispatchAttempt")
			if attempted {
				t.Fatalf("a dispatch attempt was recorded (sent %v)", db.names())
			}
			if c.closes {
				if !done || !didClose {
					t.Fatalf("plugin %q: not closed before the reservation (done %v, sent %v)", c.version, done, db.names())
				}
				if len(closed.args) != 3 || closed.args[0] != ReasonAgentOutdated || closed.args[1] != a.TenantID || closed.args[2] != a.RequestID {
					t.Fatalf("closed with %v, want [%s %s %s]", closed.args, ReasonAgentOutdated, a.TenantID, a.RequestID)
				}
				if readEntry {
					t.Fatalf("the catalogue entry was read for a request that cannot be sent (sent %v)", db.names())
				}
				return
			}
			if done || didClose {
				t.Fatalf("plugin %q: closed or stopped (done %v, sent %v)", c.version, done, db.names())
			}
			if !readEntry || plan.siteURL != db.site.Url || plan.sum != sum || string(plan.entry) != `{"name":"`+mcp.AbilityPageCreate+`"}` {
				t.Fatalf("plan not ready to reserve: %+v (sent %v)", plan, db.names())
			}
		})
	}
}
