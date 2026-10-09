package aipolicy

// The transaction lock keys that order a change to a setting against the
// decisions and reservations that rely on it. Each is taken with
// pg_advisory_xact_lock(hashtext(key), hashtext(id)).
//
// Order: PolicyTenantLockKey, then AbilitySiteDispatchLockKey. Nothing takes
// PolicyTenantLockKey while holding AbilitySiteDispatchLockKey.
const (
	// PolicyTenantLockKey, keyed on the tenant id, is held by every automatic
	// approval in the tenant and by every write to a site's mode or a
	// connection's switch, so a decision never reads a setting that is
	// changing.
	PolicyTenantLockKey = "assistant_policy_tenant"
	// AbilitySiteDispatchLockKey, keyed on the site id, is held by the
	// reservation that sends an approved site change while it re-checks the
	// setting that approved it. A setting write takes it after
	// PolicyTenantLockKey, so it waits for a reservation in flight, and the
	// next reservation reads the new setting.
	AbilitySiteDispatchLockKey = "assistant_ability_site_dispatch"
)
