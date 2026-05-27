// Command wpmgr-cli is the in-tree administrative CLI for the control plane.
// It currently supports applying database migrations; key generation and seed
// commands land in later phases.
//
// Usage:
//
//	wpmgr-cli migrate    # apply embedded versioned migrations
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
	fmt.Fprintln(os.Stderr, "  migrate    apply database migrations")
}
