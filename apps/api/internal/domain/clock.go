package domain

import "time"

// Clock abstracts the current time so services are testable without sleeping
// or depending on the wall clock.
type Clock interface {
	Now() time.Time
}

// SystemClock is a Clock backed by time.Now.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock is a Clock that always returns a fixed time (for tests).
type FixedClock struct{ T time.Time }

// Now returns the fixed time.
func (c FixedClock) Now() time.Time { return c.T }
