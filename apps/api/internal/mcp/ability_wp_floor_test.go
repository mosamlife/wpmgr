package mcp

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// N2: the WordPress version must be the whole string; trailing text or a
// trailing newline does not pass. A pre-release or development build is not a
// release, whatever number it starts with. This runs the real classification,
// so it proves the tools decide with the shared rule, and the only thing that
// varies between cases is the stored WordPress version.
func TestVendorReadWPFloor_Anchored(t *testing.T) {
	siteID := uuid.New()
	for v, runs := range map[string]bool{
		"7.1": true, "7.1.2": true, "8.0": true, "7.0.9": false,
		"7.1 x": false, "7.1\n": false, "\n7.1": false, " 7.1": false, "": false,
		"7.1-RC1": false, "7.1-beta2-59000": false, "7.1.1-src": false, "8.0-alpha-59000-src": false,
	} {
		t.Run(v, func(t *testing.T) {
			e, inv := vendorEntry(), vendorInventory(siteID)
			c := classify(e.Name, []sqlc.AbilityCatalogue{e}, &inv, vendorAgent, v)
			got := ""
			if c.reason != nil {
				got = *c.reason
			}
			want := notRunnableWPTooOld
			if runs {
				want = ""
			}
			if got != want || c.runnable != runs {
				t.Fatalf("%q: reason=%q runnable=%v, want reason=%q runnable=%v", v, got, c.runnable, want, runs)
			}
		})
	}
}
