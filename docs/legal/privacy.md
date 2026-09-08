# Privacy Policy

_Last updated: 9 June 2026_

WPMgr is open-source, self-hostable software for managing a fleet of WordPress
sites — backups, updates, performance, and security. This Privacy Policy explains
what data the WPMgr agent plugin transmits, and what the optional hosted service
at **manage.wpmgr.app** collects when you choose to use it.

The full source code is public at
**https://github.com/mosamlife/wpmgr** (the agent plugin is MIT-licensed; the
control plane is AGPL-3.0). You can self-host the entire stack and keep 100% of
your data on your own infrastructure.

## Private and self-hosted by default

The WPMgr agent plugin has **no default endpoint** and sends **no data anywhere**
until you connect it to a control plane that you choose and configure. The plugin
is inert on activation. If you point it at a control plane you self-host, your
data never reaches us — you operate the receiving service and your own policies
apply.

## What the agent sends, and only to your control plane

Once you connect a site, the agent communicates **only** with the control-plane
URL you configured, and only for the management actions you (or your schedules)
initiate. The agent is the transmitter for all management data listed below.
The one exception is the optional Real User Monitoring feature, described in the
next section, where the site visitor's browser — not the agent — transmits
anonymous performance data directly to the control plane; it is off by default.

- **Site and environment metadata** — site URL, WordPress / PHP / server
  versions, active theme and plugins, and Site Health diagnostics. Used to show
  your site's status.
- **Update inventory** — the list of available core, plugin, and theme updates.
- **Backup archives** — when you run or schedule a backup, the agent creates an
  archive of your database and/or files and sends it to the storage destination
  configured for the site. The default is a WPMgr-managed bucket; it can instead
  be a bucket you own or a folder on the site's own server. Archive contents may
  include your site's content and personal data. Archives are not encrypted on
  your server before they are sent, so they are protected by the access controls
  and encryption at rest of the destination they go to.
- **Rendered HTML** — for used-CSS optimization, the agent submits rendered HTML
  of selected pages so unused CSS can be computed.
- **Diagnostics and activity logs** — error logs, performance/cache statistics,
  and a record of management actions, so they can be surfaced in the dashboard.

Every agent request is verified with an Ed25519 signature tied to the key
established when you enroll the site. The agent does not execute arbitrary remote
code; it accepts only a fixed, named allow-list of commands.

## Real User Monitoring (when you enable it)

Real User Monitoring (RUM) is the **one exception** to the agent-as-sole-transmitter
model above. It is a separate, opt-in data flow that does not go through the agent.
When you enable RUM for a site, the agent injects a small, public measurement
script into that site's pages. The **site visitor's own browser** then sends
anonymous performance measurements **directly** to the control plane you
configured (on the hosted service, **manage.wpmgr.app**). RUM is **off by
default** and is enabled per site.

This introduces a new data subject: **your site's visitors**. What their browser
sends is deliberately minimal and anonymous:

- **Performance measurements** — Core Web Vitals (LCP, INP, CLS) plus TTFB and
  FCP, and page-load timing.
- **The page path, with the query string stripped** — tokens, emails, and order
  IDs that appear in query strings are never transmitted.
- **Coarse, non-identifying context** — browser and device type derived from the
  User-Agent, connection type (4G, 3G, etc.), and an approximate country code.
- **No cookies, no localStorage, no cross-site identifier, and no stored full IP
  address.** The visitor's IP is used only transiently for rate-limiting and the
  coarse country lookup, then discarded; it is never stored.

Because RUM transmits from the visitor's browser rather than from the agent,
**you (the site owner) are the data controller for your visitors' RUM data** and
must disclose this collection in your own site's privacy policy, the same way you
would for any analytics or performance tool. If you self-host the control plane,
this data stays entirely on your own infrastructure. On the hosted service, it
is processed by us on your behalf as described below.

## The hosted service (manage.wpmgr.app)

If you use the hosted WPMgr service rather than self-hosting, we also process:

- **Account information** — your name and email address, used to operate your
  account and send transactional email (verification, password reset, alerts).
- **The site data described above**, on your behalf, to provide the dashboard,
  backups, and management features you use.
- **Encrypted backup archives**, stored in cloud object storage.
- **Anonymous Real User Monitoring measurements** from your site visitors, if you
  enable RUM, processed on your behalf as the operator of the hosted service. We
  do not use this data to identify individual visitors.
- **Operational logs** needed to run and secure the service.

## What we do not do

We do **not** sell your data, and we do **not** share it with third parties for
advertising. The only sub-processors involved in the hosted service are our cloud
infrastructure provider (Google Cloud Platform — hosting and encrypted storage)
and a transactional email provider. Self-hosted deployments involve no
sub-processors at all.

## Security

- Agent-to-control-plane requests are Ed25519-signed and replay-protected.
- Backups are not encrypted on your server before they are sent. They are
  protected by the access controls and encryption at rest of the destination
  configured for the site, which by default is a WPMgr-managed bucket.
- All network traffic uses TLS. A backup sent to a folder on the site's own
  server does not cross the network at all.

## Your data, your control

- **Self-host** to keep all data on infrastructure you control.
- **Disconnect** the agent (or deactivate the plugin) at any time to stop all
  data transmission immediately.
- **Disable RUM** per site at any time to stop browser-originated performance
  beacons immediately.
- On the hosted service you can request access to, export of, or deletion of your
  account data by contacting us.

## Contact

Questions about this policy or your data: **mosam@mosamgor.com**.

## Changes

We may update this policy as the software evolves. Material changes will be
reflected here with a new "Last updated" date.
