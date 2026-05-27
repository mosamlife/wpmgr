=== WPMgr Agent ===
Contributors: wpmgr
Tags: management, backup, updates, monitoring, security
Requires at least: 6.0
Tested up to: 6.7
Requires PHP: 8.0
Stable tag: 0.0.0
License: MIT
License URI: https://opensource.org/licenses/MIT

Connects this WordPress site to a self-hosted WPMgr control plane.

== Description ==

WPMgr Agent securely links your site to a WPMgr control plane for centralized
backups, bulk updates with rollback, uptime monitoring, and vulnerability
scanning. Communication is authenticated with Ed25519-signed requests; no
telemetry runs by default.

== Installation ==

1. Install and activate the plugin.
2. In your WPMgr dashboard, generate a one-time pairing code.
3. Paste the code into the agent settings to enroll the site.

== Changelog ==

= 0.0.0 =
* Bootstrap scaffold.
