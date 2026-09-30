package mcp

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAbilityReadLimiter_MinutePerSiteAndDayPerConnection(t *testing.T) {
	l := newAbilityReadLimiter()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	grant, siteA, siteB := uuid.New(), uuid.New(), uuid.New()

	for i := 0; i < abilityReadPerMinute; i++ {
		if d := l.allow(grant, siteA); !d.allowed {
			t.Fatalf("read %d refused inside the minute limit", i+1)
		}
	}
	d := l.allow(grant, siteA)
	if d.allowed || d.scope != limitScopeAbilitySiteMinute || d.retryAfter <= 0 {
		t.Fatalf("read %d = %+v, want refused by the per-site minute limit", abilityReadPerMinute+1, d)
	}
	// Another site of the same connection has its own minute.
	if d := l.allow(grant, siteB); !d.allowed {
		t.Fatal("a second site was refused by the first site's minute")
	}
	// A refused call costs nothing; the next minute admits again.
	now = now.Add(time.Minute)
	if d := l.allow(grant, siteA); !d.allowed {
		t.Fatal("the next minute did not admit")
	}

	// The day limit spans sites: fill it across fresh sites.
	used := abilityReadPerMinute + 2
	for used < abilityReadPerDay {
		s := uuid.New()
		for i := 0; i < abilityReadPerMinute && used < abilityReadPerDay; i++ {
			if d := l.allow(grant, s); !d.allowed {
				t.Fatalf("read %d refused below the day limit: %+v", used+1, d)
			}
			used++
		}
	}
	d = l.allow(grant, uuid.New())
	if d.allowed || d.scope != limitScopeAbilityDay {
		t.Fatalf("read %d = %+v, want refused by the day limit", abilityReadPerDay+1, d)
	}
	// Another connection is unaffected.
	if d := l.allow(uuid.New(), siteA); !d.allowed {
		t.Fatal("another connection was refused by this one's day")
	}
	now = now.Add(24 * time.Hour)
	if d := l.allow(grant, siteA); !d.allowed {
		t.Fatal("the next day did not admit")
	}
}

// The tool answers the documented rate-limit error, and the agent is not
// called for the refused read.
func TestAbilityRun_RateLimitedAnswersRateLimitError(t *testing.T) {
	f := newAbilityFixture(t)
	r := f.router(t, &capturingRecorder{})
	body := callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "wpmgr/site-facts"})
	for i := 0; i < abilityReadPerMinute; i++ {
		if resp := decodeRPC(t, post(t, r, body, nil)); resp.Error != nil {
			t.Fatalf("read %d refused: %+v", i+1, resp.Error)
		}
	}
	calls := f.agent.calls
	resp := decodeRPC(t, post(t, r, body, nil))
	if resp.Error == nil || resp.Error.Code != codeRateLimited {
		t.Fatalf("read %d = %+v, want %d", abilityReadPerMinute+1, resp.Error, codeRateLimited)
	}
	if f.agent.calls != calls {
		t.Fatal("the agent was called for a rate-limited read")
	}
}
