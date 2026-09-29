package main

import (
	"errors"
	"testing"
)

type fakeWriteToolsMCP struct {
	refuse bool
	on     bool
	calls  []bool
}

func (f *fakeWriteToolsMCP) SetWriteToolsEnabled(on bool) error {
	f.calls = append(f.calls, on)
	if on && f.refuse {
		f.on = false
		return errors.New("no rail")
	}
	f.on = on
	return nil
}

func (f *fakeWriteToolsMCP) WriteToolsEnabled() bool { return f.on }

type fakeWriteToolsApproval struct {
	set []bool
}

func (f *fakeWriteToolsApproval) SetWriteToolsEnabled(on bool) { f.set = append(f.set, on) }

// TestSwitchWriteTools_TheRailDecides: the approval half is handed what the
// MCP half adopted, a missing rail fails boot, and nothing turns on without
// the environment AND the agent client.
func TestSwitchWriteTools_TheRailDecides(t *testing.T) {
	cases := []struct {
		name             string
		requested, agent bool
		refuse           bool
		wantOn, wantErr  bool
		wantApprovalLast bool
	}{
		{"off", false, true, false, false, false, false},
		{"no agent client", true, false, false, false, false, false},
		{"on", true, true, false, true, false, true},
		{"on but the rail is absent", true, true, true, false, true, false},
	}
	for _, c := range cases {
		m := &fakeWriteToolsMCP{refuse: c.refuse}
		a := &fakeWriteToolsApproval{}
		on, err := switchWriteTools(c.requested, c.agent, m, a)
		if on != c.wantOn || (err != nil) != c.wantErr {
			t.Errorf("%s: on=%v err=%v, want on=%v err=%v", c.name, on, err, c.wantOn, c.wantErr)
		}
		if len(a.set) != 1 || a.set[0] != c.wantApprovalLast {
			t.Errorf("%s: approval half set %v, want exactly [%v]", c.name, a.set, c.wantApprovalLast)
		}
		if len(m.calls) != 1 || m.calls[0] != (c.requested && c.agent) {
			t.Errorf("%s: the MCP half was asked %v", c.name, m.calls)
		}
	}
}
