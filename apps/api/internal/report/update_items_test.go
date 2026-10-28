package report

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// GH #969. The Updates section gains a per-site version table. These tests
// drive it through BuildReportData, the path the generate worker uses, with
// fake sources standing in for the two update queries.

var gh969Day = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

func gh969Row(siteID uuid.UUID, typ, slug, from, to, status string, day int) sqlc.ListReportUpdateItemsRow {
	return sqlc.ListReportUpdateItemsRow{
		SiteID:      siteID,
		TargetType:  typ,
		TargetSlug:  slug,
		FromVersion: from,
		ToVersion:   to,
		Status:      status,
		FinishedAt:  pgtype.Timestamptz{Time: gh969Day.AddDate(0, 0, day), Valid: true},
	}
}

func gh969Site(t *testing.T, plugins, themes []site.Component) site.Site {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"plugins": plugins, "themes": themes})
	if err != nil {
		t.Fatal(err)
	}
	return site.Site{ID: uuid.New(), Name: "site-a", URL: "https://a.example", Components: raw}
}

type gh969Call struct {
	siteID uuid.UUID
	limit  int32
}

// gh969Build runs BuildReportData for one site whose update tasks are rows.
func gh969Build(t *testing.T, s site.Site, rows []sqlc.ListReportUpdateItemsRow) (*UpdateSection, []gh969Call) {
	t.Helper()
	var calls []gh969Call
	src := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{s}, nil
		},
		GetUpdateReportStats: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]sqlc.GetUpdateReportStatsRow, error) {
			return nil, nil
		},
		ListReportUpdateItems: func(_ context.Context, _ uuid.UUID, siteID uuid.UUID, _, _ time.Time, limit int32) ([]sqlc.ListReportUpdateItemsRow, error) {
			calls = append(calls, gh969Call{siteID: siteID, limit: limit})
			if int(limit) < len(rows) {
				return rows[:limit], nil
			}
			return rows, nil
		},
	}
	rd, err := BuildReportData(context.Background(), src, BuildInput{
		TenantID:    uuid.New(),
		ClientID:    uuid.New(),
		PeriodStart: gh969Day,
		PeriodEnd:   gh969Day.AddDate(0, 1, 0),
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	if len(rd.Sites) != 1 || rd.Sites[0].Updates == nil {
		t.Fatalf("expected one site with an Updates section, got %+v", rd.Sites)
	}
	return rd.Sites[0].Updates, calls
}

func TestUpdateItemsFoldSuccessesPerComponent(t *testing.T) {
	s := gh969Site(t, []site.Component{{Slug: "akismet", Name: "Akismet Anti-spam"}}, nil)
	rows := []sqlc.ListReportUpdateItemsRow{
		gh969Row(s.ID, "plugin", "akismet", "5.0", "5.1", "succeeded", 2),
		gh969Row(s.ID, "plugin", "akismet", "5.1", "5.2", "succeeded", 9),
		gh969Row(s.ID, "plugin", "akismet", "5.2", "5.3", "succeeded", 20),
	}
	u, calls := gh969Build(t, s, rows)
	if len(u.Items) != 1 {
		t.Fatalf("three successful updates of one plugin must fold to one row, got %d: %+v", len(u.Items), u.Items)
	}
	it := u.Items[0]
	if it.FromVersion != "5.0" {
		t.Errorf("folded row starts at %q, want the first from-version 5.0", it.FromVersion)
	}
	if it.ToVersion != "5.3" {
		t.Errorf("folded row ends at %q, want the last to-version 5.3", it.ToVersion)
	}
	if want := gh969Day.AddDate(0, 0, 20); !it.Date.Equal(want) {
		t.Errorf("folded row dated %s, want the last update's date %s", it.Date, want)
	}
	if it.Status != "succeeded" {
		t.Errorf("folded row status %q, want succeeded", it.Status)
	}
	if len(calls) != 1 || calls[0].siteID != s.ID {
		t.Fatalf("the items source must be called once with this site's id, got %+v", calls)
	}
	if calls[0].limit != updateItemsFetchCap+1 {
		t.Errorf("items source limit = %d, want cap+1 = %d", calls[0].limit, updateItemsFetchCap+1)
	}
}

func TestUpdateItemsFailuresAreOwnRows(t *testing.T) {
	s := gh969Site(t, nil, nil)
	rows := []sqlc.ListReportUpdateItemsRow{
		gh969Row(s.ID, "plugin", "woo", "8.0", "8.1", "succeeded", 1),
		gh969Row(s.ID, "plugin", "woo", "8.1", "8.2", "failed", 3),
		gh969Row(s.ID, "plugin", "woo", "8.1", "8.2", "failed", 4),
		gh969Row(s.ID, "plugin", "woo", "8.1", "8.1", "rolled_back", 5),
		gh969Row(s.ID, "plugin", "woo", "8.1", "8.2", "succeeded", 6),
	}
	u, _ := gh969Build(t, s, rows)
	var ok, failed, rolled int
	for _, it := range u.Items {
		switch it.Status {
		case "succeeded":
			ok++
			if it.FromVersion != "8.0" || it.ToVersion != "8.2" {
				t.Errorf("successful row %s -> %s, want 8.0 -> 8.2", it.FromVersion, it.ToVersion)
			}
		case "failed":
			failed++
		case "rolled_back":
			rolled++
			if it.StatusLabel() != "Rolled back" {
				t.Errorf("rolled-back label %q", it.StatusLabel())
			}
		}
	}
	if ok != 1 || failed != 2 || rolled != 1 {
		t.Fatalf("want 1 folded success, 2 failed rows, 1 rolled-back row; got %d/%d/%d: %+v", ok, failed, rolled, u.Items)
	}
	for i := 1; i < len(u.Items); i++ {
		if u.Items[i].Date.Before(u.Items[i-1].Date) {
			t.Fatalf("rows are not in date order: %+v", u.Items)
		}
	}
}

func TestUpdateItemsUnknownVersionsAndNames(t *testing.T) {
	s := gh969Site(t,
		[]site.Component{{Slug: "akismet", Name: "Akismet Anti-spam"}},
		[]site.Component{{Slug: "astra", Name: "Astra"}},
	)
	rows := []sqlc.ListReportUpdateItemsRow{
		gh969Row(s.ID, "plugin", "akismet", "", "5.3", "succeeded", 1),
		gh969Row(s.ID, "plugin", "gone-plugin", "1.0", "", "succeeded", 2),
		gh969Row(s.ID, "theme", "astra", "4.0", "4.1", "succeeded", 3),
		gh969Row(s.ID, "theme", "akismet", "1.0", "1.1", "succeeded", 4),
		gh969Row(s.ID, "core", "core", "6.5", "6.6", "succeeded", 5),
	}
	u, _ := gh969Build(t, s, rows)
	if len(u.Items) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(u.Items), u.Items)
	}
	want := []struct{ name, from, to string }{
		{"Akismet Anti-spam", "unknown", "5.3"},
		{"gone-plugin", "1.0", "unknown"},
		{"Astra", "4.0", "4.1"},
		// A theme that shares a plugin's slug is not named after the plugin.
		{"akismet", "1.0", "1.1"},
		{"WordPress", "6.5", "6.6"},
	}
	for i, w := range want {
		it := u.Items[i]
		if it.Name != w.name {
			t.Errorf("row %d name %q, want %q", i, it.Name, w.name)
		}
		if it.FromLabel() != w.from || it.ToLabel() != w.to {
			t.Errorf("row %d prints %s -> %s, want %s -> %s", i, it.FromLabel(), it.ToLabel(), w.from, w.to)
		}
	}
	if u.Items[0].FromVersion != "" {
		t.Errorf("the snapshot must keep the unknown version empty, got %q", u.Items[0].FromVersion)
	}
}

func TestUpdateItemsCapAtFiftyWithOverflowCount(t *testing.T) {
	s := gh969Site(t, nil, nil)
	var rows []sqlc.ListReportUpdateItemsRow
	for i := 0; i < 53; i++ {
		rows = append(rows, gh969Row(s.ID, "plugin", fmt.Sprintf("p%02d", i), "1", "2", "succeeded", 0))
	}
	u, _ := gh969Build(t, s, rows)
	if len(u.Items) != 50 {
		t.Fatalf("want 50 rows shown, got %d", len(u.Items))
	}
	if u.MoreItems != 3 || u.MoreItemsAtLeast {
		t.Fatalf("want 3 more (exact), got %d (at least: %v)", u.MoreItems, u.MoreItemsAtLeast)
	}
	if got := u.MoreItemsLabel(); got != "and 3 more changes" {
		t.Errorf("overflow label %q, want %q", got, "and 3 more changes")
	}
}

func TestUpdateItemsNoOverflowLineAtExactlyFifty(t *testing.T) {
	s := gh969Site(t, nil, nil)
	var rows []sqlc.ListReportUpdateItemsRow
	for i := 0; i < 50; i++ {
		rows = append(rows, gh969Row(s.ID, "plugin", fmt.Sprintf("p%02d", i), "1", "2", "succeeded", 0))
	}
	u, _ := gh969Build(t, s, rows)
	if len(u.Items) != 50 || u.MoreItems != 0 || u.MoreItemsLabel() != "" {
		t.Fatalf("50 rows must all show with no overflow line, got %d rows, more=%d, label %q", len(u.Items), u.MoreItems, u.MoreItemsLabel())
	}
}

func TestUpdateItemsSourceCutIsALowerBound(t *testing.T) {
	s := gh969Site(t, nil, nil)
	var rows []sqlc.ListReportUpdateItemsRow
	for i := 0; i < updateItemsFetchCap+5; i++ {
		rows = append(rows, gh969Row(s.ID, "plugin", fmt.Sprintf("p%04d", i), "1", "2", "failed", 0))
	}
	u, _ := gh969Build(t, s, rows)
	if !u.MoreItemsAtLeast {
		t.Fatal("rows past the fetch cap were cut, but the section does not say so")
	}
	if u.MoreItems != updateItemsFetchCap-50 {
		t.Errorf("MoreItems %d, want %d", u.MoreItems, updateItemsFetchCap-50)
	}
	if got, want := u.MoreItemsLabel(), fmt.Sprintf("and at least %d more changes", updateItemsFetchCap-50); got != want {
		t.Errorf("label %q, want %q", got, want)
	}
}
