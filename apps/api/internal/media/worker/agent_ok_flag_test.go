package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
)

// okFlagSites is a SiteLookup that always resolves an enrolled site.
type okFlagSites struct{}

func (okFlagSites) GetMediaSiteURL(context.Context, uuid.UUID, uuid.UUID) (string, bool, error) {
	return "https://example.test", true, nil
}

// okFlagPresigner returns a fixed URL for every key.
type okFlagPresigner struct{}

func (okFlagPresigner) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://signed.example/get", nil
}
func (okFlagPresigner) PresignPut(context.Context, string, time.Duration) (string, error) {
	return "https://signed.example/put", nil
}
func (okFlagPresigner) Delete(context.Context, string) error { return nil }

// refusingApplyClient acks media_apply with a 200 whose body says ok=false: the
// agent REFUSED to apply the encoded variants.
type refusingApplyClient struct{ detail string }

func (c refusingApplyClient) MediaApply(context.Context, uuid.UUID, string, agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	return agentcmd.MediaApplyResponse{OK: false, Detail: c.detail}, nil
}

// acceptingApplyClient acks normally.
type acceptingApplyClient struct{}

func (acceptingApplyClient) MediaApply(context.Context, uuid.UUID, string, agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	return agentcmd.MediaApplyResponse{OK: true}, nil
}

func newOkFlagWorker(apply AgentApplyClient) *EncodeWorker {
	return NewEncodeWorker(nil, &fakeJobRepo{}, okFlagPresigner{}, nil, okFlagSites{},
		apply, "https://cp.example", time.Minute, nil)
}

// TestDispatchApply_AgentOkFalseIsAnError proves that media_apply answered with
// a 200 carrying ok=false is surfaced as an error. The agent refused, so it will
// never post the job-status callback, and returning nil here would complete the
// River job while the media job hung at "encoded" forever.
func TestDispatchApply_AgentOkFalseIsAnError(t *testing.T) {
	w := newOkFlagWorker(refusingApplyClient{detail: "body must be an empty object"})
	args := model.EncodeArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), JobID: "job-1", WPAttachmentID: 7,
	}
	variants := []agentcmd.MediaApplyVariant{{Name: "full", OptimizedMime: "image/avif", OptimizedSize: 100}}

	err := w.dispatchApply(context.Background(), args, variants)
	if err == nil {
		t.Fatal("a 200 carrying ok=false is the agent refusing media_apply; dispatchApply must report a failure")
	}
	if !strings.Contains(err.Error(), "body must be an empty object") {
		t.Fatalf("the agent's own detail must be preserved, got %q", err.Error())
	}
}

// TestDispatchApply_AgentOkTrueSucceeds guards the fix against over-reach.
func TestDispatchApply_AgentOkTrueSucceeds(t *testing.T) {
	w := newOkFlagWorker(acceptingApplyClient{})
	args := model.EncodeArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), JobID: "job-1", WPAttachmentID: 7,
	}
	variants := []agentcmd.MediaApplyVariant{{Name: "full", OptimizedMime: "image/avif", OptimizedSize: 100}}

	if err := w.dispatchApply(context.Background(), args, variants); err != nil {
		t.Fatalf("an accepted media_apply must succeed, got %v", err)
	}
}
