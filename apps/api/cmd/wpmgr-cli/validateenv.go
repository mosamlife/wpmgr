package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// validateEnv loads the configuration and runs the same aggregated checks that
// the server's degraded-boot path runs. It prints a checklist to out (one line
// per check: OK/FAIL + env-var name + reason) and returns a non-nil error if
// ANY check failed so the caller can exit non-zero.
//
// "Same checks" is the whole contract: this command exists to tell an operator
// whether the server will boot, so it reports every issue config.Validate
// raises, not a curated subset of them.
//
// It NEVER opens a database connection, a Redis connection, or an HTTP server —
// this command is safe to run in any environment without network access.
//
// SECRET-LEAK INVARIANT: names and reasons only are printed; secret values,
// DSN strings, and raw crypto errors are never surfaced.
func validateEnv(out io.Writer) error {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	issues := config.Validate(cfg)

	// Build a fast lookup for which names have issues.
	failed := make(map[string]string, len(issues))
	for _, iss := range issues {
		failed[iss.Name] = iss.Reason
	}

	// The always-surfaced checks, in config.Validate order. These print an OK
	// line even when they pass, because "this secret is present and sane" is
	// worth confirming on every run.
	alwaysShown := []string{
		"WPMGR_SESSION_SECRET",
		"WPMGR_AGENT_SIGNING_PRIVATE_KEY",
		"WPMGR_SITE_DEST_AGE_SECRET",
	}
	shown := make(map[string]bool, len(alwaysShown))
	for _, name := range alwaysShown {
		shown[name] = true
		if reason, bad := failed[name]; bad {
			fmt.Fprintf(out, "FAIL  %s: %s\n", name, reason)
		} else {
			fmt.Fprintf(out, "OK    %s\n", name)
		}
	}

	// EVERY OTHER ISSUE, printed too.
	//
	// This loop is the point of the command. The fixed list above was once the
	// whole of it, so any issue outside those three names, a partially
	// configured payment provider, a half-entered social credential, a public
	// base URL that yields a relative OAuth redirect, was dropped on the floor:
	// the command printed three OK lines and exited zero while the server would
	// refuse to leave degraded mode on the same configuration. A validator that
	// disagrees with the thing it validates is worse than no validator, because
	// the operator now has a reason to stop looking.
	//
	// Reasons are sorted by name so the output is stable between runs.
	names := make([]string, 0, len(issues))
	for _, iss := range issues {
		if !shown[iss.Name] {
			names = append(names, iss.Name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "FAIL  %s: %s\n", name, failed[name])
	}

	// Fail on ANY issue, not just the ones on the fixed list.
	if len(issues) > 0 {
		return fmt.Errorf("one or more config checks failed")
	}
	return nil
}
