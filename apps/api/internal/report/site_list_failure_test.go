package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// A failed read of the client's sites used to come back as a report with no
// sites in it, completed, stored and e-mailed to the client as if the client
// had nothing. It is a failed attempt, so River tries again.

var errSiteListDown = errors.New("site list: connection refused")

func failingSiteList(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
	return nil, errSiteListDown
}

func TestBuildReportDataFailsWhenTheSiteListFails(t *testing.T) {
	rd, err := BuildReportData(context.Background(), Sources{ListClientSites: failingSiteList}, BuildInput{
		TenantID:    uuid.New(),
		ClientID:    uuid.New(),
		PeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatalf("BuildReportData returned a report with %d sites for a client whose sites could not be read", len(rd.Sites))
	}
	if !errors.Is(err, errSiteListDown) {
		t.Errorf("error %q does not wrap the site list failure", err)
	}
}

// A client with no sites is a real, if empty, report.
func TestBuildReportDataStillReportsAClientWithNoSites(t *testing.T) {
	rd, err := BuildReportData(context.Background(), Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) { return nil, nil },
	}, BuildInput{
		TenantID:    uuid.New(),
		ClientID:    uuid.New(),
		PeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	if rd.Totals.SiteCount != 0 || len(rd.Sites) != 0 {
		t.Errorf("sites=%d count=%d for a client with none", len(rd.Sites), rd.Totals.SiteCount)
	}
}

// Through the worker: the attempt fails and nothing is stored, the next
// attempt is River's to run, and only the last one marks the row failed.
func TestWorkFailsTheAttemptWhenTheSiteListFails(t *testing.T) {
	for _, attempt := range []int{1, 3} {
		repo := newFailRecorder(nil)
		src := oneSiteSources()
		src.ListClientSites = failingSiteList
		w, pub := finalAttemptWorker(t, repo, src, pdf.NewFpdfRenderer())
		err, p := workCatching(w, context.Background(), generateJob(attempt))
		if p != nil {
			t.Fatalf("attempt %d: Work panicked: %v", attempt, p)
		}
		if err == nil {
			t.Fatalf("attempt %d: Work completed a report whose sites could not be read", attempt)
		}
		if repo.snapshot != nil {
			t.Errorf("attempt %d: a snapshot was stored for a report with no site data", attempt)
		}
		wantFail := 0
		if attempt == 3 {
			wantFail = 1
		}
		if repo.failCalls != wantFail {
			t.Errorf("attempt %d of 3: FailReport called %d times, want %d", attempt, repo.failCalls, wantFail)
		}
		if attempt == 1 && len(pub.statuses) != 0 {
			t.Errorf("attempt 1 published %v while attempts remain", pub.statuses)
		}
	}
}
