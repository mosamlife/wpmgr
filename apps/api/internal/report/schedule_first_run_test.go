package report

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Saving an enabled schedule computed its first next_run_at in UTC, while the
// scanner computes every later run in the client's timezone, so the first
// report went out at the wrong hour for any client not in UTC.

func firstNextRun(t *testing.T, tz, cadence string, day, hour int, enabled bool) *time.Time {
	t.Helper()
	var saved UpsertScheduleInput
	repo := &stubRepo{
		getClientTimezone: func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return tz, nil },
		upsertSchedule: func(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
			saved = in
			return Schedule{}, nil
		},
	}
	_, err := NewService(repo, nil).UpsertSchedule(context.Background(), UpsertScheduleInput{
		TenantID: uuid.New(), ClientID: uuid.New(), Enabled: enabled, Cadence: cadence, SendDay: day, SendHour: hour,
	})
	if err != nil {
		t.Fatalf("UpsertSchedule: %v", err)
	}
	return saved.NextRunAt
}

func TestFirstNextRunIsAtTheSendHourInTheClientsTimezone(t *testing.T) {
	cases := []struct {
		tz, cadence string
		day, hour   int
	}{
		{"Asia/Kolkata", "monthly", 15, 8},
		{"America/Los_Angeles", "monthly", 15, 8},
		{"Pacific/Auckland", "monthly", 3, 23},
		{"Asia/Kolkata", "weekly", 2, 9},
		{"America/Los_Angeles", "weekly", 0, 0},
		{"UTC", "monthly", 15, 8},
		{"", "monthly", 15, 8},
	}
	for _, c := range cases {
		t.Run(c.tz+"/"+c.cadence, func(t *testing.T) {
			next := firstNextRun(t, c.tz, c.cadence, c.day, c.hour, true)
			if next == nil {
				t.Fatal("an enabled schedule got no next_run_at")
			}
			loc, err := time.LoadLocation(c.tz)
			if err != nil {
				t.Fatal(err)
			}
			local := next.In(loc)
			if local.Hour() != c.hour || local.Minute() != 0 {
				t.Errorf("first run is %s in %q, want %02d:00", local.Format("Mon 2 Jan 15:04"), c.tz, c.hour)
			}
			if c.cadence == "monthly" && local.Day() != c.day {
				t.Errorf("first run is on day %d of the month in %q, want %d", local.Day(), c.tz, c.day)
			}
			if c.cadence == "weekly" && int(local.Weekday()) != c.day {
				t.Errorf("first run is on %s in %q, want weekday %d", local.Weekday(), c.tz, c.day)
			}
			if !next.After(time.Now()) {
				t.Errorf("first run %s is not in the future", next)
			}
			// The scanner computes the run after this one with the same function.
			want := nextRunAt(c.cadence, c.day, c.hour, c.tz)
			if !next.Equal(*want) {
				t.Errorf("first run %s differs from the scanner's computation %s", next, want)
			}
		})
	}
}

func TestFirstNextRunIsClearedWhenDisabled(t *testing.T) {
	if next := firstNextRun(t, "Asia/Kolkata", "monthly", 15, 8, false); next != nil {
		t.Errorf("a disabled schedule got next_run_at %s", next)
	}
}

// A timezone name the host cannot load falls back to UTC, as the scanner does,
// and the save still goes through.
func TestFirstNextRunFallsBackToUTCForAnUnknownTimezone(t *testing.T) {
	next := firstNextRun(t, "Mars/Olympus_Mons", "monthly", 15, 8, true)
	if next == nil || next.UTC().Hour() != 8 {
		t.Errorf("first run %v, want 08:00 UTC", next)
	}
}
