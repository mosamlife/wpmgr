package backup

// schedule_test.go — pure-domain table tests for nextOccurrence, resolveLocation,
// and the SiteJitter helper. No database, no clock access.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// mustLoc loads a timezone or panics. Only used in test setup.
func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("mustLoc: " + name + ": " + err.Error())
	}
	return loc
}

// ptr is a convenience helper returning a pointer to an int.
func ptr(v int) *int { return &v }

// TestNextOccurrence covers all cadences, DST transitions, month-end, and a
// +5:30 offset zone. All input times are constructed via time.Date — the real
// clock is never read.
func TestNextOccurrence(t *testing.T) {
	nyc := mustLoc("America/New_York")
	kolkata := mustLoc("Asia/Kolkata") // UTC+5:30
	utc := time.UTC

	// fixedPlus530 is a fixed-zone stand-in for sites that report gmt_offset=5.5
	// but no IANA name (resolveLocation fallback).
	fixedPlus530 := time.FixedZone("UTC+5", int(5.5*3600))

	type tc struct {
		name       string
		now        time.Time
		cadence    string
		hour       int
		minute     int
		dow        *int // nil unless weekly
		dom        *int // nil unless monthly
		freqHours  *int // nil unless every_n_hours
		jitter     int
		loc        *time.Location
		wantAfter  time.Time // result must be strictly after this
		wantBefore time.Time // result must be strictly before this
	}

	cases := []tc{
		// ----------------------------------------------------------------
		// daily — time not yet reached today
		// ----------------------------------------------------------------
		{
			name:    "daily/future-today",
			now:     time.Date(2026, 1, 15, 1, 0, 0, 0, utc),
			cadence: CadenceDaily,
			hour:    2, minute: 0,
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 14, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 2, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// daily — time already past today, must roll to tomorrow
		// ----------------------------------------------------------------
		{
			name:    "daily/past-today",
			now:     time.Date(2026, 1, 15, 3, 0, 0, 0, utc),
			cadence: CadenceDaily,
			hour:    2, minute: 0,
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 15, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 16, 2, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// daily — Asia/Kolkata +05:30, schedule 02:00 IST
		// 02:00 IST = 20:30 UTC previous day
		// now = 21:00 UTC Jan 14 (02:30 IST Jan 15, past 02:00) → next 20:30 UTC Jan 15
		// ----------------------------------------------------------------
		{
			name:    "daily/kolkata-02:00-IST",
			now:     time.Date(2026, 1, 14, 21, 0, 0, 0, utc),
			cadence: CadenceDaily,
			hour:    2, minute: 0,
			jitter:     0,
			loc:        kolkata,
			wantAfter:  time.Date(2026, 1, 15, 20, 29, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 20, 31, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// daily — fixed +05:30 zone (resolveLocation fallback, no DST)
		// ----------------------------------------------------------------
		{
			name:    "daily/fixed-plus530",
			now:     time.Date(2026, 1, 14, 21, 0, 0, 0, utc),
			cadence: CadenceDaily,
			hour:    2, minute: 0,
			jitter:     0,
			loc:        fixedPlus530,
			wantAfter:  time.Date(2026, 1, 15, 20, 29, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 20, 31, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// daily — with 7-minute jitter
		// ----------------------------------------------------------------
		{
			name:    "daily/with-jitter",
			now:     time.Date(2026, 1, 15, 1, 0, 0, 0, utc),
			cadence: CadenceDaily,
			hour:    2, minute: 0,
			jitter:     7,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 15, 2, 6, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 2, 8, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// weekly — next Monday (dow=1) when today is Wednesday (dow=3)
		// ----------------------------------------------------------------
		{
			name:    "weekly/wednesday-to-monday",
			now:     time.Date(2026, 1, 14, 12, 0, 0, 0, utc), // Wednesday
			cadence: CadenceWeekly,
			hour:    3, minute: 0,
			dow:    ptr(1), // Monday
			jitter: 0,
			loc:    utc,
			// 2026-01-19 (Monday) at 03:00 UTC
			wantAfter:  time.Date(2026, 1, 18, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 19, 3, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// weekly — exact day/time already past, roll to next week
		// ----------------------------------------------------------------
		{
			name:    "weekly/same-day-past",
			now:     time.Date(2026, 1, 14, 4, 0, 0, 0, utc), // Wednesday 04:00 UTC, past 03:00
			cadence: CadenceWeekly,
			hour:    3, minute: 0,
			dow:    ptr(3), // Wednesday
			jitter: 0,
			loc:    utc,
			// next Wednesday = 2026-01-21 at 03:00 UTC
			wantAfter:  time.Date(2026, 1, 20, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 21, 3, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// monthly — dom 15, before the 15th this month
		// ----------------------------------------------------------------
		{
			name:    "monthly/before-dom",
			now:     time.Date(2026, 1, 10, 0, 0, 0, 0, utc),
			cadence: CadenceMonthly,
			hour:    2, minute: 0,
			dom:        ptr(15),
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 14, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 2, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// monthly — dom 15, already past → next month
		// ----------------------------------------------------------------
		{
			name:    "monthly/past-dom",
			now:     time.Date(2026, 1, 20, 0, 0, 0, 0, utc),
			cadence: CadenceMonthly,
			hour:    2, minute: 0,
			dom:        ptr(15),
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 2, 14, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 2, 15, 2, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// monthly — dom 28 in February (month-end edge: Feb 28 exists in 2026)
		// ----------------------------------------------------------------
		{
			name:    "monthly/feb-day28",
			now:     time.Date(2026, 2, 1, 0, 0, 0, 0, utc),
			cadence: CadenceMonthly,
			hour:    0, minute: 0,
			dom:        ptr(28),
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 2, 27, 23, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 2, 28, 0, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// hourly — minute not yet reached in current hour
		// ----------------------------------------------------------------
		{
			name:    "hourly/future-in-hour",
			now:     time.Date(2026, 1, 15, 10, 20, 0, 0, utc),
			cadence: CadenceHourly,
			hour:    0, minute: 30,
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 15, 10, 29, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 10, 31, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// hourly — minute already past, roll to next hour
		// ----------------------------------------------------------------
		{
			name:    "hourly/past-in-hour",
			now:     time.Date(2026, 1, 15, 10, 45, 0, 0, utc),
			cadence: CadenceHourly,
			hour:    0, minute: 30,
			jitter:     0,
			loc:        utc,
			wantAfter:  time.Date(2026, 1, 15, 11, 29, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 11, 31, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// every_n_hours — 6h interval, anchor 00:00, now at 07:00 → next 12:00
		// ----------------------------------------------------------------
		{
			name:    "every_n_hours/6h-anchor0",
			now:     time.Date(2026, 1, 15, 7, 0, 0, 0, utc),
			cadence: CadenceEveryNHours,
			hour:    0, minute: 0,
			freqHours: ptr(6),
			jitter:    0,
			loc:       utc,
			// slots: 00:00, 06:00, 12:00, 18:00 → first strictly > 07:00 = 12:00
			wantAfter:  time.Date(2026, 1, 15, 11, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 12, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// every_n_hours — 8h interval, anchor 02:00, now at 03:00 → next 10:00
		// ----------------------------------------------------------------
		{
			name:    "every_n_hours/8h-anchor2",
			now:     time.Date(2026, 1, 15, 3, 0, 0, 0, utc),
			cadence: CadenceEveryNHours,
			hour:    2, minute: 0,
			freqHours: ptr(8),
			jitter:    0,
			loc:       utc,
			// anchor=02:00 → slots: 02:00, 10:00, 18:00 → first strictly > 03:00 = 10:00
			wantAfter:  time.Date(2026, 1, 15, 9, 59, 0, 0, utc),
			wantBefore: time.Date(2026, 1, 15, 10, 1, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// DST spring-forward: America/New_York 2026-03-08 at 02:00 clocks spring
		// forward to 03:00. A daily schedule at 02:30 NYC: Go normalises the
		// non-existent time to 03:30 EDT = 07:30 UTC.
		// now = 06:00 UTC (01:00 EST) → 02:30 NYC is still in the future that day.
		// ----------------------------------------------------------------
		{
			name:    "dst/spring-forward-daily",
			now:     time.Date(2026, 3, 8, 6, 0, 0, 0, utc), // 01:00 EST
			cadence: CadenceDaily,
			hour:    2, minute: 30,
			jitter: 0,
			loc:    nyc,
			// result must be after now and before midnight UTC on Mar 9
			wantAfter:  time.Date(2026, 3, 8, 6, 0, 0, 0, utc),
			wantBefore: time.Date(2026, 3, 9, 5, 0, 0, 0, utc),
		},
		// ----------------------------------------------------------------
		// DST fall-back: America/New_York 2026-11-01 02:00 clocks fall back.
		// Schedule at 01:30 NYC, now = 04:00 UTC (midnight EDT).
		// 01:30 EST = 06:30 UTC; result must be after now and on Nov 1.
		// ----------------------------------------------------------------
		{
			name:    "dst/fall-back-daily",
			now:     time.Date(2026, 11, 1, 4, 0, 0, 0, utc), // midnight EDT
			cadence: CadenceDaily,
			hour:    1, minute: 30,
			jitter:     0,
			loc:        nyc,
			wantAfter:  time.Date(2026, 11, 1, 4, 0, 0, 0, utc),
			wantBefore: time.Date(2026, 11, 2, 5, 0, 0, 0, utc),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := nextOccurrence(c.now, c.cadence, c.hour, c.minute, c.dow, c.dom, c.freqHours, c.jitter, c.loc)

			// Must be strictly after now.
			if !got.After(c.now) {
				t.Errorf("got %v is not strictly after now %v", got, c.now)
			}

			// Must be in UTC.
			if got.Location() != time.UTC {
				t.Errorf("got %v is not in UTC (location: %v)", got, got.Location())
			}

			// Must be truncated to the minute.
			if got.Second() != 0 || got.Nanosecond() != 0 {
				t.Errorf("got %v has sub-minute component (sec=%d ns=%d)", got, got.Second(), got.Nanosecond())
			}

			// Range checks.
			if !(c.wantAfter.IsZero()) && !got.After(c.wantAfter) {
				t.Errorf("got %v is not after lower bound %v", got, c.wantAfter)
			}
			if !(c.wantBefore.IsZero()) && !got.Before(c.wantBefore) {
				t.Errorf("got %v is not before upper bound %v", got, c.wantBefore)
			}
		})
	}
}

// TestResolveLocation checks the IANA, fixed-offset, and UTC fallback paths.
func TestResolveLocation(t *testing.T) {
	t.Run("IANA name", func(t *testing.T) {
		loc := resolveLocation("America/New_York", 0)
		if loc == nil {
			t.Fatal("got nil location")
		}
		if loc.String() != "America/New_York" {
			t.Errorf("got %q, want America/New_York", loc.String())
		}
	})

	t.Run("fixed offset +5:30", func(t *testing.T) {
		loc := resolveLocation("", 5.5)
		if loc == nil {
			t.Fatal("got nil location")
		}
		_, offset := time.Now().In(loc).Zone()
		if offset != 19800 {
			t.Errorf("got offset %d, want 19800 (+05:30)", offset)
		}
	})

	t.Run("UTC fallback on empty name and zero offset", func(t *testing.T) {
		loc := resolveLocation("", 0)
		if loc != time.UTC {
			t.Errorf("got %v, want UTC", loc)
		}
	})

	t.Run("UTC fallback on bad IANA name and zero offset", func(t *testing.T) {
		loc := resolveLocation("Not/A/Real/Zone", 0)
		if loc != time.UTC {
			t.Errorf("got %v, want UTC", loc)
		}
	})

	t.Run("fixed offset for bad IANA name with non-zero offset", func(t *testing.T) {
		loc := resolveLocation("Not/A/Real/Zone", -5)
		if loc == nil {
			t.Fatal("got nil location")
		}
		_, offset := time.Now().In(loc).Zone()
		if offset != -5*3600 {
			t.Errorf("got offset %d, want %d", offset, -5*3600)
		}
	})
}

// TestSiteJitter ensures the jitter is stable, deterministic and within [0,15].
func TestSiteJitter(t *testing.T) {
	ids := []uuid.UUID{
		uuid.MustParse("00000000-0000-0000-0000-000000000000"),
		uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff"),
		uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
	}
	for _, id := range ids {
		id := id
		t.Run(id.String(), func(t *testing.T) {
			got := SiteJitter(id)
			if got < 0 || got > 15 {
				t.Errorf("SiteJitter(%s) = %d, want [0,15]", id, got)
			}
			// Stability: calling twice gives same result.
			if got2 := SiteJitter(id); got != got2 {
				t.Errorf("SiteJitter not stable: %d != %d", got, got2)
			}
		})
	}
}
