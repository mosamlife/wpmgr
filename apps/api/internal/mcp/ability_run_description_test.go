package mcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

var (
	// clauseBreak ends a clause at a full stop, question mark, exclamation
	// mark, semicolon, colon or comma followed by white space. A description is
	// plain prose with identifiers in backticks, which hold no such break.
	clauseBreak = regexp.MustCompile(`[.!?;:,]\s+`)

	// negation matches, in a lower-cased clause, any word that can turn a
	// statement about approval round: not, no, never, without, nobody, none,
	// cannot, automatic and its forms, and a contraction such as doesn't (with
	// a straight or a curly apostrophe).
	negation = regexp.MustCompile(`\b(not|no|never|without|nobody|none|cannot|automatic\w*)\b|n['’]t\b`)
)

// saysAPersonApproves reports whether a description has a clause that names a
// person approving. "person" and "approv" must both be in that one clause, and
// the clause must hold no negation word. So a person who can only see a
// request does not count, nor does "approved" in some other clause, nor "a
// person does not approve it", "approved automatically" or "approved without a
// person".
//
// The test is on the clause and on a closed set of negation words, not on a
// list of phrases. A clause that names a person approving and also holds one
// of those words fails even when it is honest ("without delay", "cannot ...
// until"). Put the negation in a clause of its own, after a comma, a semicolon
// or a full stop.
func saysAPersonApproves(desc string) bool {
	for _, c := range clauseBreak.Split(strings.ToLower(desc), -1) {
		if strings.Contains(c, "person") && strings.Contains(c, "approv") && !negation.MatchString(c) {
			return true
		}
	}
	return false
}

// A tool whose Effect is EffectRequest writes a pending request instead of
// acting, and a person approves it in WPMgr (registry.go, Effect). Its
// description has to say so in a clause that names a person approving
// (saysAPersonApproves), and has to name the read tool that follows the
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
		if !saysAPersonApproves(e.Description) {
			t.Errorf("%q writes a request, but no clause of its description says that a person approves it "+
				"(a clause that holds not, no, never, without, nobody, none, cannot, n't or automatic does not count):\n%s",
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

// The predicate itself. Each row is a description, or a fragment of one, and
// whether it names a person approving. The two shipped descriptions are cases
// so that a predicate which refuses what ships goes red here, by name, rather
// than only in the registry test above.
func TestSaysAPersonApproves(t *testing.T) {
	shipped := map[string]string{}
	for _, e := range nonEmptyRegistry(t) {
		shipped[e.Name] = e.Description
	}
	for _, name := range []string{ToolSiteAbilityRun, ToolSiteCachePurgeRequest} {
		if shipped[name] == "" {
			t.Fatalf("%s is not registered, so its description cannot be a case", name)
		}
	}
	cases := []struct {
		name string
		desc string
		want bool
	}{
		// What it must accept.
		{"the run tool as shipped", shipped[ToolSiteAbilityRun], true},
		{"the cache-clear request tool as shipped", shipped[ToolSiteCachePurgeRequest], true},
		{"one short sentence", "A person approves it in WPMgr.", true},
		{"any letter case", "A Person APPROVES it in WPMgr", true},
		{"reads need no approval, in a sentence of their own",
			"Reads need no approval. A change becomes a request that a person approves.", true},
		{"reads need no approval, in a clause of their own",
			"Reads need no approval, but a change becomes a request that a person approves.", true},
		{"nothing is not a negation word", "Nothing changes until a person approves it.", true},
		{"note and another are not negation words", "Note that another person approves it.", true},

		// What it must refuse.
		{"automatic approval", "automatically approved", false},
		{"no approval", "no approval needed", false},
		{"approved without a person", "approved without a person", false},
		{"a person, but no approval word", "a person can see it", false},
		{"empty", "", false},
		{"a person and automatic in one sentence", "Requests from a person are approved automatically.", false},
		{"a person's approval not needed", "A person's approval is not needed.", false},
		{"no approval from a person", "No approval from a person is needed.", false},
		{"person and approval in different sentences", "A person can see the request. It is approved later.", false},
		{"a person does not approve", "A person does not approve the request.", false},
		{"a person doesn't approve", "A person doesn't approve it.", false},
		{"a person doesn't approve, curly apostrophe", "A person doesn’t approve it.", false},
		{"a person never approves", "A person never approves it.", false},
		{"a person cannot approve", "A person cannot approve it.", false},
		{"none of it is approved by a person", "None of it is approved by a person.", false},
		{"nobody approves, in the same clause as a person", "A person sees it and nobody approves it.", false},
		{"nobody approves, a person only looks, in two clauses", "Nobody approves it; a person can see it.", false},
	}
	for _, c := range cases {
		if got := saysAPersonApproves(c.desc); got != c.want {
			t.Errorf("%s: saysAPersonApproves(%q) = %v, want %v", c.name, c.desc, got, c.want)
		}
	}
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
