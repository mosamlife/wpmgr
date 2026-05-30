package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the M14
// Login Whitelabel feature. The wp-agent-engineer mirrors this shape in the
// agent's command handler for the sync_login_brand route. Field names are JSON
// wire names; do not rename without updating both sides.
//
// Transport: POST {site_url}/wp-json/wpmgr/v1/command/sync_login_brand
//   Header:  Authorization: Bearer <minted EdDSA JWT>
//            cmd="sync_login_brand", aud=<siteId>
//   Body:    application/json — LoginBrandRequest below.
//   Response: 200 with LoginBrandResult; ok=false = semantic failure.
//            Non-2xx is a transport error; ok=false at status 200 is an
//            application-level failure (agent rejected the config).

// LoginBrandRequest is the POST body for the `sync_login_brand` command.
//
//	logo_url   Full URL of the image shown on the WP login page.
//	           "" = no override (WordPress default logo).
//	logo_link  URL the logo links to. "" = no override.
//	message    Text shown below the logo. "" = no override. Max 2000 chars
//	           enforced at the CP layer before the command is sent.
type LoginBrandRequest struct {
	LogoURL  string `json:"logo_url"`
	LogoLink string `json:"logo_link"`
	Message  string `json:"message"`
}

// LoginBrandResult is the agent's response to the `sync_login_brand` command.
//
//	ok      whether the agent successfully applied the branding config.
//	detail  short human-readable note ("applied", or an error description when
//	        ok=false). The CP treats ok=false as an application-level error with
//	        this message, even though the HTTP status is 200.
type LoginBrandResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
