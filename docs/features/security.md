# Security

Per-site hardening, login protection, and scanning for a managed WordPress
site. Every control here lives on the site's **Security** tab, organized as
cards: **Login & Two-Factor**, **Password policy**, **Hardening**,
**File integrity**, **Bans & login protection**, **Hide login**, and
**Vulnerabilities**.

For securing the WPMgr *dashboard itself* (your operator account), see
[2fa.md](./2fa.md); that is a separate feature from everything on this page.

---

## Login protection and bans

**What it does.** A sliding-window brute-force gate on the site's own
WordPress login (`wp-login.php`), with three escalating tiers per IP within
a configurable look-back window:

1. **Captcha-gate threshold** (default: 3 failures). Flags the IP as
   needing a challenge.
2. **Temporary per-IP block** (default: 10 failures). Blocks that IP for
   the window.
3. **Site-wide block** (default: 100 failures across all IPs). The most
   severe tier; takes precedence over the per-IP tiers when tripped.

A successful login from an IP within the recent window (a "known-good
bypass") skips all three checks so a legitimate admin who mistyped a
password a few times isn't locked out behind their own recent success.
Private, loopback, and link-local IPs are always bypassed. An explicit
allow-CIDR list always wins over everything else.

**Default state.** Off (`disabled`). Configuring thresholds does nothing
until you pick a mode.

**Modes.**

| Mode | Behavior |
|---|---|
| `disabled` | No hooks registered, nothing recorded. |
| `audit` | Every attempt is recorded and a block *would* fire, but it never actually blocks. Useful for tuning thresholds before enforcing them. |
| `protect` | Recorded and enforced: a blocked IP gets a 403 page. |

**Where to configure.** Security → **Bans & login protection** card, "Login
protection" section. Thresholds, the look-back window per tier, the IP
header used to resolve the client address (for sites behind a proxy or
CDN), and allow/deny CIDR lists are all editable there.

**How bans propagate.** Login-protection deny/allow CIDRs and the separate,
operator-entered ban list (IP, CIDR range, or user-agent pattern, in the
same card under "Blocked IPs and user agents") are enforced at two layers:
a PHP filter on WordPress's `authenticate` hook (runs after normal password
checks and after typical third-party 2FA plugins), and an early-boot
must-use plugin (see IP firewall below) that can block a request before
WordPress itself finishes loading. Operator-entered bans (from the ban
list) are enforced in **every** mode, even `disabled`/`audit`; an explicit
ban is never conditional on brute-force mode.

**Manual unblock.** From the Login events list, unblocking an IP deletes
only its recorded *failures*, resetting its sliding-window counter to
zero; successful-login rows are kept, so the known-good bypass still works
for that IP going forward.

**The mu-plugin note.** The IP-deny gate that enforces bans (see below)
runs as a WordPress must-use plugin, which loads before any regular plugin,
including before WPMgr's own agent plugin fully boots. This is what lets a
ban take effect even if something else on the site misbehaves during
normal plugin load. The must-use plugin fails open on any database error,
so a transient DB hiccup never turns into a site-wide lockout.

**Lockout safety rail.** Turning on `protect` mode with an empty allow-list
automatically adds your own current IP as a `/32` (or `/128` for IPv6)
allow-listed address, so enabling protection can't immediately lock out the
person turning it on.

**Gotcha.** Login protection currently returns a static 403 page in
`protect` mode; there is no interactive CAPTCHA-solve flow yet, despite the
"captcha gate" tier name. That tier currently behaves the same as the
temporary block, just at a lower threshold.

---

## IP firewall (WAF)

**What it does.** An early-boot, must-use plugin (loads before any other
plugin, including third-party ones) that evaluates two independent CIDR
deny lists on every request, before WordPress finishes booting:

1. **Operator hardening bans**: IP/range entries added under Hardening's
   ban list, enforced in every mode, always.
2. **Login-protection deny list**: only enforced while login protection is
   set to `protect` mode.

Allow-listed CIDRs and private/loopback/link-local addresses always bypass
both layers, checked first. A request that matches a deny rule gets a
plain 403 with no-cache headers; a database read failure at this early
stage never blocks a real request (fails open).

**Default state.** Opt-in, inert until you add a ban entry or turn on
login-protection `protect` mode.

**Where to configure.** Security → **Bans & login protection** →
"Blocked IPs and user agents." Add an IP, a CIDR range, or a user-agent
substring pattern, each with an optional comment for your own reference.

**What it blocks.** IP and CIDR-range bans are enforced both at this
early-boot layer and, for user-agent bans, in a PHP fallback that runs on
`init` (covers hosts where the server-config write below wasn't possible).
On Apache/LiteSpeed, hardening also writes IP-range and user-agent denies
directly into the managed `.htaccess` block for a faster, PHP-free block;
nginx sites rely on the PHP-layer enforcement only.

---

## Hardening toggles

**What it does.** A set of independent WordPress hardening switches. Every
toggle defaults to **off**; turning one on only changes the behavior it
names.

| Toggle | Effect | Default |
|---|---|:---:|
| Disable file editor | Adds `DISALLOW_FILE_EDIT` to `wp-config.php` (blocks the Plugin/Theme editor in wp-admin), with a runtime permission filter as a fallback if the config file isn't writable. | off |
| XML-RPC mode | `on` (default WordPress behavior), `limited` (disables `system.multicall` and pingback methods, the two most-abused for amplification/DDoS), or `off` (disables XML-RPC entirely). | on |
| Restrict REST API | `default` (WordPress's normal behavior) or `restricted` (anonymous REST requests are rejected except for oEmbed and the WPMgr agent's own routes). | default |
| Restrict login identifier | `both` (username or email, default), `username` only, or `email` only. | both |
| Force unique nickname | Blocks a user from setting their public display name equal to their login username (defeats a common username-harvesting trick). | off |
| Disable author-archive enumeration | 404s `?author=N` probing redirects and hides the WordPress REST users list from anonymous requests. | off |
| Force SSL | Redirects HTTP to HTTPS, sets `FORCE_SSL_ADMIN`, and sends an HSTS header on every HTTPS response. **Caveat: this is opt-in and off by default.** Enabling it on a site that doesn't actually have a valid TLS certificate configured will break access to it, since the redirect fires unconditionally (except for WP-Cron and CLI requests). Confirm HTTPS actually works on the site before turning this on. | off |
| Disable directory browsing | Server-config `Options -Indexes` (Apache/LiteSpeed only). | off |
| Disable PHP execution in uploads | Blocks PHP execution inside the uploads directory at the server-config layer, closing a common upload-then-execute path. | off |
| Protect system files | Blocks web access to `wp-config.php`, `.htaccess`, and similar files at the server-config layer. | off |

**Where to configure.** Security → **Hardening** card.

**Gotcha.** The four server-config-backed toggles (directory browsing, PHP
in uploads, protect system files, and the IP/user-agent ban rules) only
write to `.htaccess` on Apache/LiteSpeed. On nginx there is no
server-config write; only the PHP-layer enforcement applies (weaker, since
it runs after WordPress has already started loading for that request).

---

## Hide login

**What it does.** Moves the login page to a secret slug you choose. A
request to the real `/wp-login.php` or `/wp-admin` (while logged out and
without the access cookie) gets a 404 or an optional redirect instead of
the login form; a request to your chosen slug is served the real login
form in place, and sets a short-lived signed access cookie so the rest of
the login flow (lost password, registration, logout) keeps working without
re-visiting the slug each time.

**Default state.** Off. Requires a slug before it can be enabled.

**Where to configure.** Security → **Hide login** card. Set a slug, and
optionally a redirect URL for blocked requests (a plain 404 is served if
no redirect is set).

**Admin-ajax exclusion (as of v0.61.59).** `admin-ajax.php` and
`admin-post.php` live under `/wp-admin/` but are legitimate logged-out
front-end endpoints many themes and page builders rely on for public forms
and AJAX. Hide login explicitly excludes both from the block; only the
actual login page and wp-admin dashboard are hidden. Earlier versions 404'd
these too, which broke front-end forms on affected sites. If you're
reporting a front-end AJAX failure on a site with hide-login enabled,
confirm you're on 0.61.59 or later.

**Lockout-proofing.** Logged-in users are never redirected or blocked. The
agent's own REST routes and the autologin path always bypass the gate
regardless of slug state, so a misconfigured slug can't lock the control
plane out of the site. A `WPMGR_DISABLE_HIDE_BACKEND` constant in
`wp-config.php` disables the feature entirely as a last-resort recovery
path if you get locked out of your own custom slug.

---

## File integrity scans

**What it does.** Compares files on disk against known-good hashes and
reports differences.

**Scan scopes.**

| Scope | What it checks |
|---|---|
| Core files | WordPress core files only, MD5-diffed against the official WordPress.org Checksums API for the site's exact version and locale. |
| All files (wp-content) | Everything under `wp-content`: added, changed, and removed files, diffed against a per-site learned baseline. |
| Full install | Core plus `wp-content` in one run. |

**Sources of truth.** Core files are compared against WordPress.org's
public checksums API (cached: 30 days for a known version, 6 hours
negative-cached on a lookup miss, so repeated fleet scans don't hammer the
public API). Files inside a plugin or theme that's hosted on WordPress.org
are similarly diffed against that plugin or theme's published checksums
when available. Everything else (custom code, premium plugins/themes,
uploads) is compared against a **learned baseline**: the last-known-good
hash per file, recorded the first time a scan sees it and updated on each
subsequent clean scan.

**Finding types** include core file modified/missing, an unexpected file
injected into a core path, a file added/changed/removed relative to the
baseline, and a wp.org-hosted plugin/theme file that doesn't match its
published checksum. Only core-path findings are treated as high severity
by default; operator-writable areas (`wp-content/`, `wp-config.php`, cache
drop-ins) are allow-listed on the core scan to cut down false positives.

**Review/accept flow.** Each finding can be marked **ignored** from the
Security → **File integrity** card. This is an audited action, not a
silent dismiss, and the finding stays visible (marked ignored) rather than
being deleted. You can also fetch the actual file content for a stored
finding directly from the dashboard (server-gated: only content already
captured by the scan, and every fetch is audited) without needing shell
access to inspect what changed.

**Where to configure/run.** Security → **File integrity** card →
**Run scan**, choose a scope from the dropdown.

**Gotcha.** The scan engine only detects file-level differences against
checksums or a learned baseline; it does not do signature-based or
heuristic malware detection on file *content*. A malicious file that
matches no known-good hash will show up as `file_added`/`plugin_unknown`,
but a file that was already present at the time the baseline was learned
will not retroactively flag as suspicious.

---

## Site-user two-factor and password policy

These control the login requirements for **the managed WordPress site's own
users** (its wp-admin accounts), completely separate from the dashboard
operator 2FA covered in [2fa.md](./2fa.md), which protects *your WPMgr
account*, not the WordPress site.

### Two-factor

**What it does.** After a site user's primary password check succeeds, an
interstitial second-factor step can be required before a real session is
issued. Supported methods: TOTP authenticator app, email one-time code, and
backup/recovery codes. Enrollment happens on the WordPress site itself (QR
code plus confirmation, or a setup wizard on first required login); this
dashboard only turns the requirement on/off and chooses which roles must
comply.

**Default state.** Off. When on, you choose which roles are required
(for example, only Administrators), a grace-login count before enforcement
kicks in for a not-yet-enrolled required user, and how long a "remember
this device" cookie lasts.

**Where to configure.** Security → **Login & Two-Factor** card.

**Gotchas:**
- Application-password authentication (used by some REST API integrations)
  is rejected outright for any user who requires 2FA or has a non-email
  method enrolled; application passwords have no second factor, so they're
  blocked rather than silently bypassing the requirement.
- The dashboard's own one-click autologin into wp-admin intentionally
  bypasses this site-level 2FA by construction (see
  [ADR-055](../adr/ADR-055-autologin-2fa-bypass.md)); that's exactly why
  dashboard 2FA ([2fa.md](./2fa.md)) matters: it's the actual security
  boundary once autologin is in play.

### Password policy

**What it does.** Enforces requirements when a site user sets or changes
their password (profile update, password reset, or registration):

- **Minimum strength score.** zxcvbn-scored (0 to 4); rejects passwords
  below the chosen minimum.
- **Block compromised passwords.** Checks against a breach corpus via a
  k-anonymity range query (only a 5-character hash prefix ever leaves the
  site; the control plane proxies the lookup). Fails open: if the check
  can't complete, the password is allowed rather than blocking a
  legitimate password change.
- **Block password reuse.** Rejects reusing any of the user's last N
  passwords.
- **Forced password expiry.** After N days, the user's next login is
  intercepted with a mandatory change-password form before they can
  proceed.

**Default state.** Every check is off (score threshold 0, reuse count 0,
max age 0, meaning no expiry) until you set a value.

**Where to configure.** Security → **Password policy** card. Per-role
overrides are also available (for example, a stricter score for
Administrators than for Authors).

---

## Vulnerability scanner

**What it does.** Matches the site's installed WordPress core version,
plugins, and themes against a maintained vulnerability intelligence feed,
and surfaces any match as a finding with severity, affected/fixed version,
and a CVE reference where one exists.

**Default state.** Runs automatically whenever the site's installed-software
inventory changes (for example, after an update) or on the schedule the
control plane maintains for the feed itself; you can also trigger an
on-demand rescan.

**Where to configure.** Security → **Vulnerabilities** card.

**Rescan / dismiss / remediate flow:**
- **Rescan**: re-runs the match against the current feed and the site's
  current inventory.
- **Dismiss**: marks a finding as acknowledged without fixing it (for
  example, a false positive, or a risk you've accepted); dismissed
  findings can be restored back to open later.
- **Remediate**: for a finding with a known fixed version, triggers an
  update run for that plugin/theme/core component directly from the
  finding.

A tenant-wide **Vulnerabilities** view rolls findings up across every site
in the fleet, sorted by severity, for triage without opening each site
individually.

**Gotcha.** The feed only covers software with a published CVE or vendor
advisory. A zero-day, or a vulnerability in code that never gets a public
advisory, will not show up here; this is a detection aid, not a guarantee
of safety. Attribution notices required by the feed's license are rendered
in the dashboard wherever findings are shown.
