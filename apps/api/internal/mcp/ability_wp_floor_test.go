package mcp

import "testing"

// N2: the WordPress version must be the whole string; trailing text or a
// trailing newline does not pass.
func TestWPMeetsVendorFloor_Anchored(t *testing.T) {
	for v, want := range map[string]bool{
		"7.1": true, "7.1.2": true, "8.0": true, "7.0.9": false,
		"7.1 x": false, "7.1\n": false, "\n7.1": false, " 7.1": false, "7.1-RC1": false, "": false,
	} {
		if got := wpMeetsVendorFloor(v); got != want {
			t.Errorf("%q: got %v, want %v", v, got, want)
		}
	}
}
