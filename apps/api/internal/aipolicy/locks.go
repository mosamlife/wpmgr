package aipolicy

// The transaction lock keys that order a change to a setting against the
// decisions and reservations that rely on it. Each is taken with
// pg_advisory_xact_lock(hashtext(key), hashtext(id)).
//
// Order: PolicyTenantLockKey, then AbilitySiteDispatchLockKey. Nothing takes
// PolicyTenantLockKey while holding AbilitySiteDispatchLockKey.
const (
	// PolicyTenantLockKey, keyed on the tenant id, is held by every automatic
	// approval in the tenant, by the reservation that sends a change a
	// setting approved, and by aitrust's writes to a site's mode and a
	// connection's switch. A decision never reads one of those settings
	// while it is changing, and such a write either commits before the
	// reservation re-checks it or waits for that reservation to commit.
	PolicyTenantLockKey = "assistant_policy_tenant"
	// AbilitySiteDispatchLockKey, keyed on the site id, is held by the
	// reservation that sends an approved site change while it re-checks the
	// setting that approved it. aitrust's write to a site's mode takes it
	// after PolicyTenantLockKey, so it waits for a reservation in flight, and
	// the next reservation reads the new mode.
	AbilitySiteDispatchLockKey = "assistant_ability_site_dispatch"
)
