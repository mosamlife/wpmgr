package site

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// newEnqueueService is a service whose saved address is saved, with a
// recording queue wired.
func newEnqueueService(saved string) (*Service, *recordingQueue) {
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	svc, _ := newAdoptService(saved, &fakeProber{pingOK: true}, clk)
	q := &recordingQueue{}
	svc.SetAdoptURLEnqueuer(q)
	return svc, q
}

// oversizedAddress is a well-formed site address of exactly n bytes, so only
// its length can refuse it.
func oversizedAddress(t *testing.T, n int, tag string) string {
	t.Helper()
	head := "https://example.com/" + tag
	if n < len(head) {
		t.Fatalf("oversizedAddress: %d bytes is shorter than %q", n, head)
	}
	s := head + strings.Repeat("a", n-len(head))
	if _, _, ok := siteaddr.Parse(s); !ok {
		t.Fatalf("positive control: siteaddr.Parse refused the %d-byte address; the length cap would not be what drops it", n)
	}
	return s
}

// TestEnqueueAdoptReportedURL_DropsAnOversizedReport: an 8 MiB report, well
// formed as an address, queues nothing and never fails the push. The limit is
// exact: 2048 bytes queues, 2049 does not.
func TestEnqueueAdoptReportedURL_DropsAnOversizedReport(t *testing.T) {
	ctx := context.Background()
	svc, q := newEnqueueService("http://example.com")

	if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), oversizedAddress(t, 8<<20, ""), "agent_diagnostics", ""); err != nil {
		t.Fatalf("an oversized report failed the push: %v", err)
	}
	if n := len(q.jobs); n != 0 {
		t.Fatalf("an 8 MiB report queued %d jobs, want 0", n)
	}

	if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), oversizedAddress(t, maxReportedURLLen+1, ""), "agent_diagnostics", ""); err != nil {
		t.Fatalf("a %d-byte report failed the push: %v", maxReportedURLLen+1, err)
	}
	if n := len(q.jobs); n != 0 {
		t.Fatalf("a %d-byte report queued %d jobs, want 0", maxReportedURLLen+1, n)
	}

	atLimit := oversizedAddress(t, maxReportedURLLen, "")
	if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), atLimit, "agent_diagnostics", ""); err != nil {
		t.Fatalf("a %d-byte report: %v", maxReportedURLLen, err)
	}
	if n := len(q.jobs); n != 1 || q.jobs[0].Reported != atLimit {
		t.Fatalf("a %d-byte report queued %d jobs, want exactly 1 carrying it", maxReportedURLLen, n)
	}
}

// TestEnqueueAdoptReportedURL_DropsANonAddress: a report siteaddr.Parse
// refuses queues nothing and never fails the push.
func TestEnqueueAdoptReportedURL_DropsANonAddress(t *testing.T) {
	ctx := context.Background()
	svc, q := newEnqueueService("http://example.com")

	for _, junk := range []string{"not a url", "ftp://example.com", "https://user@example.com", "https://example.com/?q=1", "https://"} {
		if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), junk, "agent_diagnostics", ""); err != nil {
			t.Fatalf("%q failed the push: %v", junk, err)
		}
	}
	if n := len(q.jobs); n != 0 {
		t.Fatalf("reports that are not site addresses queued %d jobs, want 0: %+v", n, q.jobs)
	}
}

// TestEnqueueAdoptReportedURL_QueuesAValidEquivalentAddress: an address the
// saved one could become still queues exactly one job, from either push.
func TestEnqueueAdoptReportedURL_QueuesAValidEquivalentAddress(t *testing.T) {
	ctx := context.Background()

	svc, q := newEnqueueService("http://example.com")
	if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), "https://example.com", "agent_diagnostics", ""); err != nil {
		t.Fatalf("diagnostics enqueue: %v", err)
	}
	if n := len(q.jobs); n != 1 || q.jobs[0].Reported != "https://example.com" {
		t.Fatalf("diagnostics push queued %+v, want exactly 1 job for https://example.com", q.jobs)
	}

	svc, q = newEnqueueService("http://example.com")
	if _, err := svc.ApplyAgentMetadata(ctx, uuid.New(), uuid.New(), agentpkg.Metadata{HomeURL: "https://www.example.com"}); err != nil {
		t.Fatalf("metadata push: %v", err)
	}
	if n := len(q.jobs); n != 1 || q.jobs[0].Reported != "https://www.example.com" {
		t.Fatalf("metadata push queued %+v, want exactly 1 job for https://www.example.com", q.jobs)
	}
}

// TestEnqueueAdoptReportedURL_MetadataDropsWhatTheSavedAddressCannotBecome:
// the metadata push knows the saved address, so a report the job would never
// adopt over it (another host, another path, the same address) queues nothing.
func TestEnqueueAdoptReportedURL_MetadataDropsWhatTheSavedAddressCannotBecome(t *testing.T) {
	ctx := context.Background()
	svc, q := newEnqueueService("https://example.com")

	for _, reported := range []string{"https://other.example.org", "https://example.com/blog", "http://example.com", "https://example.com/", "https://EXAMPLE.com"} {
		if _, err := svc.ApplyAgentMetadata(ctx, uuid.New(), uuid.New(), agentpkg.Metadata{HomeURL: reported}); err != nil {
			t.Fatalf("metadata push of %q: %v", reported, err)
		}
	}
	if n := len(q.jobs); n != 0 {
		t.Fatalf("reports the saved address cannot become queued %d jobs, want 0: %+v", n, q.jobs)
	}
}

// TestEnqueueAdoptReportedURL_FiftyJunkReportsQueueNothing: fifty distinct
// junk reports from one site, half unparseable and half well formed but over
// the limit, queue no job at all.
func TestEnqueueAdoptReportedURL_FiftyJunkReportsQueueNothing(t *testing.T) {
	ctx := context.Background()
	svc, q := newEnqueueService("http://example.com")
	tenant, site := uuid.New(), uuid.New()

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		junk := fmt.Sprintf("junk %d, not a url", i)
		if i%2 == 1 {
			junk = oversizedAddress(t, maxReportedURLLen+1+i, fmt.Sprintf("%d-", i))
		}
		if seen[junk] {
			t.Fatalf("junk report %d repeats an earlier one", i)
		}
		seen[junk] = true
		if err := svc.EnqueueAdoptReportedURL(ctx, tenant, site, junk, "agent_diagnostics", ""); err != nil {
			t.Fatalf("junk report %d failed the push: %v", i, err)
		}
	}
	if n := len(q.jobs); n != 0 {
		t.Fatalf("50 distinct junk reports queued %d jobs, want 0", n)
	}
}
