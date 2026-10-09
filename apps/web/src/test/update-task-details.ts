// The sentences the control plane writes into an update task, quoted for the
// display tests.
//
// A display test that invents its own detail text proves nothing about what an
// operator reads: the point of these cases is the real sentence, at its real
// length, behind the real chip. `update-task-details.test.ts` holds every
// constant here to the Go source it was copied from, so a reworded sentence
// fails there and names the constant to refresh instead of leaving these tests
// green against prose the server no longer writes.

// apps/api/internal/agentcmd/command_error.go: ForbiddenMessage, with the lead
// "Update not started. " that runApply passes for an apply.
export const FIREWALL_403_DETAIL =
  "Update not started. A firewall or security rule blocked the request (HTTP 403) before it reached the WPMgr agent. " +
  "Ask the site's host to allow requests to /wp-json/wpmgr/v1/ from WPMgr, and allow them in any firewall service or security plugin the site uses.";

// The same function, for a reply whose code is wpmgr_token_expired.
export const CLOCK_403_DETAIL =
  "Update not started. The WPMgr agent refused the request (HTTP 403, wpmgr_token_expired) because the site's server clock differs from WPMgr's, " +
  "and WPMgr's signed requests are valid for less than a minute. Ask the site's host to sync the server time (NTP), then run the update again.";

// What the same refusals leave in the task's error log: CommandError.Error()
// for command "update". The second body is the agent's own refusal,
// WP_Error('wpmgr_' . $code, 'Forbidden.', ['status' => 403]) in
// apps/agent/includes/class-router.php.
export const FIREWALL_403_RAW_ERROR =
  "update command rejected by agent: status 403 body=<html><body><h1>403 Forbidden</h1>Request forbidden by administrative rules.</body></html>";
export const CLOCK_403_RAW_ERROR =
  'update command rejected by agent: status 403 body={"code":"wpmgr_token_expired","message":"Forbidden.","data":{"status":403}}';

// apps/api/internal/update/worker.go: the three core outcomes that are not a
// success. coreRollbackUndeliverableDetail is the only one that says the site
// is down.
export const CORE_ROLLBACK_UNDELIVERABLE_DETAIL =
  "The site is down after the WordPress core update: it answered with a server error or showed a PHP fatal error, " +
  "and the rollback command could not be delivered. Nothing restores WordPress core automatically, so the site needs manual recovery.";

export const CORE_LEFT_AS_IS_DETAIL =
  "WordPress core was updated, but the site did not pass the health check afterwards. " +
  "Core was left as is: an automatic core rollback runs only when the check confirms a crash (an HTTP 500 or WordPress's error screen), " +
  "and this check did not. Check the site.";

export const CORE_NO_CHANGE_UNHEALTHY_DETAIL =
  "WordPress core reported no change, but the site did not pass the health check that followed. " +
  "Nothing was rolled back; check the site.";

// The error log of a task whose rollback could not be delivered:
// CommandError.Error() for command "rollback".
export const ROLLBACK_RAW_ERROR =
  "rollback command rejected by agent: status 500 body=<p>There has been a critical error on this website.</p>";

// A failed post-update health check's own reason, in the shape worker.go writes
// it: the task error of a core task left as is, and the tail of the detail of a
// rolled back task ("rolled back: " + reason).
export const HEALTH_CHECK_FAILED_REASON =
  "post-update health failed after 3 attempt(s): status=503 Service Unavailable";

// worker.go, rollback(): the plugin and theme counterpart. The agent's update
// watchdog exists for these two target types, so for them the sentence is true.
export const PLUGIN_SITE_DOWN_DETAIL =
  "site not responding: site-wide PHP fatal after update; rollback command undeliverable. " +
  "The agent update watchdog will attempt automatic filesystem recovery; if it cannot, manual filesystem recovery is required.";
