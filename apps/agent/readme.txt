=== Fleet Agent Site Manager - Securely Connects Your Sites to a WPMgr Dashboard ===
Contributors: mosamlife
Tags: backup, security, performance, updates, site management
Requires at least: 6.2
Tested up to: 7.0
Requires PHP: 8.1
Stable tag: 0.61.139
License: GPLv2 or later
License URI: https://www.gnu.org/licenses/gpl-2.0.html

Securely connects this site to a WPMgr dashboard, self-hosted or hosted, so backups, updates, security and speed all run from one screen.

== Description ==

**Fleet Agent Site Manager securely connects the WordPress sites you look after to a WPMgr dashboard, so backups, updates, caching, security and performance for every one of them run from a single screen.**

**You need a WPMgr dashboard as well as this plugin.** Run one on your own server for free (it is open source: https://github.com/mosamlife/wpmgr), or use the hosted service at https://manage.wpmgr.app.

Until you enter a dashboard address and complete a signed pairing, this plugin has no endpoint, opens no outbound connection and sends nothing anywhere.

Every action the dashboard can ask for is on a closed, named list compiled into the plugin, and each one is verified against an Ed25519 signature tied to that pairing before it runs. There is no eval, no remote include and no remote PHP execution of any kind.

= What you can do once a site is connected =

**Backups and restore.** Full and incremental backups of the database and files, scheduled per site or run on demand. Archives are encrypted on the site before upload, and an incremental run uses a content-addressed chunk store so only changed blocks move. Send them to storage the dashboard manages, a folder on your own server, or your own S3-compatible bucket. Restore a whole site or pick components, with a health check afterwards and an automatic rollback if the site does not come back.

**Updates you can undo.** Core, plugin and theme updates across every connected site, applied by WordPress's own Upgrader against WordPress.org packages. A snapshot is taken before each one, so a bad release is one click back rather than a restore from last night's backup. A watchdog recovers a site whose update died mid-write.

**Speed.** Disk page cache with an nginx and Apache fast path that serves a hit without booting PHP. Optional Redis object cache. Remove Unused CSS, CSS and JavaScript minification, deferred JavaScript, speculation rules, CDN rewriting, lazy iframes, and self-hosted Google Fonts, Gravatars and third-party scripts.

**Images and fonts.** Convert the media library to WebP and AVIF, keep the originals, and put them back if you do not like the result. An unused-image cleaner finds files nothing references. TTF, OTF and WOFF are transcoded to WOFF2.

**Security.** Hardening switches that stay off until you turn them on: file editor, XML-RPC, REST, author enumeration, SSL and HSTS, directory browsing, PHP execution in uploads. Login protection with per-site IP, CIDR and user-agent bans enforced at earliest boot. A hidden login address. File-integrity scanning against WordPress.org checksums and a learned per-site baseline.

**Two-factor for your site's own users.** Authenticator app, emailed code or single-use backup codes, with guided enrollment once you require it for a role.

**Password policy.** Minimum strength, reuse blocking, optional expiry, and a breach check that never lets the password or its full hash leave the site. Policies target the roles your site actually has, including roles added by WooCommerce, membership and LMS plugins.

**Database tools.** Scan for revisions, transients, spam and orphaned metadata, preview exactly what will go before anything is deleted, take a snapshot before any change, and run a serialization-aware search and replace.

**File manager.** Browse, read, write, rename, delete, chmod, search, upload, archive and extract, with a version history on writes and a restore. Guarded against executable writes and protected roots.

**Site email.** Route this site's outgoing mail through Amazon SES, SendGrid, Mailgun, Postmark or SMTP, with per-sender routing, automatic fallback, suppression of known-bad addresses and a full send log.

**Real User Monitoring.** Off by default. Turn it on and Core Web Vitals come from your actual visitors, page by page, instead of one lab score.

Cache purge reaches the layer in front of WordPress too, detected automatically: Varnish, Kinsta, WP Engine, SiteGround, Cloudways, RunCloud, GridPane, SpinupWP, CloudPanel, Rocket.net and WP Cloud, plus Cloudflare once you add its API credentials to wp-config.php.

The expensive work runs on the dashboard rather than on your server: unused-CSS computation, image and font encoding, vulnerability matching against a managed feed, uptime probing and site screenshots.

This plugin is GPLv2 or later and the dashboard is AGPL-3.0. Source for both: https://github.com/mosamlife/wpmgr

== Installation ==

1. Install and activate this plugin on the site you want to manage.
2. Get a dashboard, if you do not have one yet. Either run WPMgr on your own server (source and install guide at https://github.com/mosamlife/wpmgr) or sign in at https://manage.wpmgr.app. Both give you the same dashboard.
3. In the dashboard, add the site. It gives you a pairing code.
4. Back on this site, open Fleet Agent Site Manager in the WordPress admin menu. Put your dashboard's address in the **Control-plane URL** field and click **Save URL**.
5. Paste the pairing code into the **Pairing code** field and click **Enroll**. The site appears in your dashboard, and every management action runs from there.

= What you need before you start =

* WordPress 6.2 or newer
* PHP 8.1 or newer
* A WPMgr dashboard to connect to, self-hosted or hosted

= How to disconnect =

Click **Disconnect** on the same settings screen, or deactivate the plugin. All outbound communication stops immediately.

== Frequently Asked Questions ==

= Do I need anything besides this plugin? =

Yes. This plugin is the agent that runs on your site, and it needs a WPMgr dashboard to talk to. That is either a WPMgr instance you run yourself (open source, https://github.com/mosamlife/wpmgr) or the hosted service at https://manage.wpmgr.app. On its own the plugin does nothing at all.

= Why is this called Fleet Agent Site Manager when the dashboard is called WPMgr? =

WPMgr is the project. This plugin is its agent, listed in this directory under the name the plugin directory approved, and the project's own code and documentation call it the WPMgr agent. One plugin, one repository: https://github.com/mosamlife/wpmgr

= Is self-hosting a cut-down version? =

No. The dashboard is AGPL-3.0, has no site limit when you run it yourself, and nothing in it is gated behind a paid tier. Running it does mean running the control-plane service, PostgreSQL and object storage, plus the encoder service if you want image, font and unused-CSS optimization, and a free Wordfence Intelligence API key if you want vulnerability matching. The install guide is in the repository.

= How many sites can one dashboard manage? =

Self-hosted, as many as your server will carry. The hosted service has per-plan limits, listed at https://wpmgr.app/pricing

= Will it work on shared hosting? =

Yes. The work that needs real CPU runs on the dashboard, not on your server. Long jobs such as backups acknowledge the request and continue in the background instead of holding a connection open.

= Does it conflict with my existing cache or security plugin? =

Every feature is separately optional, so you can connect a site and use only the parts you want. Leave the page cache off and keep the caching plugin you already have; backups, updates and the rest work regardless.

= What happens if the dashboard is unreachable? =

Your site carries on serving. Work the dashboard schedules, such as backups, waits until it can reach the site again. Nothing on the site depends on the dashboard being up.

= What runs on my server, and what runs on the dashboard? =

This plugin listens for the dashboard's signed commands, carries them out, and sends a heartbeat and environment metadata so the dashboard knows the site is alive. That is all it does on your server.

Everything expensive runs on the dashboard: Remove Unused CSS is computed there, image and font encoding happen there, vulnerability matching against a managed feed happens there, and uptime is probed from there rather than from inside your site. Your server never needs a headless browser or an image library.

= Where are my backups stored, and can I use my own bucket? =

Per site, choose storage the dashboard manages, a folder on your own server, or any S3-compatible bucket. Your storage credentials stay on the dashboard and never reach the site; the dashboard signs each transfer.

= Does this plugin phone home? =

No. The plugin contains no default endpoint and makes zero outbound connections until you connect it to a control plane that you supply. It is completely inert on activation.

= Do I need a WPMgr account? =

Only if you use the hosted service at https://manage.wpmgr.app. You can also self-host the entire WPMgr control plane, and the agent works identically either way. The plugin itself has no dependency on any specific account or service.

= Is my data sent anywhere by default? =

No. Without an active connection to a control plane you configured, no data is sent anywhere. All transmission is initiated by commands from the control plane you enrolled, never autonomously.

= How are updates handled? =

Updates to this plugin are delivered via the WordPress.org plugin directory and applied through the standard WordPress update mechanism. There is no separate update channel in this build.

= Can the control plane execute arbitrary code on my site? =

No. The command dispatcher accepts only a closed, named allow-list of commands. Every command is verified against an Ed25519 signature tied to the enrollment key. There is no mechanism to execute arbitrary PHP, SQL, or shell code.

= What happens if I deactivate the plugin? =

All outbound communication stops immediately. The control plane can no longer reach the site. Stored cache files, optimized images, and backup archives that already exist on disk are not automatically removed, and you can clean those up from the plugin settings before deactivating.

= Does this plugin write any files outside its own folder? =

Only for a small number of opt-in features that must run before WordPress finishes loading your other plugins. Enabling the optional Error Monitor writes a small must-use plugin file to wp-content/mu-plugins/ so a fatal error occurring during another plugin's own startup can still be captured and reported; disabling Error Monitor removes that file. The same pattern is used by the optional login-protection ban list (a must-use file that enforces IP and user-agent blocks at the earliest possible point in the request) and by the automatic update-safety watchdog, which is present only while a site is connected to a control plane (it is inert until an update is actually in progress, and is removed if you disconnect the site). No other feature writes outside the plugin's own folder.

== Privacy / What data is sent and where ==

This plugin does not contact any external service until you connect it to a WPMgr control plane that you choose. There is NO default endpoint; the agent is inert until you supply a control-plane URL and complete a one-time, signed enrollment from that control plane. That control plane is either a WPMgr instance you self-host or the hosted WPMgr service at https://manage.wpmgr.app.

Once connected, the agent communicates only with the control-plane URL you configured. It sends the following, only to that endpoint, and only for the management actions you or your schedules initiate:

- Site and environment metadata: site URL, WordPress, PHP and server versions, active theme and plugins, and Site Health diagnostics. Sent on connect, on a periodic heartbeat, and when you click Re-run checks. Used to display your site's status in the dashboard.
- Update inventory: the list of available core, plugin and theme updates. Sent when inventory is refreshed. Used to show and apply updates.
- Backup archives (encrypted): when you run or schedule a backup, the agent archives your database and/or files, encrypts the archive, and uploads it to the storage destination your control plane configured. Archive contents may include your site's content and personal data, and are encrypted before leaving the server.
- Rendered HTML: for CSS optimization (used-CSS generation), the agent submits rendered HTML of selected pages so unused CSS can be computed. Used only to produce optimized stylesheets.
- Diagnostics and activity logs: error logs, performance and cache statistics, and a record of management actions, sent so they can be surfaced in the dashboard.

The agent does not sell or share this data with third parties. It receives signed, allow-listed commands (backup, restore, update, cache operations) from your control plane; it does NOT download or execute arbitrary remote PHP code.

**Real User Monitoring (when you enable it)**

Real User Monitoring (RUM) is off by default and must be enabled per site. It is the one exception to the agent-as-sole-transmitter model above: the agent adds a small, public measurement script to HTML it already serves, and your visitor's own browser, not the agent, then sends anonymous performance measurements directly to the control plane.

What the visitor's browser sends:

- Core Web Vitals (LCP, INP, CLS) plus TTFB and FCP, and page-load timing.
- The page path only. Query strings are stripped before transmission, so tokens, emails, and order IDs in URLs are never sent.
- Coarse, non-identifying context: browser and device type derived from the User-Agent, connection type, and an approximate country code.

What is never collected: cookies, localStorage, cross-site identifiers, or the visitor's full IP address. The IP is used only transiently for rate-limiting and coarse country lookup, then discarded and never stored.

This data originates from your visitors' browsers, so you (the site owner) are its data controller and must disclose it in your own site's privacy policy. If you self-host the control plane, RUM data stays entirely on your own infrastructure and never reaches WPMgr. If you use the hosted service at https://manage.wpmgr.app, that service processes the measurements on your behalf.

Disable RUM at any time in the Performance settings; the script is removed from newly cached pages immediately.

If you connect to the hosted WPMgr service, its Terms of Service (https://manage.wpmgr.app/terms) and Privacy Policy (https://manage.wpmgr.app/privacy) apply. If you self-host the control plane, you operate the receiving service and your own policies apply. You can stop all data transmission at any time by disconnecting the agent (Disconnect in the agent admin screen) or deactivating the plugin.

**How it works / security**

Commands arrive from the control plane over HTTPS. Each carries an Ed25519 signature produced with the key established at enrollment, and the agent verifies it before executing any action. The allow-list of permitted commands is compiled into the plugin, no mechanism exists to add new command types at runtime, and there is no eval, remote include, or remote PHP execution. Core, plugin, and theme updates are applied using WordPress's own Upgrader against wordpress.org packages only.

== External services ==

This plugin contacts external hosts only after you connect it to a control plane, and only for the specific features you enable. The plugin is inert until connected. This build contains no self-update client; updates arrive through the WordPress.org directory only.

**WPMgr control plane (the URL you supply)**

What is sent: site URL and name, WordPress and PHP versions, active plugin and theme inventory, Site Health results, rendered HTML of selected pages (for used-CSS computation), encrypted backup archives, transcoded font bytes, and cache and performance statistics.
When: on enrollment, diagnostics, heartbeat, backup progress, cache and performance operations, Remove Unused CSS, autologin token consumption, database clean, font transcoding, and password breach checking. Always triggered by an action or schedule you initiate, never autonomously.
Why: this is the dashboard that manages the site.
Hosted at https://manage.wpmgr.app, its terms and privacy policy apply. Terms: https://manage.wpmgr.app/terms Privacy: https://manage.wpmgr.app/privacy
Self-hosted, you operate the receiving service and your own policies apply.

**Have I Been Pwned (https://haveibeenpwned.com), reached via the WPMgr control plane**

What is sent: the first 5 characters of the SHA-1 hash of a candidate password, a k-anonymity range query. The site sends that prefix to the WPMgr control plane, which relays it to the Have I Been Pwned range API (https://haveibeenpwned.com/API/v3#searchingPwnedPasswordsByRange) and returns the matching hash suffixes. The password itself, the full hash and the user's identity never leave the site; the agent compares the remaining 35-character suffix locally.
When: only while the optional password-policy breach check is enabled, and only when a site user sets or changes a password. Off by default.
Why: to refuse a password already known to be breached.
Terms: https://haveibeenpwned.com/TermsOfUse Privacy: https://haveibeenpwned.com/Privacy
The control-plane hop is covered by the WPMgr terms above.

**Object storage (configured by your control plane)**

What is sent: encrypted backup archives, restored backup chunks, optimized media files and transcoded font bytes, over short-lived presigned URLs the control plane supplies. No storage endpoint is hardcoded in this plugin.
When: during backup, restore, and media or font optimization operations that you initiate.
Why: this is where your backups and optimized assets are stored.
The hosted service uses Google Cloud Storage (storage.googleapis.com) by default; a self-hosted operator may configure any S3-compatible destination. For the hosted default, Terms: https://cloud.google.com/terms Privacy: https://policies.google.com/privacy

**ipify (https://api.ipify.org), operated by ipify (https://www.ipify.org)**

What is sent: nothing. A plain GET request carrying no site data; the response is this server's public outbound IP address, cached locally for 8 hours.
When: during diagnostics collection, which you run or schedule.
Why: to infer which host the site runs on.
Terms: https://geo.ipify.org/terms-of-service Privacy: https://geo.ipify.org/privacy-policy

**Cloudflare API (https://api.cloudflare.com), operated by Cloudflare, Inc.**

What is sent: the configured Cloudflare zone ID and the API credentials you placed in wp-config.php.
When: on a cache purge, and only while the Cloudflare integration is active. It stays inactive unless you define CLOUDFLARE_EMAIL and CLOUDFLARE_API_KEY, or CLOUDFLARE_API_TOKEN, in wp-config.php.
Why: to purge the Cloudflare edge cache so visitors do not keep an old page.
Terms: https://www.cloudflare.com/website-terms/ Privacy: https://www.cloudflare.com/privacypolicy/

**Google Fonts (https://fonts.googleapis.com, https://fonts.gstatic.com), operated by Google LLC**

What is sent: the font-family request derived from the page, the same request a browser would otherwise make.
When: on cache build, and only while the self-hosted fonts optimization is enabled and a page uses Google Fonts.
Why: to download the CSS and WOFF2 files server-side and serve them from your own domain instead.
Terms: https://policies.google.com/terms Privacy: https://developers.google.com/fonts/faq/privacy

**Gravatar (https://gravatar.com, https://secure.gravatar.com), operated by Automattic**

What is sent: the avatar hash derived from the page.
When: on cache build, and only while the self-host Gravatars optimization is enabled.
Why: to download avatar images server-side and serve them from your own domain instead.
Terms: https://wordpress.com/tos/ Privacy: https://automattic.com/privacy/

**Third-party asset hosts referenced by your own pages**

What is sent: a plain GET request to a cross-origin script or stylesheet URL that your page already references.
When: on cache build, and only while the self-host third-party assets optimization is enabled.
Why: to serve those assets from your own domain instead.
There is no single provider. The hosts depend entirely on what your own pages embed, and the applicable terms and privacy policies are whichever of those hosts publish.

**Email delivery providers (only the one you select, if any)**

What is sent, for every provider below: sender address, recipient addresses (To, Cc, Bcc), subject, message body (HTML and/or plain text), and attachments or attachment metadata.
When: only when that provider is the active email transport for this site and an outgoing email is sent. No provider is active until you configure one.
Why: to deliver this site's outgoing mail through a provider rather than the server's own mailer.

- Postmark, operated by Wildbit LLC. Endpoint: https://api.postmarkapp.com/email Terms: https://postmarkapp.com/terms-of-service Privacy: https://postmarkapp.com/privacy-policy
- Amazon SES (https://aws.amazon.com/ses/), operated by Amazon Web Services, Inc. Endpoint: https://email.{region}.amazonaws.com/ where {region} is your configured AWS region, for example us-east-1. The message is sent as raw MIME signed with AWS Signature Version 4. Terms: https://aws.amazon.com/service-terms/ Privacy: https://aws.amazon.com/privacy/
- Mailgun, operated by Sinch Email (formerly Mailgun Technologies, Inc.). Endpoints: https://api.mailgun.net/v3/{domain}/messages (US region) or https://api.eu.mailgun.net/v3/{domain}/messages (EU region). Terms: https://www.mailgun.com/legal/terms/ Privacy: https://www.mailgun.com/legal/privacy-policy/
- SendGrid, operated by Twilio Inc. Endpoint: https://api.sendgrid.com/v3/mail/send Terms: https://www.twilio.com/en-us/legal/tos Privacy: https://www.twilio.com/en-us/legal/privacy
- SMTP: whichever server you configure. Your own provider's terms and privacy policy apply.

== Third-party / Credits ==

**matthiasmullie/minify (MIT)**

CSS and JavaScript minification uses matthiasmullie/minify (^1.3, MIT license), a pure-PHP minification library included in the plugin's Composer dependencies. Source and license: https://github.com/matthiasmullie/minify

Copyright (c) 2012 Matthias Mullie. Licensed under the MIT License.

No other third-party libraries are bundled in the plugin zip. Image encoding and WOFF2 font transcoding run on the control-plane service, not inside this plugin.

== Source code ==

This plugin ships two minified JavaScript files. Their human-readable source and build tooling are in the public repository at https://github.com/mosamlife/wpmgr

* **assets/wpmgr-rum.min.js** is the Real User Monitoring collector. The readable, non-minified build ships alongside the plugin at assets/wpmgr-rum.js. TypeScript source: apps/tracker/src/index.ts and apps/tracker/src/vitals.ts. Build: cd apps/tracker && npm install && npm run build (esbuild IIFE bundle, which also bundles Google web-vitals under its Apache-2.0 license; the same build produces both the minified and readable outputs).
* **assets/wpmgr-delay.min.js** is the deferred-script runtime. The readable source ships alongside the plugin at assets/wpmgr-delay.js in the same repository.

== Screenshots ==

1. The WPMgr dashboard. Every site you manage in one list, with its status, pending updates and tags.
2. This plugin's own settings screen in wp-admin before pairing: a Control-plane URL to save, then a Pairing code to enroll. Nothing happens until you supply both.
3. One site's Backups tab: a completed run, the destination it went to, and the restore history.
4. Updates across every connected site, with the pre-update snapshot available to roll back.
5. Performance: Core Web Vitals from real visitors, page cache state and image optimization.
6. Security: hardening switches, login protection, and the vulnerability findings the dashboard matched for this site.

== Changelog ==

The entries below summarize the notable changes since 0.31.1. This project ships frequently and not every intermediate patch release is listed here. Full history: https://github.com/mosamlife/wpmgr/blob/main/CHANGELOG.md

= 0.61.139 =
* Added: this site now also reports an email delivery failure detected through WordPress's own mail-failure signal, not only failures on mail this site routes through WPMgr. Sites sending through their own SMTP setup are now covered too.
* Added: a detected failure is recorded even when this site's email log is turned off. That setting controls whether successful sends are kept for review; a failure is recorded regardless, and the recipient, subject, sender and message body are withheld from the record unless email logging is on.
* Fixed: `wp_mail()` reported success on a failed send, so a contact form or password reset could tell a visitor the message went out when nothing was delivered. `wp_mail()` now returns false on a failed send and fires WordPress's own mail-failure hook, so plugins and themes that check the result learn the truth.

= 0.61.131 =
* Fixed: sending email from this site could silently stop working days after it had been set up and used successfully, showing as "SMTP Error: Could not authenticate" (GH #380). Saving, testing or syncing this site's email settings from the dashboard could send an empty password, and the site read that as an instruction to delete the working password it already had. This site no longer deletes a stored password unless the dashboard actually sends a real replacement, and it now refuses an email settings update it cannot make sense of instead of acting on the part it understood. If this site lost its password this way, the dashboard restores it the next time it syncs this site's email settings, with nothing for you to re-enter.
* Security: a password belonging to your dashboard account is no longer sent to a mail server you choose from this site's own settings page, and a stored password is no longer carried over automatically when you point this site's email at a different mail server, mailbox user or provider.

= 0.61.127 =
* Fixed: a password policy can now target the roles the site actually has, not only the five roles a stock WordPress install ships with. On a WooCommerce store the roles that matter are the ones the store added, and a shop manager who can edit orders, refund customers and read every buyer's address could not be required to hold a strong password, because that role could not be selected. The same applied to membership, LMS and booking plugins, and to roles an agency created by hand. Enforcement was never the problem: the agent has always applied a policy to whatever role a user really holds. What was missing was any way to write the rule. Role names now also read the way they read on the site itself, in the site's own language, while the rule stays bound to the underlying role identifier, so renaming or translating a role never changes who it covers.

= 0.61.114 =
* Fixed: two updates could be dispatched to the same site at once, for example two plugins in one bulk run, or an update and a rollback overlapping, which ran more than one WordPress installer against the same site at the same time (GH #328). A second installer can delete files the first one is still using. Updates and rollbacks on a single site now run strictly one at a time.
* Fixed: when an update fails before it has touched anything on the site, for example a corrupted download, the site no longer runs an automatic restore over a plugin or theme directory it never modified.

= 0.61.112 =
* Fixed: outgoing email failed completely whenever a plugin set a Reply-To header in the ordinary "Name <email@example.com>" form, which WooCommerce, Fluent Forms and many others do by default (GH #312). The whole string, display name included, was handed to the mail transport as if it were the address; the transport rejected it, and one bad address aborted the entire message, so nothing was sent. Addresses are now parsed properly wherever a bare address is required, the display name is kept, and a single bad address costs that one recipient instead of the whole email. The same defect applied to To, Cc and Bcc on the SMTP and SendGrid providers, including headers carrying more than one address.

= 0.61.95 =
* Fixed: a failed backup left its working files behind on the site (GH #256). One reported site was holding roughly 1.4 GB of upload parts, copied plugins and themes, and a database dump. All four paths that can give up on a backup now reclaim that working directory, and each one takes the same lock a live backup holds before deleting anything, so a backup that is merely slow is never touched. The routine cleanup of old working directories and of leftover restore files no longer depends on WP-Cron, so it also runs on a site where WP-Cron never fires.

= 0.61.87 =
* Fixed: an interrupted backup now resumes cleanly instead of failing on a missing local chunk (GH #283). The agent deleted each chunk from local disk right after uploading it but only recorded its progress once the whole upload finished, so a worker stopped partway through resumed, was correctly told to send chunks it had already uploaded and deleted, then failed looking for them. Upload progress is now recorded durably as it goes, and no local chunk is deleted until that progress is safely persisted.

= 0.61.85 =
* Fixed: full backups on slow servers no longer fail at the upload stage (GH #279). A large backup could go quiet during the archiving stage for long enough that the control plane wrongly marked it failed while it was still working. The agent now sends progress heartbeats during long archive and encryption passes, reports the exact control-plane status when a callback is rejected, and removes its local working files when a run ends in failure.

= 0.61.84 =
* Fixed: backups no longer time out on OpenLiteSpeed and LiteSpeed servers (GH #274). The agent acknowledges a backup request and continues the work in the background; it previously released that acknowledgment using a PHP-FPM-only mechanism, which OpenLiteSpeed's PHP does not provide, so the connection stayed open for the entire backup. The agent now releases the acknowledgment using whichever mechanism the site's server actually supports.

= 0.61.82 =
* Fixed: the "disable file editor" hardening toggle could fatal every request on a Roots/Bedrock site (GH #268). Roots/Bedrock manages that constant through its own config layer and throws when it is defined a second time; the agent now checks whether it is already defined (or the config file is otherwise framework-managed) before writing anything, so nothing is written in that case and the toggle still reports success. Standard WordPress installs are unaffected. The full-page cache's WP_CACHE constant is protected the same way.

= 0.61.81 =
* Fixed: control-plane commands could fail on sites running a third-party plugin that globally decodes the Authorization header on every request, for example as part of its own JWT-based auth (GH #269). The agent now moves its own signed Authorization value out of the request before any other plugin's code runs, so this class of conflict can no longer occur.

= 0.61.80 =
* Changed: the WordPress.org distribution build no longer adjusts WordPress's own upgrader working-directory handling during a plugin, theme, or core update. Self-hosted installs are unaffected.
* Fixed: reworded a documentation comment in the file-manager write guard that quoted an example attack payload verbatim, so security scanners no longer report a false-positive match on the plugin's own file (GH #266). No behavior changed.

= 0.61.79 =
* Fixed: on a site installed from a plain git clone or a GitHub source download (no `composer install` run), activating the plugin could fail immediately with a fatal error (GH #262). The plugin's own class loader could not find a handful of its own files; it now resolves every one of them without depending on any other tool being present.

= 0.61.78 =
* Fixed: the plugin could fail to set up its encryption key on some managed hosts (for example Hostinger and other CloudLinux/CageFS hosts), leaving it active but unable to connect (GH #257). It now tolerates a wp-config.php with missing or partial secret salts, tries the uploads folder as an additional fallback file location, and as a last resort can store a dedicated encryption key in the database when no file location is writable. Key setup is now safe against two requests racing to set it up at the same time and against a write interrupted partway through. Already-connected sites are unaffected.

= 0.61.71 =
* Fixed: the "Manage in WPMgr" admin bar link now opens the site's Cache page in the dashboard. It previously pointed at a page that has never existed.

= 0.61.64 =
* Fixed: scheduled backups no longer stall indefinitely at "queued". A backup run now always starts immediately instead of depending solely on WordPress cron, and a request-driven check recovers any run that still gets stuck. A file-based lock alongside the existing database lock prevents two runs of the same backup from overlapping.

= 0.61.63 =
* Changed: the Real User Monitoring script now ships with a readable, non-minified source file alongside the minified one, for WordPress.org directory transparency. No behavior change.

= 0.61.62 =
* Fixed: pre-update rollback snapshots (captured before each core, plugin, or theme update) are now reliably cleaned up instead of accumulating indefinitely on a quiet site. Cleanup no longer depends on WordPress cron.

= 0.61.61 =
* Hardening: a second WordPress.org-compliance pass. Login and forced-password-change screens now escape their output through an explicit allowed-tag list, the media-library helper script is enqueued instead of printed inline, the settings screen is fully translatable, and the WordPress.org build measures folder sizes without shelling out to the operating system.

= 0.61.58 =
* Fixed: bulk update runs no longer repeat a full WordPress.org update check for every item in the batch. Updates no longer fail with "Could not copy file" on hosts with an overloaded shared temporary directory. Hide-login no longer blocks front-end AJAX requests. Two-factor sign-in messaging is clearer, with a wider accepted time window and single-use setup codes.

= 0.61.57 =
* Hardening: code-quality and WordPress.org-compliance pass with no behavior change for connected sites. Real User Monitoring now loads through the standard wp_enqueue_script mechanism instead of a hand-built script tag; the two-factor and forced-password-change login screens now escape their output through an explicit allowed-tag list at the point of output; the long-running backup, restore, database-dump, and media routines now use a bounded time limit instead of an unbounded one; and the CloudPanel cache-purge integration sanitizes its server-variable reads inline.

= 0.61.39 =
* Fixed: the agent no longer overwrites a working Real User Monitoring beacon key with an empty one.

= 0.61.33 =
* Fixed: backup destinations other than managed control-plane storage now actually work. A local folder on the server or your own S3-compatible bucket could be configured and pass "Test connection", but every backup still went to managed storage because the destination was never threaded through to the backup run. Full and incremental backups now run to the configured destination, and restore reads back from it; for your own bucket, the control plane signs the uploads and downloads so the site never holds your storage credentials.

= 0.61.27 =
* Fixed: Real User Monitoring now collects data on sites running any page cache, not just this plugin's own. The measurement script was previously injected only inside this plugin's own cache output; it is now injected on a standard WordPress hook during page generation.

= 0.61.25 =
* Fixed: restore no longer silently drops plugin or theme files whose path happens to contain a reserved WordPress drop-in name (for example a plugin's own class-db.php), which could leave a restored site broken while the restore reported success. Restore now matches its protected-file exclusions by exact path instead of substring, so only genuine root drop-ins (db.php, object-cache.php, advanced-cache.php) and config files are held back.

= 0.57.0 / 0.56.0 =
* New: vulnerability scanning. Installed plugins, themes, and the WordPress core version are checked against the free Wordfence Intelligence vulnerability feed; findings (severity, affected version range, fixed version, CVE references) surface in the dashboard, with one-click remediation using the existing update flow. Requires a free Wordfence Intelligence API key configured on the control plane; the agent reports inventory only, and the feed lookup itself runs on the control plane.

= 0.55.0 =
* New: guided two-factor enrollment for WordPress site users. Once an operator requires 2FA for a role, an affected user is walked through scanning a QR code, confirming a code, and saving backup codes on their next login, or can start enrollment proactively from their profile.

= 0.54.0 =
* New: optional, per-site, off-by-default two-factor authentication for WordPress site users (authenticator app, email one-time code, or single-use backup codes), with configurable grace logins and a remember-this-device window. The control plane and wp-config recovery constants can always bypass enforcement so an operator can never be locked out.
* New: optional password policy (minimum strength, known-compromised-password check via a privacy-preserving prefix query, reuse blocking, optional expiry with a forced-change screen).
* New: optional hide-login, which moves the login page to a secret per-site address.

= 0.53.0 =
* New: file integrity monitoring. A scan (core files, wp-content, or the full install) compares file hashes against WordPress.org checksums for core and wp.org-hosted plugins and themes, and against a learned per-site baseline for everything else, flagging changed, added, or removed files.

= 0.52.0 =
* New: per-site WordPress hardening controls (disable file editor, restrict XML-RPC and the REST API, restrict login identifiers, force unique nicknames, block author-archive user enumeration, force SSL/HSTS, disable directory browsing, block PHP execution in uploads, protect system files). All off by default and opt-in; the control plane and the operator's own session can never be locked out by a hardening rule.
* New: a per-site ban list (blocked IP addresses, CIDR ranges, and user agents), enforced at early boot and at the web-server config level. Broad blocks covering all addresses or private/loopback ranges are rejected, and the operator's own allow-listed IPs always bypass the ban.

= 0.51.2 =
* Changed: the early-boot security helpers (the login-protection ban list and the Error Monitor fatal-error trap) are now installed as a must-use plugin file only once you turn on the corresponding feature, and removed again the moment you turn it off or deactivate the plugin. A freshly activated agent writes nothing outside its own plugin folder until you opt in to one of these features.

= 0.48.2 =
* Fixed: one-click "Log in to wp-admin" no longer triggers a second two-factor challenge on sites running another two-factor plugin; it now lands directly in wp-admin. Sites running a security plugin that replaces the login flow entirely now get a clear "sign in normally" message instead of a loop.

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
* New: multiple named email connections with per-connection encrypted credentials, per-sender routing, and automatic fallback retry. The agent routes outgoing mail by matching the FROM address to a connection key, falls back to the default connection, and retries once via a configured fallback connection when the primary send fails. The email log records the connection key actually used, plus attachment names and sizes.

= 0.35.0 =
* New: per-site email delivery and logging. Route this site's outgoing email through Amazon SES, SendGrid, Mailgun, Postmark, or any SMTP server, configured from the WPMgr dashboard. Every send is logged (with optional bounce and complaint suppression), and known-bad addresses are skipped automatically. Email sending is unchanged until you configure a provider.

= 0.34.0 =
* Changed: one-click wp-admin login is more reliable and now lands past common two-factor prompts. The login token still expires, is single-use, and is bound to the site and your role.
* Changed: site connection status is steadier. The connection indicator no longer briefly flips to "degraded" on healthy low-traffic sites, and a "Re-check connection" action forces an immediate refresh from the dashboard.

= 0.33.9 =
* Hardening for WordPress.org guidelines: request inputs (including server and cookie values) are sanitized; the media quarantine and database-snapshot data now write under the uploads directory (with a read fallback to the legacy location so existing installs keep working); the diagnostics info REST endpoint binds its signed token to this site and endpoint; the login-screen branding style is enqueued; and the readme now documents every external service and the public source of the bundled scripts. No change to backups, cache, or other behavior.

= 0.33.8 =
* Fixed: WooCommerce cart-fragments now inject reliably on themes whose body tag has attributes; previously the shim only matched a bare body tag and skipped injection. Cart totals refresh correctly on cached catalog pages.
* Fixed: cache hit-ratio now counts 304 Not Modified and HEAD responses served from cache, and hit/miss counts are no longer lost if a stats upload to the control plane fails.

= 0.33.0 =
* New: Real User Monitoring (RUM), per-site and off by default. When enabled, the agent injects a tiny first-party measurement script into cached pages; the visitor's browser sends anonymous Core Web Vitals (LCP, INP, CLS, FCP, TTFB) and page-load timing directly to your control plane. No cookies, no cross-site identifiers, the page path is stored with the query string stripped, and the visitor IP is never stored. See the Privacy section.

= 0.32.0 =
* New: self-hosted font subsetting (experimental, default off). Discovers fonts loaded via external stylesheets in addition to inline font-face rules, and reports per-font conversion progress to the dashboard. Subsetting and transcoding run on the control-plane service, not inside the plugin.

= 0.31.1 =
* New: incremental backup engine with a content-addressed chunk store, and incremental chain restore.
* New: WOFF2 font transcoding. TTF, OTF and WOFF are converted on the control plane; the flag defaults to off.

== Upgrade Notice ==

= 0.61.139 =
Email delivery failures are now detected on sites that send through their own SMTP setup, not only sites routed through WPMgr, and a plugin bug that reported a failed send as successful is fixed: wp_mail() now returns false on a failed send. Forms and other flows that check the result of wp_mail() will start correctly reporting failures they previously hid. Safe to update in place.

= 0.61.131 =
Fixes a bug that could silently delete this site's working SMTP password when its email settings were saved, tested or synced from the dashboard, breaking outgoing email. Update now if this site sends mail through SMTP, SES, SendGrid, Mailgun or Postmark; the dashboard restores the password automatically on its next sync with this site.

= 0.61.127 =
Password policies can now target the roles your site actually has, including roles added by WooCommerce, membership, LMS and booking plugins, shown in the site's own language. Safe to update in place.

= 0.61.114 =
Updates and rollbacks on one site now run strictly one at a time, so two WordPress installers can no longer run against the same site at once and corrupt an update in progress. Safe to update in place.

= 0.61.112 =
Fixes outgoing email failing completely when a plugin sets a Reply-To, To, Cc or Bcc header in the "Name <email@example.com>" form, which WooCommerce and many form plugins do by default. Update if this site sends mail through SMTP or SendGrid.

= 0.61.95 =
Failed backups no longer leave their working files on the site, which could be more than a gigabyte, and the cleanup no longer depends on WP-Cron. Safe to update in place.

= 0.61.85 =
Fixes full backups failing at the upload stage on slow servers. The agent now reports progress during long archive and encryption passes. Safe to update in place.

= 0.61.25 =
Fixes a restore bug that could silently drop plugin or theme files whose path contained a reserved drop-in name, leaving a restored site broken while reporting success. If a restore on an older version left a site broken, re-restore the same snapshot on this version.
