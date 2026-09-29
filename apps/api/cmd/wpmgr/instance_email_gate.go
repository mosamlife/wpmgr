package main

import (
	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// newInstanceEmailGate builds the gate behind the instance SMTP settings from
// the loaded config. The install-owner arm is live only when WPMGR_HOSTED is
// not true, so the hosted flag is read here, in one place that
// TestNewInstanceEmailGate_FollowsHostedFlag pins.
func newInstanceEmailGate(pool *db.Pool, cfg config.Config) admingate.InstanceEmailPoolStore {
	return admingate.NewInstanceEmailPoolStore(pool, cfg.Hosted.Enabled)
}
