package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// A tool whose Effect is EffectRequest writes a pending request instead of
// acting, and a person approves it in WPMgr (registry.go, Effect). Its
// description has to say so, and has to name the read tool that follows the
// request by its request_id, or a caller is told something other than what the
// call does. site_ability_run once said that only reviewed reads run, after
// change abilities had started going through it.
func TestRequestEffectToolsSayThatAPersonApprovesAndHowToFollow(t *testing.T) {
	entries := nonEmptyRegistry(t)
	var checked []string
	for _, e := range entries {
		if e.Effect != EffectRequest {
			continue
		}
		checked = append(checked, e.Name)
		if !strings.Contains(strings.ToLower(e.Description), "approv") {
			t.Errorf("%q writes a request, but its description never says that a person approves it:\n%s",
				e.Name, e.Description)
		}
		followed := false
		for _, other := range entries {
			if other.Effect == EffectRead && strings.Contains(string(other.InputSchema), `"request_id"`) &&
				strings.Contains(e.Description, "`"+other.Name+"`") {
				followed = true
			}
		}
		if !followed {
			t.Errorf("%q writes a request, but its description does not name a registered read tool that takes a request_id:\n%s",
				e.Name, e.Description)
		}
	}
	// A loop over an empty set passes having checked nothing.
	if len(checked) == 0 {
		t.Fatal("no request-effect tool is registered, so nothing was checked")
	}
	if !containsString(checked, ToolSiteAbilityRun) {
		t.Fatalf("%s is not among the request-effect tools checked %v", ToolSiteAbilityRun, checked)
	}
	t.Logf("request-effect tools checked: %v", checked)
}

// The run tool's description says that the result of a change carries a
// request_id to follow with the status tool. This is the result a change gets,
// built by the function the creation rail builds it with.
func TestAbilityRequestResultCarriesTheRequestIDTheRunDescriptionPointsAt(t *testing.T) {
	id := uuid.New()
	row := sqlc.AssistantAbilityRequest{
		ID: id, SiteID: uuid.New(), AbilityName: AbilityPageCreate, Snapshot: "none",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	b, err := json.Marshal(abilityResultFromRow(row, false))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the result is not a JSON object: %v: %s", err, b)
	}
	if got["request_id"] != id.String() {
		t.Fatalf("request_id = %v, want %s: %s", got["request_id"], id, b)
	}
	if got["state"] != stateWaitingForApproval {
		t.Fatalf("state = %v, want %s: %s", got["state"], stateWaitingForApproval, b)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, ToolSiteAbilityRequestStatus) {
		t.Fatalf("message does not name %s: %s", ToolSiteAbilityRequestStatus, b)
	}

	// The status tool takes the member the result carries.
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(abilityStatusSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["request_id"]; !ok {
		t.Fatalf("%s takes no request_id: %s", ToolSiteAbilityRequestStatus, abilityStatusSchema)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
