package html

import (
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #969. The Updates section renders the version table and the Core count.

func updateTableFixture(t *testing.T) reportdata.ReportData {
	t.Helper()
	data := partialFixture(t, false)
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	data.Sites[0].Updates = &reportdata.UpdateSection{
		Plugins: 4, Themes: 1, Core: 7, Failed: 1, Total: 12,
		Items: []reportdata.UpdateItem{
			{Type: "plugin", Slug: "akismet", Name: "Akismet Anti-spam", FromVersion: "5.0", ToVersion: "5.3", Status: "succeeded", Date: day},
			{Type: "plugin", Slug: "woo", Name: "woo", FromVersion: "", ToVersion: "8.2", Status: "failed", Date: day},
			{Type: "core", Slug: "core", Name: "WordPress", FromVersion: "6.5", ToVersion: "6.5", Status: "rolled_back", Date: day},
		},
		MoreItems: 3,
	}
	return data
}

func TestHTMLRendersUpdateVersionTable(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render(updateTableFixture(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	for _, want := range []string{
		`class="update-table"`,
		">Akismet Anti-spam</td>",
		">5.0</td>",
		">5.3</td>",
		">unknown</td>",
		">Failed</td>",
		">Rolled back</td>",
		">WordPress</td>",
		">20 May 2026</td>",
		"and 3 more changes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered Updates section is missing %q", want)
		}
	}
	if !strings.Contains(body, `<div class="m-val">7</div><div class="m-lbl">Core</div>`) {
		t.Error("the HTML Updates section does not show the Core count")
	}
	if strings.Contains(body, "WPMgr") && !strings.Contains(body, "Prepared with WPMgr") {
		t.Error("the table added WPMgr branding")
	}
	if strings.Count(body, "WPMgr") != 1 {
		t.Errorf("WPMgr appears %d times; only the existing footer may name it", strings.Count(body, "WPMgr"))
	}
}

func TestHTMLNoTableWithoutItems(t *testing.T) {
	data := updateTableFixture(t)
	data.Sites[0].Updates.Items = nil
	data.Sites[0].Updates.MoreItems = 0
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "update-table") || strings.Contains(string(out), "more change") {
		t.Error("a section with no items rendered a table or an overflow line")
	}
}
