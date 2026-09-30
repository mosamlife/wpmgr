package mcp

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// The per-process read limits on site_ability_run (v4 §1.4): 30 a minute per
// connection per site, and 600 a day per connection. Fixed windows, counted
// only when a call is admitted.
const (
	abilityReadPerMinute = 30
	abilityReadPerDay    = 600

	limitScopeAbilitySiteMinute = "ability_read_site_minute"
	limitScopeAbilityDay        = "ability_read_connection_day"

	abilityLimitKeyCap = 10000
)

type windowCount struct {
	start time.Time
	n     int
}

type abilityReadLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	sites map[[2]uuid.UUID]*windowCount
	days  map[uuid.UUID]*windowCount
}

func newAbilityReadLimiter() *abilityReadLimiter {
	return &abilityReadLimiter{
		now:   time.Now,
		sites: map[[2]uuid.UUID]*windowCount{},
		days:  map[uuid.UUID]*windowCount{},
	}
}

// windowLocked returns the live window for key, starting a new one when the
// old one has passed.
func windowLocked[K comparable](m map[K]*windowCount, key K, width time.Duration, now time.Time) *windowCount {
	w, ok := m[key]
	if !ok || now.Sub(w.start) >= width {
		if !ok && len(m) >= abilityLimitKeyCap {
			for k, v := range m {
				if now.Sub(v.start) >= width {
					delete(m, k)
				}
			}
		}
		w = &windowCount{start: now}
		m[key] = w
	}
	return w
}

// allow admits one read for (grant, site) or says which limit refused it and
// how long until it would be admitted. A nil receiver refuses.
func (l *abilityReadLimiter) allow(grantID, siteID uuid.UUID) requestRateDecision {
	if l == nil {
		return requestRateDecision{retryAfter: time.Minute, scope: limitScopeAbilitySiteMinute}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	day := windowLocked(l.days, grantID, 24*time.Hour, now)
	if day.n >= abilityReadPerDay {
		return requestRateDecision{retryAfter: atLeastASecond(day.start.Add(24 * time.Hour).Sub(now)), scope: limitScopeAbilityDay}
	}
	minute := windowLocked(l.sites, [2]uuid.UUID{grantID, siteID}, time.Minute, now)
	if minute.n >= abilityReadPerMinute {
		return requestRateDecision{retryAfter: atLeastASecond(minute.start.Add(time.Minute).Sub(now)), scope: limitScopeAbilitySiteMinute}
	}
	day.n++
	minute.n++
	return requestRateDecision{allowed: true}
}
