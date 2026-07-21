=== Fleet Agent Site Manager ===
Contributors: mosamlife
Tags: backup, restore, performance, cache, security
Requires at least: 6.2
Tested up to: 7.0
Requires PHP: 8.1
Stable tag: 0.61.56
License: GPLv2 or later
License URI: https://www.gnu.org/licenses/gpl-2.0.html

Connects this WordPress site to a user-chosen WPMgr control plane for managed backups, performance, security, and updates.

== Description ==

This plugin links your WordPress site to a WPMgr control plane that YOU choose and configure. The default endpoint is none -- the plugin is completely inert until you supply a control-plane URL and complete a one-time signed enrollment. The control plane is either a WPMgr instance you self-host or the hosted service at manage.wpmgr.app.

**How it works / security**

The agent accepts only a closed, named allow-list of commands (backup, restore, update, cache operations, diagnostics, and similar). Every inbound command is Ed25519-signature-verified against the enrollment key established at connect time. There is no eval, no remote include, no remote PHP execution of any kind. Core, plugin, and theme updates are applied using WordPress's own native Upgrader against packages from wordpress.org -- not from the control plane directly.

**Feature set**

* Backup and restore -- full or incremental database and file backups, encrypted at rest, streamed to the storage destination your control plane configures. Incremental chains use a content-addressed chunk store so only changed blocks are transferred.
* Performance -- disk-based full-page cache with nginx/Apache fast-path bypassing PHP entirely; Remove Unused CSS (computed on the control plane, no headless browser required); self-hosted web font transcoding (TTF/OTF/WOFF to WOFF2); image optimization pipeline (WebP/AVIF conversion, lossless/lossy re-encode, format transcoding).
* Updates -- bulk WordPress core, plugin, and theme updates with rollback support, applied via the WordPress native Upgrader.
* Security scanning -- vulnerability checks against a managed database, login protection, and error monitoring.
* Uptime and health -- site health diagnostics surfaced in the control-plane dashboard, periodic heartbeat, and environment metadata (PHP, server, active plugins, theme).

All features are opt-in. Connecting the agent and initiating actions from the control plane is the only way any data leaves the site.

== Installation ==

1. Upload and activate the plugin, or install it from the WordPress plugin directory.
2. In your WPMgr control plane (self-hosted or manage.wpmgr.app), open the site-connection screen and generate a one-time signed enrollment token.
3. Paste the token into the Fleet Agent settings screen and click Connect.
4. The site appears in your control-plane dashboard. All management actions run from there.

To disconnect, click Disconnect in the Fleet Agent settings screen or remove the plugin. All communication stops immediately.

== Frequently Asked Questions ==

= Does this plugin phone home? =

No. The plugin contains no default endpoint and makes zero outbound connections until you connect it to a control plane that you supply. It is completely inert on activation.

= Do I need a WPMgr account? =

Only if you use the hosted service at manage.wpmgr.app. You can also self-host the entire WPMgr control plane -- the agent works identically either way. The plugin itself has no dependency on any specific account or service.

= Is my data sent anywhere by default? =

No. Without an active connection to a control plane you configured, no data is sent anywhere. All transmission is initiated by commands from the control plane you enrolled, never autonomously.

= How are updates handled? =

Updates to this plugin are delivered via the WordPress.org plugin directory and applied through the standard WordPress update mechanism. There is no separate update channel in this build.

= Can the control plane execute arbitrary code on my site? =

No. The command dispatcher accepts only a closed, named allow-list of commands. Every command is verified against an Ed25519 signature tied to the enrollment key. There is no mechanism to execute arbitrary PHP, SQL, or shell code.

= What happens if I deactivate the plugin? =

All outbound communication stops immediately. The control plane can no longer reach the site. Stored cache files, optimized images, and backup archives that already exist on disk are not automatically removed -- you can clean those up from the plugin settings before deactivating.

= Does this plugin write any files outside its own folder? =

Only for a small number of opt-in features that must run before WordPress finishes loading your other plugins. Enabling the optional Error Monitor writes a small must-use plugin file to wp-content/mu-plugins/ so a fatal error occurring during another plugin's own startup can still be captured and reported; disabling Error Monitor removes that file. The same pattern is used by the optional login-protection ban list (a must-use file that enforces IP and user-agent blocks at the earliest possible point in the request) and by the automatic update-safety watchdog, which is present only while a site is connected to a control plane (it is inert until an update is actually in progress, and is removed if you disconnect the site). No other feature writes outside the plugin's own folder.

== Privacy / What data is sent and where ==

This plugin does not contact any external service until you connect it to a WPMgr control plane that you choose. There is NO default endpoint; the agent is inert until you supply a control-plane URL and complete a one-time, signed enrollment from that control plane. The control plane is software you point the agent at -- either a WPMgr instance you self-host, or the hosted WPMgr service at https://manage.wpmgr.app.

Once connected, the agent communicates only with the control-plane URL you configured. It sends the following data, only to that endpoint, and only for the management actions you (or your schedules) initiate:

- Site & environment metadata -- site URL, WordPress/PHP/server versions, active theme and plugins, and Site Health diagnostics. Sent on connect, on a periodic heartbeat, and when you click Re-run checks. Used to display your site's status in the dashboard.
- Update inventory -- the list of available core, plugin, and theme updates. Sent when inventory is refreshed. Used to show and apply updates.
- Backup archives (encrypted) -- when you run or schedule a backup, the agent creates an archive of your database and/or files, encrypts it, and uploads it to the storage destination configured by your control plane. Archive contents may include your site's content and personal data; they are encrypted before leaving the server.
- Rendered HTML -- for CSS optimization (used-CSS generation), the agent submits rendered HTML of selected pages so unused CSS can be computed. Used only to produce optimized stylesheets.
- Diagnostics & activity logs -- error logs, performance/cache statistics, and a record of management actions, sent so they can be surfaced in the dashboard.

The agent does not sell or share this data with third parties. It receives signed, allow-listed commands (backup, restore, update, cache operations) from your control plane; it does NOT download or execute arbitrary remote PHP code.

**Real User Monitoring (when you enable it)**

Real User Monitoring (RUM) is off by default and must be enabled per site. It is the one exception to the agent-as-sole-transmitter model above.

When RUM is enabled, the agent injects a small, public measurement script into cached pages at cache-write time. Your site visitor's own browser -- not the agent -- then sends anonymous performance measurements directly to the control plane. The agent itself transmits nothing new; it only adds the script to the HTML it already serves.

What the visitor's browser sends:

- Core Web Vitals (LCP, INP, CLS) plus TTFB and FCP, and page-load timing.
- The page path only -- query strings are stripped before transmission, so tokens, emails, and order IDs in URLs are never sent.
- Coarse, non-identifying context: browser and device type derived from the User-Agent, connection type, and an approximate country code.

What is never collected: cookies, localStorage, cross-site identifiers, or the visitor's full IP address. The IP is used only transiently for rate-limiting and coarse country lookup, then discarded and never stored.

Because this data originates from your site visitors' browsers, you (the site owner) are the data controller for it and must disclose it in your own site's privacy policy. If you self-host the control plane, RUM data stays entirely on your own infrastructure and never reaches WPMgr. If you use the hosted service at https://manage.wpmgr.app, that service processes the measurements on your behalf.

Disable RUM at any time in the Performance settings; the script is removed from newly cached pages immediately.

If you connect to the hosted WPMgr service, that service's Terms of Service (https://manage.wpmgr.app/terms) and Privacy Policy (https://manage.wpmgr.app/privacy) apply. If you self-host the control plane, you operate the receiving service and your own policies apply. You can stop all data transmission at any time by disconnecting the agent (Disconnect in the agent admin screen) or deactivating the plugin.

**How it works / security**

Commands arrive from the control plane over HTTPS. Each command carries an Ed25519 signature produced with the key established at enrollment; the agent verifies the signature before executing any action. The allow-list of permitted commands is compiled into the plugin -- no mechanism exists to add new command types at runtime, and there is no eval, remote include, or remote PHP execution. Core, plugin, and theme updates are applied using WordPress's own Upgrader against packages from wordpress.org only.

== External services ==

This plugin contacts external hosts only after you connect it to a control plane and only for the specific features you enable. The plugin is inert until connected. The wp.org build contains no self-update client; updates arrive through the WordPress.org directory only.

**WPMgr control plane (the URL you supply)**

Every management action (enrollment, diagnostics, heartbeat, backup progress, cache and performance operations, Remove Unused CSS, autologin token consumption, database clean, font transcoding, password breach checking) sends data to the WPMgr control plane URL you configured. Data transmitted includes: site URL and name, WordPress and PHP versions, active plugin and theme inventory, Site Health results, rendered HTML of selected pages (for used-CSS computation), encrypted backup archives, transcoded font bytes, and cache and performance statistics. All transmission is triggered by actions or schedules you initiate from the control plane, never autonomously. If you use the hosted control plane at https://manage.wpmgr.app its terms and privacy policy apply: Terms https://manage.wpmgr.app/terms -- Privacy https://manage.wpmgr.app/privacy. If you self-host the control plane, you operate the receiving service and your own policies apply.

**Have I Been Pwned (https://haveibeenpwned.com) -- via the WPMgr control plane**

When the optional password-policy breach check is enabled and a site user sets or changes a password, the plugin computes the SHA-1 hash of the candidate password and sends only the first 5 characters of that hash (a k-anonymity range query) to the WPMgr control plane. The full password and the full hash never leave the site. The WPMgr control plane relays the 5-character prefix to the Have I Been Pwned range API (https://haveibeenpwned.com/API/v3#searchingPwnedPasswordsByRange), receives back a list of matching hash suffixes and breach counts, and returns that list to the agent. The agent then checks the remaining 35-character suffix locally to determine whether the password is known-breached. No full password, no full hash, and no user identity is transmitted at any point in this flow. This check is off by default; it runs only when the breach-check policy is active and only at the moment of a password set or change. Data flow: site sends the 5-character prefix to the WPMgr control plane, which forwards the range query to Have I Been Pwned. Have I Been Pwned Terms https://haveibeenpwned.com/TermsOfUse -- Have I Been Pwned Privacy https://haveibeenpwned.com/Privacy. The WPMgr control-plane hop is also covered by the terms above: Terms https://manage.wpmgr.app/terms -- Privacy https://manage.wpmgr.app/privacy.

**Object storage (configured by your control plane)**

Encrypted backup archives, restored backup chunks, optimized media files, and transcoded font bytes are transferred to and from a storage destination that your control plane supplies via short-lived presigned URLs. No storage endpoint is hardcoded in this plugin. The hosted WPMgr service uses Google Cloud Storage (storage.googleapis.com) by default; a self-hosted control plane operator may configure any S3-compatible destination. Trigger: backup, restore, and media or font optimization operations that you initiate. For the hosted default: Google Cloud Terms https://cloud.google.com/terms -- Google Privacy Policy https://policies.google.com/privacy.

**ipify (https://api.ipify.org)**

During diagnostics collection (triggered when you run or schedule a diagnostics check), the plugin makes a plain GET request to https://api.ipify.org to determine the site's public outbound IP address. No site data is included in the request; the response is the IP address, which is used for host-provider inference and cached locally for 8 hours. ipify is operated by ipify (https://www.ipify.org); its terms of service and privacy policy are published at Terms https://geo.ipify.org/terms-of-service -- Privacy https://geo.ipify.org/privacy-policy.

**Cloudflare API (https://api.cloudflare.com)**

When Cloudflare integration is active (requires CLOUDFLARE_EMAIL/CLOUDFLARE_API_KEY or CLOUDFLARE_API_TOKEN constants in wp-config.php) and a cache purge runs, the plugin sends the configured Cloudflare zone ID and the API credentials from wp-config to https://api.cloudflare.com to purge the Cloudflare edge cache. This feature is inactive unless you configure those constants. Terms https://www.cloudflare.com/website-terms/ -- Privacy https://www.cloudflare.com/privacypolicy/

**Google Fonts (https://fonts.googleapis.com, https://fonts.gstatic.com)**

When the self-hosted fonts optimization is enabled and a page uses Google Fonts, the plugin downloads the Google Fonts CSS and the referenced WOFF2 files server-side to serve them from your own domain. Data sent: the font-family request derived from the page (the same request a browser would make). Trigger: on cache build when this optimization is on. Terms https://policies.google.com/terms -- Font API Privacy https://developers.google.com/fonts/faq/privacy

**Gravatar (https://gravatar.com, https://secure.gravatar.com)**

When the self-host Gravatars optimization is enabled, the plugin downloads avatar images from Gravatar server-side to serve them from your own domain. Data sent: the avatar hash derived from the page. Trigger: on cache build when this optimization is on. Gravatar is operated by Automattic. Terms https://wordpress.com/tos/ -- Privacy https://automattic.com/privacy/

**Third-party asset hosts referenced by your pages**

When the "self-host third-party assets" optimization is enabled, the plugin downloads cross-origin script and stylesheet URLs that your own pages already reference, to serve those assets locally. It contacts whatever third-party hosts your pages embed. Data sent: a plain GET request to the URL already present in your page. Trigger: on cache build when this optimization is on. No single provider; the specific hosts depend on your site's content.

**Postmark (https://api.postmarkapp.com)**

Postmark is a transactional email delivery service operated by Wildbit LLC. When Postmark is the configured email transport for this site, the plugin POSTs outgoing email to https://api.postmarkapp.com/email. Data sent: sender address, recipient addresses (To/Cc/Bcc), subject, message body (HTML and/or plain text), and any attachments. Trigger: only when Postmark is selected as the active email provider and an outgoing email is sent from this site. Terms https://postmarkapp.com/terms-of-service -- Privacy https://postmarkapp.com/privacy-policy

**Amazon SES (https://aws.amazon.com/ses/)**

Amazon Simple Email Service (SES) is an email delivery service operated by Amazon Web Services, Inc. When Amazon SES is the configured email transport for this site, the plugin POSTs a raw MIME message to the regional SES API endpoint (https://email.{region}.amazonaws.com/ -- {region} = your configured AWS region, e.g. us-east-1). Data sent: sender address, recipient addresses (To/Cc/Bcc), subject, message body (HTML and/or plain text), and any attachments, encoded as a raw MIME message signed with AWS Signature Version 4. Trigger: only when Amazon SES is selected as the active email provider and an outgoing email is sent from this site. Terms https://aws.amazon.com/service-terms/ -- Privacy https://aws.amazon.com/privacy/

**Mailgun (https://api.mailgun.net, https://api.eu.mailgun.net)**

Mailgun is an email delivery service operated by Sinch Email (formerly Mailgun Technologies, Inc.). When Mailgun is the configured email transport for this site, the plugin POSTs outgoing email to https://api.mailgun.net/v3/{domain}/messages (US region) or https://api.eu.mailgun.net/v3/{domain}/messages (EU region). Data sent: sender address, recipient addresses (To/Cc/Bcc), subject, message body (HTML and/or plain text), and attachment metadata. Trigger: only when Mailgun is selected as the active email provider and an outgoing email is sent from this site. Terms https://www.mailgun.com/legal/terms/ -- Privacy https://www.mailgun.com/legal/privacy-policy/

**SendGrid (https://api.sendgrid.com)**

SendGrid is a cloud email delivery service operated by Twilio Inc. When SendGrid is the configured email transport for this site, the plugin POSTs outgoing email to https://api.sendgrid.com/v3/mail/send. Data sent: sender address, recipient addresses (To/Cc/Bcc), subject, message body (HTML and/or plain text), and any attachments. Trigger: only when SendGrid is selected as the active email provider and an outgoing email is sent from this site. Terms https://www.twilio.com/en-us/legal/tos -- Privacy https://www.twilio.com/en-us/legal/privacy

== Third-party / Credits ==

**matthiasmullie/minify (MIT)**

CSS and JavaScript minification uses matthiasmullie/minify (^1.3, MIT license), a pure-PHP minification library included in the plugin's Composer dependencies. Source and license: https://github.com/matthiasmullie/minify

Copyright (c) 2012 Matthias Mullie. Licensed under the MIT License.

No other third-party libraries are bundled in the plugin zip. Image encoding and WOFF2 font transcoding run on the control-plane service, not inside this plugin.

== Source code ==

This plugin ships two minified JavaScript files. Their human-readable source and build tooling are in the public repository at https://github.com/mosamlife/wpmgr.

* **assets/wpmgr-rum.min.js** -- Real User Monitoring collector. The readable, non-minified build ships alongside the plugin at assets/wpmgr-rum.js. TypeScript source: apps/tracker/src/index.ts and apps/tracker/src/vitals.ts. Build: cd apps/tracker && npm install && npm run build (esbuild IIFE bundle, also bundles Google web-vitals under its Apache-2.0 license; the same build produces both the minified and readable outputs).
* **assets/wpmgr-delay.min.js** -- deferred-script runtime. The readable source ships alongside the plugin at assets/wpmgr-delay.js in the same repository.

== Screenshots ==

1. Fleet Agent connect screen -- enter a control-plane URL and enrollment token to pair the site.
2. Control-plane dashboard -- live status, environment metadata, and health indicators for the connected site.
3. Backup in progress -- incremental backup running with chunk-transfer progress and estimated completion.
4. Performance settings -- page cache, Remove Unused CSS, self-hosted fonts, and image optimization controls.

== Changelog ==

The entries below summarize the notable changes since 0.36.0. This project ships frequently; not every intermediate patch release is listed here individually -- see the full history at https://github.com/mosamlife/wpmgr/blob/main/CHANGELOG.md.

= 0.61.80 =
* Changed: the WordPress.org distribution build no longer adjusts WordPress's own upgrader working-directory handling during a plugin, theme, or core update. Self-hosted installs are unaffected.
* Fixed: reworded a documentation comment in the file-manager write guard that quoted an example attack payload verbatim, so security scanners no longer report a false-positive match on the plugin's own file (GH #266). No behavior changed.

= 0.61.79 =
* Fix: on a site installed from a plain git clone or a GitHub source download (no `composer install` run), activating the plugin could fail immediately with a fatal error (GH #262). The plugin's own class loader could not find a handful of its own files on its own; it now resolves every one of them without depending on any other tool being present.

= 0.61.78 =
* Fix: the plugin could fail to set up its encryption key on some managed hosts (for example Hostinger and other CloudLinux/CageFS hosts), leaving it active but unable to connect (GH #257). It now tolerates a wp-config.php with missing or partial secret salts, tries the uploads folder as an additional fallback file location, and as a last resort can store a dedicated encryption key in the database when no file location is writable. The key setup is now safe against two requests racing to set it up at the same time and against a write that is interrupted partway through. Already-connected sites are unaffected. The setup notice now explains the actual cause when the key still cannot be established.

= 0.61.71 =
* Fix: the "Manage in WPMgr" admin bar link now opens the site's Cache page in the control-plane dashboard. It previously pointed at a page that has never existed.

= 0.61.64 =
* Fix: scheduled backups no longer stall indefinitely at "queued". A backup run now always starts immediately instead of depending solely on WordPress cron, and a request-driven check recovers any run that still gets stuck. A file-based lock alongside the existing database lock prevents two runs of the same backup from overlapping.

= 0.61.63 =
* Changed: the Real User Monitoring script now ships with a readable, non-minified source file alongside the minified one, for WordPress.org directory transparency. No behavior change.

= 0.61.62 =
* Fix: pre-update rollback snapshots (captured before each core, plugin, or theme update) are now reliably cleaned up instead of accumulating indefinitely on a quiet site. Cleanup no longer depends on WordPress cron.

= 0.61.61 =
* Hardening: a second WordPress.org-compliance pass. Login and forced-password-change screens now escape their output through an explicit allowed-tag list, the media-library helper script is enqueued instead of printed inline, the settings screen is fully translatable, and the WordPress.org build measures folder sizes without shelling out to the operating system.

= 0.61.58 =
* Fix: bulk update runs no longer repeat a full WordPress.org update check for every item in the batch. Updates no longer fail with "Could not copy file" on hosts with an overloaded shared temporary directory. The fleet backup-health check no longer errors for a site with no completed backup. Hide-login no longer blocks front-end AJAX requests. Two-factor sign-in messaging is clearer, with a wider accepted time window and single-use setup codes.

= 0.61.57 =
* Hardening: code-quality and WordPress.org-compliance pass with no behavior change for connected sites. Real User Monitoring now loads through the standard wp_enqueue_script mechanism instead of a hand-built script tag; the two-factor and forced-password-change login screens now escape their output through an explicit allowed-tag list at the point of output; the long-running backup, restore, database-dump, and media routines now use a bounded time limit instead of an unbounded one; and the CloudPanel cache-purge integration sanitizes its server-variable reads inline.

= 0.61.39 =
* Fix: the agent no longer overwrites a working Real User Monitoring beacon key with an empty one. Pairs with a control-plane-side recovery mechanism for the rare case where the one-time key delivery to a site was lost.

= 0.61.33 =
* Fix: backup destinations other than managed control-plane storage now actually work. A local folder on the server or your own S3-compatible bucket could be configured and pass "Test connection", but every backup still went to managed storage because the destination was never threaded through to the backup run. Full and incremental backups now run to the configured destination, and restore reads back from it; for your own bucket, the control plane signs the uploads and downloads so the site never holds your storage credentials.

= 0.61.27 =
* Fix: Real User Monitoring now collects data on sites running any page cache, not just this plugin's own. The measurement script was previously injected only inside this plugin's own cache output; it is now injected on a standard WordPress hook during page generation, so a dedicated caching plugin (or no page cache at all) no longer silently prevents Real User Monitoring data from arriving.

= 0.61.25 =
* Fix: restore no longer silently drops plugin or theme files whose path happens to contain a reserved WordPress drop-in name (for example a plugin's own class-db.php), which could leave a restored site broken while the restore reported success. Restore now matches its protected-file exclusions by exact path instead of substring, so only genuine root drop-ins (db.php, object-cache.php, advanced-cache.php) and config files are held back.

= 0.57.0 / 0.56.0 =
* New: vulnerability scanning. Installed plugins, themes, and the WordPress core version are checked against the free Wordfence Intelligence vulnerability feed; findings (severity, affected version range, fixed version, CVE references) surface in the control-plane dashboard, with one-click remediation using the existing update flow. Requires a free Wordfence Intelligence API key configured on the control plane; the agent reports inventory only, the feed lookup itself runs on the control plane.

= 0.55.0 =
* New: guided two-factor enrollment for WordPress site users. Once an operator requires 2FA for a role, an affected user is walked through scanning a QR code, confirming a code, and saving backup codes on their next login, or can start enrollment proactively from their profile.

= 0.54.0 =
* New: optional, per-site, off-by-default two-factor authentication for WordPress site users (authenticator app, email one-time code, or single-use backup codes), with configurable grace logins and a remember-this-device window. The control plane and wp-config recovery constants can always bypass enforcement so an operator can never be locked out.
* New: optional password policy (minimum strength, known-compromised-password check via a privacy-preserving prefix query, reuse blocking, optional expiry with a forced-change screen).
* New: optional hide-login, which moves the login page to a secret per-site address.

= 0.53.0 =
* New: file integrity monitoring. A scan (core files, wp-content, or the full install) compares file hashes against WordPress.org checksums for core and wp.org-hosted plugins/themes, and against a learned per-site baseline for everything else, flagging changed, added, or removed files.

= 0.52.0 =
* New: per-site WordPress hardening controls (disable file editor, restrict XML-RPC and the REST API, restrict login identifiers, force unique nicknames, block author-archive user enumeration, force SSL/HSTS, disable directory browsing, block PHP execution in uploads, protect system files). All off by default and opt-in; the control plane and the operator's own session can never be locked out by a hardening rule.
* New: a per-site ban list (blocked IP addresses, CIDR ranges, and user agents), enforced at early boot and at the web-server config level. Broad blocks covering all addresses or private/loopback ranges are rejected, and the operator's own allow-listed IPs always bypass the ban.

= 0.51.2 =
* Changed: the early-boot security helpers (the login-protection ban list and the Error Monitor fatal-error trap) are now installed as a must-use plugin file only once you turn on the corresponding feature, and removed again the moment you turn it off or deactivate the plugin. A freshly activated agent writes nothing outside its own plugin folder until you opt in to one of these features.

= 0.48.2 =
* Fix: one-click "Log in to wp-admin" no longer triggers a second two-factor challenge on sites running Solid Security or the official Two Factor plugin; it now lands directly in wp-admin. Sites running SecuPress (which replaces the login flow entirely) now get a clear "sign in normally" message instead of a loop.

= 0.46.0 =
* Changed: local backups are stored under the uploads directory (with a deny-all .htaccess and an index.php guard) instead of wp-content directly, with a best-effort migration of any existing local backups.
* Changed: the object-cache drop-in installer's transient cleanup and the media URL rewriter's postmeta lookup now bind their values through prepared-statement placeholders.

= 0.45.0 =
* New: the page-cache drop-in nudges WP-Cron on a cache hit when the cron marker is more than 60 seconds stale, so scheduled tasks keep running on a fully page-cached, low-traffic site where WordPress itself rarely boots.

= 0.44.0 =
* New: a signed, cheap liveness ("ping") command the control plane can use to verify a quiet site is actually reachable (and wake WP-Cron) before ever marking it disconnected, instead of relying solely on traffic-driven heartbeats.

= 0.41.0 =
* New: an optional, off-by-default persistent Redis object cache for the dynamic, uncacheable side of WordPress (logged-in users, admin screens, carts and checkout, REST responses, and database round-trips the page cache cannot serve). Configured and tested per site from the control plane before it can be enabled; degrades safely to an in-memory array cache on any connection failure so the site never goes down because of it.

= 0.36.0 =
* New: multiple named email connections with per-connection encrypted credentials. Define additional provider connections (for example "ses-backup") alongside the primary, each with its own provider, settings, and encrypted secret stored separately in the agent keystore.
* New: per-sender routing and automatic fallback retry. The agent routes outgoing mail by matching the FROM address to a connection key, falls back to the default connection, and retries exactly once via a configured fallback connection when the primary send fails (fallback is disabled for test sends). The email log records the connection key actually used and prefixes the response with the primary failure reason when a fallback fired.
* New: attachment names and sizes in the local email log. Each logged email now stores attachment file names (capped to 255 characters, paths stripped) and sizes in bytes (up to 50 attachments per message). This data is included in the batch pushed to the control plane and appears in the dashboard log.
* Internal: local email log schema bumped to v11 (two new columns: connection_key VARCHAR(32) NOT NULL DEFAULT '' and attachments TEXT NULL). Applied automatically via dbDelta on plugin update; no manual database change required.

= 0.35.0 =
* New: per-site email delivery and logging. Route this site's outgoing email through Amazon SES, SendGrid, Mailgun, Postmark, or any SMTP server, configured from the WPMgr dashboard. Every send is logged (with optional bounce and complaint suppression), and known-bad addresses are skipped automatically. Email sending is unchanged until you configure a provider.

= 0.34.2 =
* Fix a rare "502" when clicking "Log in to wp-admin" a second time while already signed in. The agent now detects the existing browser session reliably (independent of the REST nonce) and simply redirects instead of re-issuing the login, and any unexpected fatal during login is turned into a clean redirect rather than a server error.

= 0.34.0 =
* One-click wp-admin login is more reliable and now lands past common two-factor prompts. Clicking "Login to wp-admin" while already signed in no longer errors; the login token still expires, is single-use, and is bound to the site and your role.
* Site connection status is steadier: the connection indicator no longer briefly flips to "degraded" on healthy low-traffic sites, and a "Re-check connection" action forces an immediate refresh from the dashboard.

= 0.33.9 =
* Hardening for WordPress.org guidelines: request inputs (including server and cookie values) are sanitized; the media quarantine and database-snapshot data now write under the uploads directory (with a read fallback to the legacy location so existing installs keep working); the diagnostics info REST endpoint binds its signed token to this site and endpoint; the login-screen branding style is enqueued; and the readme now documents every external service and the public source of the bundled scripts. No change to backups, cache, or other behavior.

= 0.33.8 =
* Fix: WooCommerce cart-fragments now inject reliably on themes whose body tag has attributes (e.g. `<body class="...">`); previously the shim only matched a bare body tag and skipped injection. Cart totals refresh correctly on cached catalog pages.
* Fix: cache hit-ratio now counts 304 Not Modified and HEAD responses served from cache, so the dashboard hit ratio is no longer under-reported.
* Fix: cache hit/miss counts are no longer lost if a stats upload to the control plane fails; counts are staged and only cleared after a confirmed send, and any interrupted batch is recovered on the next cycle.
* Performance: the cache stats consumer no longer reads whole tally files into memory, and the Unused Image Cleaner bounds its in-use list to keep memory flat on very large media libraries.

= 0.33.5 =
* Maintenance: version alignment with the control plane. No plugin functional changes (the fix in this release was control-plane only: the Real User Monitoring dashboard's "All devices" tab now shows data).

= 0.33.4 =
* Fix: the Real User Monitoring collector script is now loaded from a versioned URL, so a content delivery network or browser cache refetches it whenever the plugin updates. Previously the collector was served from a static filename, so a long-lived edge cache could keep serving the previous collector after an update until the cache was manually purged. This is the control-plane dashboard release that adds Core Web Vitals distribution bars (good / needs improvement / poor) and a 28-day trend chart; the agent change in this version is the cache-busting fix only.

= 0.33.3 =
* Fix: Real User Monitoring now reliably collects CLS on cached pages. The Core Web Vital collectors are registered in the order recommended by the web-vitals library (paint metrics before layout shift) so the CLS reporter is always armed before a load-and-leave visitor can hide the page. Previously, on an already-cached page, the CLS measurement could be dropped in a brief timing window. No effect on backups, cache, or other features.

= 0.33.2 =
* Fix: Real User Monitoring now collects CLS. The collector is upgraded to web-vitals 5 and loaded early (async, in the head) so the CLS reporter is armed before the page is hidden; previously, on a load-and-leave visit, CLS was never sent. No effect on backups, cache, or other features.

= 0.33.1 =
* Fix: Real User Monitoring now collects CLS and INP. The collector sends each Core Web Vital the moment it is finalized instead of batching at page-hide, so CLS and INP (which finalize at page-hide) are no longer dropped. INP still requires a real visitor interaction; CLS reports 0 on pages with no layout shift.

= 0.33.0 =
* Performance: Real User Monitoring (RUM), per-site and off by default. When enabled, the agent injects a tiny first-party measurement script into cached pages; the visitor's browser sends anonymous Core Web Vitals (LCP, INP, CLS, FCP, TTFB) and page-load timing directly to your control plane. No cookies, no cross-site identifiers, the page path is stored with the query string stripped, and the visitor IP is never stored. See the Privacy section.

= 0.32.1 =
* Maintenance: version alignment with the control plane. No plugin functional changes (the fix in this release was control-plane only).

= 0.32.0 =
* Performance: self-hosted font subsetting (experimental, default off). Discovers fonts loaded via external stylesheets in addition to inline font-face rules, and reports per-font conversion progress to the control-plane dashboard. Subsetting and transcoding run on the control-plane service, not inside the plugin.

= 0.31.1 =
* Onboarding: cancel action hard-deletes a site that was never connected; Disconnected-sites empty-state panel now shows Reconnect and Remove actions.
* Backup: incremental backup engine v1 with content-addressed chunk store (ADR-048); incremental chain restore (ADR-049).
* Media: WOFF2 font transcoding -- converts TTF/OTF/WOFF to WOFF2 via a pure-Go transcoder on the control plane (ADR-052); flag `fonts_transcode_woff2` defaults off.
* Web: Fleet Hub brand favicon (SVG) and theme-color meta.
* Fix: PHP and JS CI jobs green.

== Upgrade Notice ==

= 0.61.57 =
WordPress.org compliance and code-quality hardening (safer output escaping, standard script enqueueing, bounded long-running operations). No behavior change for connected sites. Safe to update in place.

= 0.61.25 =
Fixes a restore bug that could silently drop plugin or theme files whose path contained a reserved drop-in name, leaving a restored site broken while reporting success. If a restore on an older version left a site broken, re-restore the same snapshot on this version.

= 0.54.0 =
Adds optional, off-by-default two-factor authentication, password policy, and hide-login controls for your WordPress site's own users. All opt-in per site from your control plane; nothing changes until you enable them. Safe to update in place.

= 0.41.0 =
Adds an optional, off-by-default persistent Redis object cache. Requires configuring and testing a Redis connection from your control plane before it activates. Safe to update in place.

= 0.33.9 =
WordPress.org compliance hardening: input sanitization, uploads-directory storage, REST token binding, enqueued login style, and external-service + source documentation. Safe to update in place.

= 0.33.8 =
Reliability fixes for WooCommerce cart-fragments injection on themed body tags and for cache hit-ratio accuracy (304/HEAD now counted, stats no longer lost on a failed upload). Safe to update in place.

= 0.33.5 =
Version alignment with the control plane. No plugin functional changes. Safe to update in place.

= 0.33.4 =
Serves the Real User Monitoring collector from a versioned URL so future collector updates are never masked by a content delivery network or browser cache. Pairs with a control-plane dashboard update that adds Core Web Vitals distribution bars and a 28-day trend. Safe to update in place.

= 0.33.3 =
Fixes Real User Monitoring so it reliably collects CLS on cached pages by registering the Web Vitals collectors in the recommended order. Update to capture CLS from real visitors. Safe to update in place.

= 0.33.2 =
Fixes Real User Monitoring so it collects CLS (Cumulative Layout Shift), completing Core Web Vitals coverage. Update to capture CLS from real visitors. Safe to update in place.

= 0.33.1 =
Fixes Real User Monitoring so it collects CLS and INP (not just LCP, FCP, TTFB). Update to capture all Core Web Vitals from real visitors. Safe to update in place.

= 0.33.0 =
Adds opt-in, off-by-default Real User Monitoring (anonymous Core Web Vitals from real visitors). No database changes on the site. Safe to update in place.

= 0.32.1 =
Version alignment with the control plane. No plugin functional changes. Safe to update in place.

= 0.31.1 =
Adds incremental backup engine, WOFF2 font transcoding, and an onboarding cancel fix that hard-deletes never-connected sites. No database changes required. Safe to update in place.
