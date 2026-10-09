package agentcmd

import "testing"

// The WordPress version is the whole string, exactly as stored: trailing text,
// a trailing newline or surrounding space does not pass, and a pre-release or
// development build is not a release whatever number it starts with.
func TestWPMeetsVendorFloor(t *testing.T) {
	for v, want := range map[string]bool{
		// Releases at or above the floor.
		"7.1": true, "7.1.0": true, "7.1.2": true, "7.1.2.3": true, "7.10": true, "8.0": true, "10.0": true,
		// Releases below it.
		"7.0": false, "7.0.9": false, "6.9": false, "6.9.4": false,
		// Not the whole string.
		"7.1 x": false, "7.1\n": false, "\n7.1": false, " 7.1": false, "7.1 ": false,
		// Malformed or missing.
		"": false, "7": false, "7.": false, ".7.1": false, "7.1.2.3.4": false, "latest": false,
		// A pre-release or development build is not a release.
		"7.1-RC1": false, "7.1-rc2": false, "7.1-beta2": false, "7.1-beta2-59000": false,
		"7.1-alpha-59000": false, "7.1-alpha-59000-src": false, "7.1.1-src": false, "8.0-dev": false,
		"7.1+build": false, "7.1_1": false,
	} {
		if got := WPMeetsVendorFloor(v); got != want {
			t.Errorf("WPMeetsVendorFloor(%q) = %v, want %v", v, got, want)
		}
	}
}

// WPMeetsVendorFloor is WPVersionMeetsFloor at MinWPVersionForVendorReads.
func TestWPMeetsVendorFloorIsTheVendorFloor(t *testing.T) {
	for _, v := range []string{"7.0.9", "7.1", "7.1.1", "7.1-RC1", "7.1.1-src", "", "x"} {
		if got, want := WPMeetsVendorFloor(v), WPVersionMeetsFloor(v, MinWPVersionForVendorReads); got != want {
			t.Errorf("%q: WPMeetsVendorFloor = %v, WPVersionMeetsFloor at the vendor floor = %v", v, got, want)
		}
	}
	if !WPMeetsVendorFloor(MinWPVersionForVendorReads) {
		t.Errorf("the floor itself, %q, must meet the floor", MinWPVersionForVendorReads)
	}
}

func TestWPVersionMeetsFloorUsesTheGivenFloor(t *testing.T) {
	for _, c := range []struct {
		v, floor string
		want     bool
	}{
		{"9.9", "9.9", true},
		{"9.9.1", "9.9", true},
		{"9.8.9", "9.9", false},
		{"7.1", "9.9", false},
		{"9.9-RC1", "9.9", false},
		{"9.9.1-src", "9.9", false},
		{"7.1", "6.9", true},
	} {
		if got := WPVersionMeetsFloor(c.v, c.floor); got != c.want {
			t.Errorf("WPVersionMeetsFloor(%q, %q) = %v, want %v", c.v, c.floor, got, c.want)
		}
	}
}
