package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the S1.2
// PHP-error ignore-list + error-level config feature. The wp-agent-engineer
// mirrors this shape in apps/agent/includes/commands/class-metadata-command.php
// (sync_error_config route). Field names are JSON wire names; do not rename
// without updating both sides.
//
// Transport: POST {site_url}/wp-json/wpmgr/v1/command/sync_error_config
//   Header:  Authorization: Bearer <minted EdDSA JWT>
//            cmd="sync_error_config", aud=<siteId>
//   Body:    application/json — ErrorConfigRequest below.
//   Response: 200 with ErrorConfigResult below; ok=false means the agent
//             rejected or could not apply the config (non-2xx is a transport
//             error; ok=false at status 200 is a semantic failure).

// DefaultErrorLevel is the PHP E_* bitmask WordPress uses by default:
// E_ALL & ~E_STRICT = 6143. Returned when no per-site row exists yet.
const DefaultErrorLevel = 6143

// ErrorConfigRequest is the POST body for the `sync_error_config` command.
//
//	error_level   PHP E_* bitmask to apply (>0, fits int32). Agent writes this
//	              to the WP error-reporting level so the agent's collector only
//	              captures errors at/above the configured mask.
//	ignore_md5s   Ordered list of 32-character lowercase hex md5 fingerprints
//	              the agent must suppress (not count, not report) going forward.
//	              An empty list clears all suppression. The list is the full
//	              canonical ignore-set; the agent replaces its local list
//	              atomically on each sync.
type ErrorConfigRequest struct {
	ErrorLevel int      `json:"error_level"`
	IgnoreMD5s []string `json:"ignore_md5s"`
}

// ErrorConfigResult is the agent's response to the `sync_error_config` command.
//
//	ok      whether the agent successfully applied the new config.
//	detail  short human-readable note ("applied", or an error description when
//	        ok=false). The CP treats ok=false as an application-level error with
//	        this message, even though the HTTP status is 200.
type ErrorConfigResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
