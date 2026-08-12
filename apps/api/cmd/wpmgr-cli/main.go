// Command wpmgr-cli is the in-tree administrative CLI for the control plane.
//
// Usage:
//
//	wpmgr-cli migrate        # apply embedded versioned migrations
//	wpmgr-cli gen-secrets    # mint the boot-critical secrets (KEY=VALUE lines)
//	wpmgr-cli validate-env   # check the environment config and list every problem at once
//	wpmgr-cli reclaim ...    # object-storage reclamation (see reclaim.go)
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "migrate":
		if err := migrate(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied")
	case "gen-secrets":
		if err := genSecrets(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "gen-secrets:", err)
			os.Exit(1)
		}
	case "validate-env":
		if err := validateEnv(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "validate-env:", err)
			os.Exit(1)
		}
	case "reclaim":
		// Exits NON-ZERO when it changed nothing. That is the property that makes
		// this a recovery path (GH #408): the remedies it replaces reported
		// success while RLS silently discarded the statement.
		if err := reclaimCmd(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "reclaim:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func migrate() error {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Migrations run with the owner/superuser DSN (falls back to the app DSN).
	pool, err := db.Connect(ctx, cfg.DB.MigrateDSN())
	if err != nil {
		return err
	}
	defer pool.Close()
	return pool.Migrate(ctx)
}

func usage() {
	fmt.Fprintln(os.Stderr, "wpmgr-cli — admin tooling")
	fmt.Fprintln(os.Stderr, "  migrate        apply database migrations")
	fmt.Fprintln(os.Stderr, "  gen-secrets    mint boot-critical secrets as KEY=VALUE lines")
	fmt.Fprintln(os.Stderr, "  validate-env   check the environment config and list every problem at once")
	fmt.Fprintln(os.Stderr, "  reclaim        object-storage reclamation; run `reclaim` alone for its subcommands")
}
