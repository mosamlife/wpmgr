package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the S2
// Login Protection + IP store feature. The wp-agent-engineer mirrors these
// shapes in the agent's class-admin.php / class-schema.php command handlers.
// Field names are JSON wire names; do not rename without updating both sides.
//
// Transport — sync_security_config:
//   POST {site_url}/wp-json/wpmgr/v1/command/sync_security_config
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="sync_security_config", aud=<siteId>
//   Body:   application/json — SecurityConfigRequest below.
//   Response: 200 with SecurityConfigResult; ok=false = semantic failure.
//
// Transport — unblock_ip:
//   POST {site_url}/wp-json/wpmgr/v1/command/unblock_ip
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="unblock_ip", aud=<siteId>
//   Body:   application/json — UnblockIPRequest below.
//   Response: 200 with UnblockIPResult; ok=false = semantic failure.

// SecurityThresholds holds the per-site brute-force thresholds the agent
// applies to decide when to challenge, temporarily block, or permanently block
// an IP. All values are integers; zero uses the agent's compiled-in default.
//
//	captcha_limit      — failures before a CAPTCHA challenge is shown.
//	temp_block_limit   — failures before a temporary block.
//	block_all_limit    — failures before a permanent block.
//	failed_login_gap   — seconds of inactivity that resets the failure counter.
//	success_login_gap  — seconds after a success before a new failure series starts.
//	all_blocked_gap    — seconds a permanent block remains active.
type SecurityThresholds struct {
	CaptchaLimit    int `json:"captcha_limit"`
	TempBlockLimit  int `json:"temp_block_limit"`
	BlockAllLimit   int `json:"block_all_limit"`
	FailedLoginGap  int `json:"failed_login_gap"`
	SuccessLoginGap int `json:"success_login_gap"`
	AllBlockedGap   int `json:"all_blocked_gap"`
}

// DefaultSecurityThresholds are the compiled-in defaults matching the agent's
// built-in values (also the DB column default for the thresholds JSONB column).
var DefaultSecurityThresholds = SecurityThresholds{
	CaptchaLimit:    3,
	TempBlockLimit:  10,
	BlockAllLimit:   100,
	FailedLoginGap:  1800,
	SuccessLoginGap: 1800,
	AllBlockedGap:   1800,
}

// SecurityConfigRequest is the POST body for the `sync_security_config` command.
//
//	mode        "disabled" | "audit" | "protect"
//	thresholds  see SecurityThresholds above.
//	ip_header   HTTP header the agent reads for the real client IP
//	            (e.g. "REMOTE_ADDR", "HTTP_X_FORWARDED_FOR").
//	allow_cidrs CIDRs always allowed (bypass all checks). Empty = no allowlist.
//	deny_cidrs  CIDRs always denied (before threshold evaluation). Empty = none.
type SecurityConfigRequest struct {
	Mode       string             `json:"mode"`
	Thresholds SecurityThresholds `json:"thresholds"`
	IPHeader   string             `json:"ip_header"`
	AllowCIDRs []string           `json:"allow_cidrs"`
	DenyCIDRs  []string           `json:"deny_cidrs"`
}

// SecurityConfigResult is the agent's response to the `sync_security_config`
// command.
//
//	ok      whether the agent successfully applied the new config.
//	detail  short human-readable note ("applied", or an error message when
//	        ok=false). The CP treats ok=false as an application-level error with
//	        this message, even though the HTTP status is 200.
type SecurityConfigResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// UnblockIPRequest is the POST body for the `unblock_ip` command.
//
//	ip  the IPv4 or IPv6 address to unblock (dotted-decimal or RFC 5952).
type UnblockIPRequest struct {
	IP string `json:"ip"`
}

// UnblockIPResult is the agent's response to the `unblock_ip` command.
//
//	ok      whether the agent successfully removed the block for the IP.
//	detail  short human-readable note.
type UnblockIPResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
