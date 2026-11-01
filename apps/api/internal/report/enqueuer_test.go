package report

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"
)

// The queue lookup finds a report's job by JSON containment on two keys of the
// args River stored. They have to be the keys GenerateArgs encodes the tenant
// and the report under: with a key that differs, the lookup matches nothing and
// every queued job looks dead.
func TestGenerateJobFilterKeysAreTheOnesGenerateArgsEncodes(t *testing.T) {
	tenantID, reportID := uuid.New(), uuid.New()

	filterJSON, err := json.Marshal(generateJobFilter{TenantID: tenantID, ReportID: reportID})
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := json.Marshal(GenerateArgs{
		TenantID: tenantID, ClientID: uuid.New(), ReportID: reportID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var filter, args map[string]any
	if err := json.Unmarshal(filterJSON, &filter); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		t.Fatal(err)
	}

	// The filter names the tenant and the report, and nothing else: any other
	// key would have to match the job's whole args.
	if len(filter) != 2 || filter["tenant_id"] != tenantID.String() || filter["report_id"] != reportID.String() {
		t.Fatalf("filter = %s, want exactly tenant_id and report_id", filterJSON)
	}
	for key, want := range filter {
		got, ok := args[key]
		if !ok {
			t.Errorf("filter key %q is not a key GenerateArgs encodes (%s)", key, argsJSON)
			continue
		}
		if got != want {
			t.Errorf("filter %q = %v, GenerateArgs encodes it as %v", key, want, got)
		}
	}
}

// A job is alive unless it has finished. Every River state is classified, so a
// state a later River release adds is a decision made here and not a quiet
// "dead".
func TestActiveGenerateJobStatesAreEveryStateButTheFinishedOnes(t *testing.T) {
	finished := map[rivertype.JobState]bool{
		rivertype.JobStateCompleted: true,
		rivertype.JobStateCancelled: true,
		rivertype.JobStateDiscarded: true,
	}
	active := make(map[rivertype.JobState]bool, len(activeGenerateJobStates))
	for _, s := range activeGenerateJobStates {
		if finished[s] {
			t.Errorf("state %q has finished and counts as active", s)
		}
		if active[s] {
			t.Errorf("state %q is listed twice", s)
		}
		active[s] = true
	}
	for _, s := range rivertype.JobStates() {
		if !finished[s] && !active[s] {
			t.Errorf("River state %q is neither listed as active nor known to be finished", s)
		}
	}
}
