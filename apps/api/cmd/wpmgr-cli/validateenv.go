package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// validateEnv loads the configuration and runs the same aggregated checks the
// server runs at boot. It prints a checklist to out (one line per finding:
// OK/FAIL/WARN + env-var name + reason) and returns a non-nil error if any
// BOOT-BLOCKING check failed, so the caller can exit non-zero.
//
// Two severities, matching exactly what the server does with them:
//
//	FAIL  config.Validate: the server parks in degraded boot. Exit code 1.
//	WARN  config.Advisories: the named feature is switched off and the rest of
//	      the control plane serves normally. Exit code 0.
//
// Reporting both is the whole contract: this command exists to tell an operator
// what their configuration will actually do, so it prints every finding either
// function raises, not a curated subset. Keeping the exit code tied to FAIL
// alone is the other half of it: a warning that fails a deployment pipeline
// teaches operators to stop reading warnings.
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

	// EVERY OTHER BOOT-BLOCKING ISSUE, printed too.
	//
	// This loop is the point of the command. The fixed list above was once the
	// whole of it, so any issue outside those three names, a partially
	// configured payment provider for instance, was dropped on the floor: the
	// command printed three OK lines and exited zero while the server would
	// refuse to leave degraded mode on the same configuration. A validator that
	// disagrees with the thing it validates is worse than no validator, because
	// the operator now has a reason to stop looking.
	//
	// Sorted by name so the output is stable between runs.
	printSorted(out, "FAIL", issues, shown)

	// ADVISORIES. Not failures: each one names a feature that is switched off,
	// on an install that otherwise runs normally. They are printed for the same
	// reason they are logged at boot, because the alternative for a
	// half-configured feature is silence.
	advisories := config.Advisories(cfg)
	printSorted(out, "WARN", advisories, nil)

	// Exit non-zero on boot-blocking issues ONLY.
	if len(issues) > 0 {
		return fmt.Errorf("one or more config checks failed")
	}
	return nil
}

// printSorted writes one "<level>  NAME: reason" line per issue, in name order,
// skipping any name already reported by the fixed checklist.
func printSorted(out io.Writer, level string, issues []config.Issue, skip map[string]bool) {
	reasons := make(map[string]string, len(issues))
	names := make([]string, 0, len(issues))
	for _, iss := range issues {
		if skip[iss.Name] {
			continue
		}
		if _, dup := reasons[iss.Name]; dup {
			// One variable can draw two advisories (an unusable public base URL
			// that is ALSO plain http in production). Keep the first: they are
			// printed in the order the checks run, most specific first.
			continue
		}
		reasons[iss.Name] = iss.Reason
		names = append(names, iss.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "%s  %s: %s\n", level, name, reasons[name])
	}
}
