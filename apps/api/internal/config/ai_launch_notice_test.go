package config

import (
	"os"
	"testing"
)

// TestLoadAILaunchNotice proves WPMGR_AI_LAUNCH_NOTICE reaches the config
// through Load, and what each value means: unset, empty or on sends the
// notice, off holds it, and any other value holds it and is named by
// Advisories. No value ever stops the boot.
//
// Mutation: drop the ai_launch_notice case in mapEnvKey; Load then ignores
// the variable and "off" sends the notice.
func TestLoadAILaunchNotice(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		value    string
		on       bool
		advisory bool
	}{
		{name: "unset", on: true},
		{name: "empty", set: true, value: "", on: true},
		{name: "on", set: true, value: "on", on: true},
		{name: "on in capitals with space", set: true, value: " ON ", on: true},
		{name: "off", set: true, value: "off", on: false},
		{name: "off with a capital", set: true, value: "Off", on: false},
		{name: "false holds", set: true, value: "false", on: false, advisory: true},
		{name: "true holds", set: true, value: "true", on: false, advisory: true},
		{name: "a typo holds", set: true, value: "of", on: false, advisory: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// t.Setenv restores the process value afterwards, also for the
			// unset case, which removes the variable for this test only.
			t.Setenv("WPMGR_AI_LAUNCH_NOTICE", c.value)
			if !c.set {
				if err := os.Unsetenv("WPMGR_AI_LAUNCH_NOTICE"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.AI.LaunchNoticeOn(); got != c.on {
				t.Fatalf("LaunchNoticeOn() = %v with the variable %q (set %v), want %v", got, c.value, c.set, c.on)
			}
			advised := false
			for _, is := range Advisories(cfg) {
				if is.Name == "WPMGR_AI_LAUNCH_NOTICE" {
					advised = true
				}
			}
			if advised != c.advisory {
				t.Fatalf("advisory for %q = %v, want %v", c.value, advised, c.advisory)
			}
			for _, is := range Validate(cfg) {
				if is.Name == "WPMGR_AI_LAUNCH_NOTICE" {
					t.Fatalf("Validate refuses the boot over %q: %+v; only the notice may wait", c.value, is)
				}
			}
		})
	}
}
