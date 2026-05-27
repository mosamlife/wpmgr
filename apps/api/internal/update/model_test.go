package update

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestValidateItemsVersion proves the CP-side version validator accepts safe
// pins/"latest" and rejects argument-injection payloads (the M3 security fix).
func TestValidateItemsVersion(t *testing.T) {
	good := []string{"latest", "6.5.2", "1.0.0", "1.0", "1", "6.5.2-beta1", ""}
	for _, v := range good {
		if err := validateItems([]Item{{Type: TargetPlugin, Slug: "akismet", Version: v}}); err != nil {
			t.Errorf("version %q should be accepted, got %v", v, err)
		}
	}

	bad := []string{
		"1.0 --activate",    // smuggled WP-CLI flag
		"latest --activate", // smuggled flag with latest
		"; rm -rf",          // command separator
		"1.0;rm",            // command separator
		"6.5 && curl evil",  // shell chaining
		"$(id)",             // command substitution
		"`id`",              // backtick substitution
		"--force",           // leading flag
		"1.0 2.0",           // embedded space
		"latest\n",          // newline
		">out",              // redirection
	}
	for _, v := range bad {
		err := validateItems([]Item{{Type: TargetPlugin, Slug: "akismet", Version: v}})
		if err == nil {
			t.Errorf("version %q must be rejected", v)
			continue
		}
		if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindValidation {
			t.Errorf("version %q: want KindValidation (422), got %v", v, err)
		}
	}
}

// TestValidateItemsSlug proves the slug charset guard accepts real slugs and
// rejects spaces, traversal, and shell metacharacters.
func TestValidateItemsSlug(t *testing.T) {
	good := []string{"akismet", "akismet/akismet", "woo-commerce", "My_Plugin.2", "core"}
	for _, s := range good {
		if err := validateItems([]Item{{Type: TargetPlugin, Slug: s, Version: "latest"}}); err != nil {
			t.Errorf("slug %q should be accepted, got %v", s, err)
		}
	}

	bad := []string{
		"",                 // empty
		"../../etc/passwd", // path traversal
		"plugin; rm -rf",   // command separator
		"plug in",          // space
		"a/b/c",            // too many path segments
		"plugin&cmd",       // shell metachar
		"$(id)",            // substitution
	}
	for _, s := range bad {
		err := validateItems([]Item{{Type: TargetPlugin, Slug: s, Version: "latest"}})
		if err == nil {
			t.Errorf("slug %q must be rejected", s)
			continue
		}
		if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindValidation {
			t.Errorf("slug %q: want KindValidation (422), got %v", s, err)
		}
	}
}
