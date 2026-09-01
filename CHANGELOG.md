# Changelog

All notable changes to WPMgr are documented here.
Format: Keep a Changelog (keepachangelog.com). Versioning: SemVer (semver.org).
House rules: no em dashes, no en dashes, no competitor names. Use "to" for ranges.

## [Unreleased]

## [0.61.153] - 2026-09-01

### Security

- A connection scoped to a subset of a tenant's sites could learn that the tenant held more sites than the connection was allowed to see. The assistant's site list read a fixed-size page across the whole tenant and then filtered it, so a caller receiving every site it was entitled to could still be told that sites had been withheld. The read is now bounded over the connection's own scope, so the page it receives can never carry a fact about the wider tenant. **This was live in 0.61.151 and 0.61.152.**
- The same read now also runs under the database's own site-scope policy, rather than relying solely on the application-level filter above. This is one boundary enforced twice against different implementation mistakes, not two independent protections.

### Added

- A typed partial-result format for fleet-wide assistant questions: a response can now say that some sites could not be answered and why, with a stated reason per site and a timestamp on any answer drawn from stale data. A site outside a connection's scope is left out of the result entirely rather than named in a refusal, because naming it would itself disclose that it exists.

### Changed

- **Client-visible.** The fleet site-listing tool is renamed from `list_sites` to `fleet_sites_list` to match the published tool catalogue. A client re-reads the tool list when it reconnects, so this is picked up automatically; a client that has cached the old name will get an ordinary "no such tool" refusal rather than a silent alias.

## [0.61.152] - 2026-09-01

### Added

- An operator can now see which capabilities each AI connection holds, directly on the connections list. That set was already stored on every grant but never rendered, so there was no way to audit what a connection could actually do without querying the database.

### Fixed

- A site no longer reports a WordPress core update with no version to update to. A blank target version was being read as "an update is available," which left the site detail page's core row showing an empty target and inflated the fleet's pending-update count.
- An audit failure on the assistant surface no longer comes back as an invalid params error blaming the caller. A genuine server-side fault writing the audit record is now reported as a server fault, not a client mistake.

### Security

- Audit on the assistant surface is now fail-closed, reads included: when the record of what the assistant read cannot be written, the read itself is refused rather than answered silently. This is a deliberate posture change for this surface, taken by owner decision.

## [0.61.151] - 2026-09-01

### Added

- A per-tenant switch that turns the assistant surface off entirely, independent of any single connection's grant (m130).
- The read capability vocabulary a grant can hold widened from one member to the full v1 set (m131). The default a newly created grant receives is unchanged, so nothing already issued gains a capability it was not explicitly given.
- Tool calls on the live MCP endpoint now carry a per-connection and a per-tenant rate limit. A refusal names both the sustained rate and the burst allowance it is enforcing, rather than leaving a client to find the ceiling by trial and error.
- Connections now record the client the operator said they were setting the connection up for, distinct from the client name and version the connection later negotiates on its own, and return it alongside the grant.
- A grant's expiry can no longer be set more than one year past its creation. Nothing today asks for more than 90 days, so this closes an open-ended bound ahead of the caller that will.

### Fixed

- A capability refusal on the assistant surface now answers 403 with its own error code, naming the capability that was required and the ones the grant actually holds, instead of falling through to a generic 401 that told the client to re-authenticate. Re-authenticating could never have changed the answer, because the refusal was about the grant and not the credential. **Client-visible behaviour change**: anything integrating against the assistant surface that treated 401 as "renew and retry" needs to treat 403 as terminal instead.

### Security

- Every table the assistant surface can read now carries the same restrictive site-scope policy already enforced elsewhere in the schema: 22 policies added across 22 tables that previously had no database-level opinion of their own about which site a scoped grant could reach (m132). These protect every path that runs with a site-constrained principal, which is the ordinary way a site-scoped collaborator reaches this data.
- The chokepoint the assistant surface uses to resolve which sites a connection may reach now takes the caller's authenticated principal rather than a bare tenant id, so it is capable of engaging the site-scope policies above wherever that principal is site-constrained. On the assistant's own read path it is not: resolving a grant's site allowlist is deliberately done with a tenant-scoped principal, because the allowlist cannot be used to scope the query that produces it. The assistant surface's site scoping there is enforced by the resolved allowlist in application code, not by this database policy.

## [0.61.150] - 2026-08-31

### Fixed

- The consent screen now states the truth about how long a connection lasts. It previously said "This connection does not expire on its own. It lasts until you revoke it," and that the key a client holds is short-lived and renews itself. Every grant is stamped with a 90-day absolute expiry, enforced in the authentication lookup, and the connection token carries the same lifetime. The screen now states the term it is actually consenting to, supplied by the API rather than computed in the browser.

### Changed

- Agent worktrees are now excluded from the Cloud Build upload, so a build no longer uploads the repository once for every live worktree.
- ADR-062 and ADR-064 are now Accepted. ADR-062 is accepted with four checklist items open, converted from acceptance criteria to ship blockers, so no content-write code ships until each closes.

## [0.61.149] - 2026-08-30

### Added

- Connection tokens can now be minted directly from the dashboard: `POST /api/v1/mcp/connections`, plus a wizard that walks an operator through picking a client, naming the connection, choosing which sites it may reach, choosing how it authenticates, and revealing the token exactly once. This is the headless path, for the cases the browser sign-in flow cannot reach: CI, an SSH session, a container.
- A site-access explanation on the consent screen, stating plainly that scoping is a check made at the moment the assistant asks rather than a boundary enforced inside the database, and that a site added to a scoped tag later is included without anyone approving it.

### Changed

- Grants now carry an expiry, an idle expiry and a capability set, all enforced inside the same authorization check that already refused a revoked grant.
- The connections list now records real activity. It previously reported every connection as "never used," including one actively reading the fleet; it now reports the truth.
- The MCP surface now writes audit events for grant creation, revocation and tool calls, under a new assistant actor kind, inside the same transaction as the thing they record.

### Fixed

- A connection whose grant holds no capability now answers 403 rather than 401, so a client stops re-running an authentication handshake that could not change the outcome.
- An empty site allowlist is now a valid thing to approve.
- A tag selection that only partly resolves is now refused rather than silently narrowed.

## [0.61.148] - 2026-08-30

### Added

- The AI connection surface is now usable end to end from the dashboard. `/ai` is a sidebar entry and the front door, and `/ai/connect` is a wizard that generates the configuration block for the client you pick. The per-client differences come from a tested table rather than hand-written snippets, so each published block is generated from a recorded entry carrying the date it was last verified, and a shape we have no source for is refused rather than approximated.
- A connections list, showing which AI clients are connected, what each one negotiated, and when it was last used. "Never connected" and "connected but sent no protocol version" are kept distinct, and a list that failed to load is distinguishable from an organisation with no connections.
- Revoke, from that list.
- OAuth discovery documents (RFC 8414 and RFC 9728), so a GUI client with no field for an authorization endpoint can find one for itself. The issuer is derived from the configured public base URL rather than a constant, and an unset or unparseable value returns an error naming `WPMGR_PUBLIC_BASE_URL` instead of serving a document that names the wrong host.
- The production load-balancer routing configuration now lives in this repository (`infra/urlmap.yaml`), with a CI check that proves every route the API actually mounts has a rule pointing at the API backend. The route list is dumped from the real engine, never hand-kept.

### Changed

- Revoking a connection now revokes its tokens in the same step, so the client stops working on its next request rather than continuing until its token expires.
- The bundled nginx configuration and the development proxy now forward the MCP endpoint and the OAuth discovery paths. These are mounted at the root rather than under `/api/`, and were previously answered by the dashboard web app, so an operator who copied the endpoint out of the connection wizard got a page of HTML and no client could complete a handshake.
- **Self-hosted action, if you run your own reverse proxy** rather than the bundled one. Forward these four paths to the control plane; all four are root-mounted, so a rule that only forwards `/api/` will miss every one of them:

  ```
  /mcp
  /.well-known/oauth-authorization-server
  /.well-known/oauth-protected-resource
  /.well-known/oauth-protected-resource/mcp
  ```

  The last is not covered by the one above it: RFC 9728 inserts the resource path after the well-known segment, and current clients try that form first. Prefer exact-match rules over a `/.well-known/` prefix, so an ACME HTTP-01 challenge served from the same host keeps working. The bundled `infra/nginx/nginx.conf` already does all of this and needs no action. Without these rules the connection wizard will hand you an endpoint that returns your own dashboard.

### Security

- Organisation-scope enforcement is now applied to the AI connection authorization endpoints. Upgrading is recommended for every install that has the AI connection surface reachable.
- **The `WPMGR_SESSION_SECRET` rotation guidance from 0.61.147 still stands and is not superseded by this release.** If you are upgrading from 0.61.146 or earlier you have not seen it yet: read the "Self-hosted upgrade action" note in the [0.61.147 entry](#061147---2026-08-30) below before rotating, because there is one prerequisite that has to come first or stored secrets become unreadable.

## [0.61.147] - 2026-08-30

### Added

- A read-only MCP connection surface, so an AI client can be granted scoped read access to a fleet. It ships end-to-end: connection storage, the OAuth authorization flow (dynamic client registration, PKCE, an explicit consent screen, token exchange), the Streamable HTTP transport with protocol version negotiation, a first fleet-read tool, and a tool registry that applies per-connection policy filtering. A client that requests no recognised scope is refused rather than granted a default. Every tool call is checked against the registry in its own right, so a tool that was filtered out of the listing cannot be invoked by naming it directly.
- The AI-connection consent screen. It shows what a client is asking for before anything is granted, marks a client's self-declared identity as unverified rather than presenting it as established, and sends a refusal back to the client instead of leaving it waiting.
- Governed per-site and organisation context: five screens (effective-context preview, site context editor, organisation context editor, and version history with diff and restore), the API and resolver behind them, and the tables they read. Where a layer's contribution could not be loaded or had to be truncated, the screens say so instead of rendering an incomplete answer as a complete one.

### Changed

- `PATCH /auth/me` now returns the caller's scope and role alongside the rest of the profile, so a client no longer has to make a second call to learn what the account it just updated is allowed to do.
- Authentication rate limits now derive the client address from the deployment's proxy configuration rather than assuming one topology. The number of proxies in front of the control plane that append to `X-Forwarded-For` is set with `WPMGR_AUTH_PROXY_HOPS`. **The default suits the hosted topology and is not correct for the bundled compose deployment**, which needs `1`; the shipped `.env.example` already carries that value, and [docs/install.md](docs/install.md#proxy-hops) explains how to count it for any other deployment. Self-hosters running behind their own reverse proxy should set this deliberately rather than leave it to the default.

### Fixed

- Network-activated plugins now report as active in site metadata. On a multisite network, a plugin activated for the whole network was reported inactive, so it was missing from the inventory and from anything that reads it.
- The site inventory no longer presents "never collected" as though it were a collection date. Inventory age is now stamped from when the component list was actually gathered rather than from the site's last heartbeat, and a site with no gathered inventory renders an explicit unknown state. A failure to load the inventory is now distinguishable from an inventory that is genuinely empty, in the API and on screen.
- Email webhook deduplication is now scoped to the tenant it belongs to.
- The bundled compose deployment now takes the control-plane and media-encoder credential values from the environment file it is given, so a value set there reaches the service that needs it instead of being shadowed.
- `make test-integration` now serialises across worktrees with a machine-wide lock, so two runs on one machine no longer collide over the same database. Affects contributors to this repository, not the running system.

### Security

- The control plane now validates `WPMGR_SESSION_SECRET` at boot and refuses to start on a value that is unfit to hold confidentially, naming the variable in the error rather than starting and behaving oddly later.
- **Self-hosted upgrade action.** Rotating `WPMGR_SESSION_SECRET` is recommended as part of this upgrade. Two things to know before you do. Every operator is signed out once and signs in again, which is expected. More importantly, **if `WPMGR_SITE_DEST_AGE_SECRET` is empty, the secrets-at-rest key is derived from `WPMGR_SESSION_SECRET`**, so rotating the session secret on its own would leave stored secrets (operator two-factor enrollments, SMTP passwords, backup-destination and object-cache credentials) unreadable. Pin an explicit `WPMGR_SITE_DEST_AGE_SECRET` first, then rotate. See ["Pin your secrets"](docs/install.md#pin-your-secrets) for the pinning step and the recovery path if a key has already moved underneath stored data.
- Upgrading is recommended for every self-hosted install.

## [0.61.146] - 2026-08-26

### Security

- The application-password two-factor control now actually runs. It was registered against a name that is a core WordPress function, not a hook anything fires, so WordPress silently stored the callback and never invoked it, on any site, ever. It now registers on the hook core actually fires once a supplied application password has matched a stored one. **Breaking change for integrations**: on update, a site with two-factor authentication enabled will have application passwords stop working for any user who has a second factor enrolled or whose role requires one, returning HTTP 401 to the caller. That is the intended fix, not a regression (GH #523).

### Fixed

- A database restore could silently drop the dump's last SQL statement while still reporting success. When the restorer could not tell whether the unterminated tail end of a dump was nothing but SQL comments, it treated that uncertainty as "yes, only comments" and discarded the fragment. Proven end to end against two otherwise byte-identical dumps that differed only in whether the final statement ended with a semicolon: the one that landed in the discarded tail lost a row, silently. A restore that cannot classify its own trailing fragment now aborts loudly instead of finishing and reporting success (GH #525).
- A database restore could also write straight into the site's live tables while still reporting success. The step that stages a restore under a temporary table prefix decided whether a dump statement named a table by treating "the pattern-match engine gave up" (a backtrack limit exhausted on a host with tight PHP `pcre.*` settings) the same as "this statement names no table," so on an engine giving up, an unstaged statement naming a live table replayed straight into the live database instead of a scratch copy, and the restore reported success with nothing to roll back. It now aborts the restore rather than guessing. Proven end to end against a real database: a sentinel row seeded in a live table is confirmed to survive the abort, and confirmed to have been silently overwritten before this fix. Anyone who has run a restore on this plugin should know both of these restore bugs existed (GH #531).
- The database tools' orphaned-data scan could fail outright with a fatal error every time it actually ran, calling a global function WordPress does not provide in place of the equivalent method it does provide. It now calls the correct one (GH #525).
- Resending a failed outgoing email now actually works. The control plane and the agent had never agreed on what a resend command needs to carry, so every resend attempt failed with "missing required field: agent_seq," shown to the operator verbatim, on every connected site. A failed resend attempt no longer increments the "resent" counter or writes an audit entry claiming the email was resent; both now move only after a confirmed send (GH #520).
- Resend could send a different email than the one selected, after a site's database restore. The resend command's only selector, `agent_seq`, is a local auto-increment id on the site's own email log table; a restore rolls that counter back, so a later resend could re-use an id the control plane had already bound to a different message, and the site would resend whatever now sat at that row while reporting a clean success. A resend command now also carries the provider's own recorded message id, and the site refuses the send outright when the row at that id no longer matches it. The check is additive: a site running an older plugin still resends exactly as it always has, it simply cannot be confirmed. When a resend cannot be confirmed, whether because the site's plugin cannot yet run the check or because no message id was ever recorded to check against, the dashboard now shows the operator the specific reason instead of a plain, unqualified success, for a single resend and across a bulk resend alike (GH #528).
- Editing a user's profile in wp-admin no longer fails with a critical error. WordPress does not always hand the agent's hooks the object type they expect on a profile edit, and five callbacks assumed one unconditionally; the crash hit every profile edit on every connected site, whether or not a password policy was configured (GH #521).
- The disk-size probe behind Site Health's Directory sizes panel, the agent's own daily size walk, and every media upload on a multisite network no longer crashes when its cache is cold, which is any fresh install, any cache or object-cache flush, or the first read after either (GH #521).
- The documented recovery constant for an admin locked out by this plugin's own auth policy (`WPMGR_DISABLE_SITE_2FA`) now also releases the unique-nickname rule, and now also releases the automatic login lockout described below. It previously silenced the password-policy crash above but still left both other locks blocking recovery (GH #521, #529).
- A generic, operator-defined user-agent ban could 403 the site's own login page, with no recovery path. The ban matched as an unbounded substring against every request from WordPress's earliest hook, with no minimum pattern length and no check for genericity, so a pattern naming a common browser engine (a substring of nearly every real browser's user-agent string) turned the login page into a 403 for every visitor, administrators included. The site's login page (and a hidden login address, if one is configured) is now exempt from user-agent bans, and the configuration boundary now refuses a pattern that is too short or that matches ordinary browsers outright (GH #529).
- Release manifests no longer advertise a stale WordPress compatibility ceiling. The published "tested up to" value was a fixed default nobody ever overrode, so every release advertised WordPress 6.8 as the tested ceiling regardless of what the plugin package itself declared. It is now read from the release package's own declaration, and a release that cannot determine its own compatibility ceiling now fails to publish instead of guessing (GH #515).
- The cache toggle could destroy a site's stored CDN credentials. Enabling or disabling the page cache, or rotating a site's beacon key, could not distinguish a genuine read failure on the site's encrypted CDN credentials from "no credentials stored," so a transient failure silently replaced real credentials with nothing while the endpoint reported success; every later CDN purge then silently did nothing until an operator noticed and re-entered the key. Control-plane only, and already deployed (GH #522).
- A parser-configuration bug had kept the agent's own static analysis from ever completing a run. Completing it for the first time is what surfaced the two fixes above, plus several smaller correctness fixes with no live symptom (GH #525).
- That analysis now also runs automatically in CI, and distinguishes a run that aborted partway through from a run that completed and found nothing, so an incomplete analysis can no longer pass as a short, clean list of findings. Affects contributors to this repository, not the running system (GH #525).

## [0.61.144] - 2026-08-22

### Added

- An API key can now be granted least privilege instead of a whole role. Keys previously carried a single rank-ordered role, so granting an assistant permission to read files transitively granted managing members, minting further API keys, reading the audit log, and logging into sites as a user. A key now carries an explicit capability set, and can additionally be restricted to named sites (GH #510). Existing keys are unchanged and keep exactly the access they have today. The site restriction is enforced in the application, not in the database; the migration says so in place, and database-level scoping is a separate change that has not shipped.

### Fixed

- The API contract no longer declares nullability with a keyword the spec version it claims does not have. `packages/openapi/openapi.yaml` declares OpenAPI 3.1.0, which removed `nullable`, and used it in 163 places. The TypeScript generator ignored the keyword outright, so those fields were published to consumers of the generated client as always present when the control plane genuinely sends JSON null. All of them now use the forms 3.1 does have: the union `type: [x, "null"]` for scalars, and an `anyOf` with a null branch for the `$ref` and `allOf` shapes the union form cannot reach. Anyone generating a client from this spec was being told a value would always exist when it might not (GH #479).
- `make gen` regenerates both client trees or fails loudly (GH #511). It previously printed a message about being wired in a later phase and exited 0 without generating anything, so editing the spec and running the documented command left a stale generated tree that looked correct. Each generator is now proved to have written, each is bounded by a deadline, and a new CI job fails when the committed trees do not match a fresh generation. Affects contributors to this repository, not the running system.
- The plugin now declares compatibility with WordPress 7.1 (GH #514). The listing said 7.0, which wordpress.org treats as grounds for excluding a plugin from search results. This is listing metadata only: no plugin code changed and the agent version does not move. The declaration reaches the public listing when the plugin is next published to wordpress.org, which this release does not do.

## [0.61.143] - 2026-08-20

### Fixed

- An agent self-update run halted by the kill switch, or by a withdrawn release manifest, could be overwritten to `completed` when a task already in flight finished afterward: the run status write carried no precondition against writing over a terminal `halted` or `expired` run. Operators saw a rollout they had stopped reported as finished. `SetUpdateRunStatus` now refuses to move a run off `halted` or `expired` (GH #482, #490).
- A backup snapshot's claim (`pending` to `running`) and its terminal outcome (to `completed` or `failed`) were each a blind update keyed only on `(id, tenant_id)`, so a late failure could land on a snapshot already completed, or the reverse, with nothing stopping either. That leaked chunk storage permanently, since no retention rule selects a failed snapshot for cleanup, and could leave a good backup refused for restore. A late manifest submit against a cancelled or already completed snapshot also returned success while writing nothing; it now returns not found and the transaction rolls back. All three writes are now guarded by the snapshot's current status (GH #458, #497).
- `events.Listener` released its Postgres connection back to the shared application pool on cancellation without issuing `UNLISTEN` first. `LISTEN` is session state, so the connection returned to the pool still subscribed, and the next unrelated caller to acquire it could receive notifications meant for the cancelled listener. The listener now unsubscribes before releasing the connection, and hijacks it out of the pool entirely when the `UNLISTEN` cannot be confirmed (GH #496, #504).
- Thirteen integration test files turned any container start failure, not only an unreachable Docker daemon, into a skip, so a bad image pull, low disk space, or a resource limit read as `SKIP` and the suite still printed `ok` having asserted nothing. A container start failure is now fatal; only a positively probed unreachable Docker daemon still skips (GH #501, #503).
- A rules file and an agent definition each claimed a `PreToolUse` hook still blocks an edit to an already applied migration and backs up the generated tree permission rule. That hook, and the rest of the shell guards, was removed on 2026-08-14; `.claude/settings.json` configures no hooks at all today. The three stale claims now say what still enforces what and point at `docs/harness.md` (GH #471, #505). Affects contributors to this repository, not the running system.

## [0.61.142] - 2026-08-20

### Added

- New endpoint `GET /api/v1/fleet/uptime-history` returns a measured 90-day availability strip, one entry per UTC day, in place of a figure the dashboard previously derived from a single 7-day number (GH #460).

### Changed

- The probe-retention window the new uptime strip depends on is now pinned, and the retention guard that enforces it derives from that same window instead of a separate value (GH #460).

### Fixed

- A scheduled update run no longer dispatches to a site that was paused after the run was scheduled (GH #463, #492).
- Scheduled database cleaning, which deletes rows from a customer's own live database, and the vulnerability digest now both decline a paused site the same way (GH #493, #494).
- A site with no measurement now reports uptime as null instead of 0%; previously a site that had never been probed rendered as 90 days of solid outage (GH #460).
- Session advisory locks are now released on a detached, bounded context. Three call sites, all reachable by an ordinary graceful shutdown or a River job timeout, could leak a session-scoped lock because the deferred unlock ran on an already-cancelled context and silently did nothing: while leaked, scheduled backups stopped fleet-wide, per-tenant chunk GC stopped, and `DELETE /orgs/{orgId}` / `POST /orgs/{orgId}/restore` blocked until the pooled connection recycled, up to 30 minutes later (GH #483, #495).
- The webhook dedup GC worker is now registered with River; it was written but never wired in, so it has never run (GH #461).
- Email GC registration now fails startup instead of being silently skipped when it is handed a nil worker.
- Removed a migrations stub that advertised a capability that was never built (GH #462).
- Removed dead RUM hourly/daily fold paths and an unreachable ClickHouse store.

## [0.61.141] - 2026-08-19

### Added

- A new endpoint, `POST /api/v1/updates/runs/{id}/cancel`, cancels a scheduled update run before it fires (GH #463). It only succeeds while the run is still `scheduled`; once dispatch has claimed it, stopping it means using the existing halt path instead, because work may already be on real sites by then. Cancelling terminalizes the run and every one of its tasks in one transaction, and a second cancel of an already-cancelled or already-started run returns an error rather than a silent no-op.

### Changed

- Scheduled update runs now actually wait for their scheduled time instead of running immediately (GH #463). A run submitted after this release with a future `scheduled_at` is held until that time and then dispatched; a run submitted to run now is unaffected. This does not apply retroactively: a run created before this release was enqueued immediately regardless of its `scheduled_at`, so upgrading does not put any existing row on hold. If one of those older runs is still sitting unfinished, that reflects a stranded enqueue from before this change, not a deferral, and this release does not alter it.
- Update runs and tasks carry new statuses that the API and the dashboard now show: runs can be `scheduled`, `dispatching` or `expired`; tasks can be `scheduled` or `expired`. Anything consuming the API programmatically should handle all of them.
- A scheduled run whose start time passed more than two hours ago is marked `expired` rather than dispatched late, and the dashboard says plainly that nothing was sent to any of its sites.

### Fixed

- `scripts/init-env.sh` could report failure on a successful, fully-configured re-run: its cleanup step's own exit status silently overrode the script's own `exit 0` on that path (GH #467). Re-running it against an already-configured `.env` now exits 0 as it should.

## [0.61.140] - 2026-08-19

### Changed

- The control plane now validates its update-timing configuration at startup and refuses to boot when `update.apply_http_timeout` (env `WPMGR_UPDATE_APPLY_HTTP_TIMEOUT`) is set high enough that the derived claim-staleness bound reaches the stale-task reaper's threshold, naming the variable to lower in the refusal message. Measured against the real loader: an `apply_http_timeout` of 33m7s boots, 33m8s refuses. The default (8m) has roughly 25 minutes of headroom, so no ordinary install is affected, but anyone who raised the timeout will find out at startup rather than at deploy time.
- Self-hosted installs now require `WPMGR_BOOTSTRAP_CLAIM_SECRET` to establish first-run ownership. `scripts/init-env.sh` generates it, and both quickstart paths route through that script. An existing install that already has an owner is unaffected. An existing install that was stood up and never claimed needs the operator to re-run `scripts/init-env.sh` (idempotent, it fills only the missing key) or set the variable directly, then restart the `api` service; see "First-run notes" in `docs/install.md` for the claim command.
- `DECISIONS.md` and the ADR index were repaired: a false boundary in the record was corrected and `docs/adr/README.md` was added as the index. This affects people reading WPMgr's own decision history and changes nothing about installing or running it.

### Fixed

- Claiming an update task could race: two workers could each observe the same non-terminal task row and both dispatch it, applying one item to a site twice. Claiming a task is now a compare-and-swap against the row's own status, so only one worker can win it.

## [0.61.139] - 2026-08-17

### Added

- Email delivery failure detection now also listens to WordPress's own mail-failure signal, so a site is covered even when it sends through its own SMTP setup instead of through WPMgr (GH #381). Previously WPMgr could only see a failure on mail it routed itself, while the Notifications page promised detection generally; a site sending through its own SMTP configuration could have per-failure alerts turned on and never receive one. The Notifications page now states how many connected sites can actually report a failure, and says so plainly when none can, rather than promising coverage it cannot deliver.
- A detected failure is now recorded even on a site that has email logging turned off. That preference controls whether successful sends are kept for review; a failure is an incident, not a record of mail that worked, so it is written regardless. The recipient, subject, sender and message body are withheld from the record unless the site has opted into email logging; only the fact that a failure happened is kept.

### Fixed

- `wp_mail()` reported success on a failed send: the filter WPMgr uses to route outgoing mail returned a truthy value on a provider failure, so any caller, a contact form, a password reset, a WooCommerce order email, was told the message went out when nothing was delivered, and WordPress's own mail-failure hook never fired (GH #439). `wp_mail()` now returns false on a failed send and fires that hook itself with the same error shape WordPress's own failure paths use, omitting the message body and any secrets. Forms and flows that check the return value of `wp_mail()` will start surfacing delivery failures they were previously hiding; that is the intent of this fix, not a new fault introduced by it.

## [0.61.138] - 2026-08-16

### Fixed

- The "Monitoring paused" badge was illegible in dark mode: an amber outline around text you effectively could not read, and on some sites the pill looked completely empty (GH #414). The badge has no warning-colored fill, only a border, so its text always sits on the ordinary card surface, and it was using the color meant for text sitting directly on a solid warning-colored background instead of the color meant for warning-tinted text on an ordinary surface. Measured contrast on the dark surfaces where the badge appears ranged from 1.0:1 to 1.14:1; at the low end the text was rendered in exactly the same color as the surface behind it. It now uses the correct color, and contrast on those same surfaces ranges from 11.1:1 to 12.65:1, well past the 4.5:1 minimum for readable text.

## [0.61.137] - 2026-08-16

### Added

- Monitoring can now be paused on a site, from the site's row menu, from its own page, or in bulk from the sites list (GH #414). A pause takes a reason and, optionally, a resume time, and both are shown wherever the paused state appears, in all three places. Pausing stops uptime probes, uptime alerts, the weekly screenshot fanout, and scheduled vulnerability rescans. It does not stop backups, the WP-Cron kick, connection checks, RUM, or retention, and it never stops anything a person clicks by hand, including an on-demand rescan or update check: pausing stops the schedule, never the operator. Resuming happens by hand, or automatically at the stored resume time if one was set, exactly once either way, and both the pause and the resume are recorded in the audit log.

### Changed

- The monthly report's uptime section is now keyed on whether a pause actually overlapped the reporting period, not on whether the site happens to be paused when the report runs. A period a pause never touched is measured and shown in full, even for a site that is paused today. A period a pause partly covered is shown with a "Partial coverage" note naming the hours that went unmeasured, instead of being presented as a complete month. A period a pause covered in full still names the site and states that monitoring was paused and why, rather than showing an empty section. If the pause history itself cannot be read, the report says coverage is unconfirmed rather than defaulting to a clean bill, so a read failure can no longer look like a fully measured month.

## [0.61.136] - 2026-08-15

### Fixed

- The "28d distribution" bar on the performance tab's worst-offenders table did not show a site's real data. The cell read only the row's rating word and looked up a fixed set of percentages for it: every site rated needs-improvement showed 40% good, 45% needs-improvement, 15% poor, and every site rated poor showed 10% good, 30% needs-improvement, 60% poor, regardless of that site's actual numbers. The table only lists sites rated poor or needs-improvement, so those two pictures were the only ones the column ever produced, across every site and every time window. The sample count shown beside the bar was real, which made a fabricated bar look measured. It shipped as an acknowledged placeholder on 2026-06-15, and the code comment recording that was deleted the same day while the placeholder itself stayed; it was live for about two months. If you drew a conclusion about a specific site from that bar during that time, the picture it showed did not reflect that site. Nothing was stored wrongly, no other number in the product was affected, and no action is required beyond upgrading.
- The bar now shows the real histogram for whichever metric, LCP, INP or CLS, produced the row's rating, built from data the product was already gathering. It shows "Insufficient samples" instead of a bar when a site does not have enough of that metric's data to be meaningful.

### Changed

- The distribution bar now names the metric it is showing rather than being labelled "Overall". A site's LCP, INP and CLS distributions have no single combined meaning, and "Overall" implied a share of pageviews good on all three, a figure this product does not compute.

## [0.61.135] - 2026-08-14

### Security

- The Go toolchain moves from 1.26.5 to 1.26.6, which closes seven vulnerabilities in the Go standard library that the binaries published with the previous release still carry. None of them is remote code execution. Five are denial of service: missing recursion-depth limits in the XML and the ASN.1 decoders, where deeply nested input drives the decoder down until it exhausts the stack; a routine in the URL package whose cost grew quadratically with the length of its input; a header timeout that was not applied on one server path; and an unbounded number of messages accepted after a TLS handshake has completed. The other two are input validation rather than exhaustion: a name-encoding check on outbound requests, and escaping context tracking in the HTML templating package. That distinction is what should decide your urgency, because the realistic exposure here is a process that stops answering rather than one that runs somebody else's code. In this codebase the affected packages are reached through the object-storage client, the database connection pool, outbound HTTP, every outbound TLS connection, and the HTTP server itself, so they sit under code handling input from outside your control plane. Rebuilding on the newer toolchain is the whole fix. No WPMgr code changed.
- There was nothing here for an operator to have noticed. No code changed and no behaviour changed on either side of it: a live advisory database was updated overnight, and the same commit that passed CI in the evening failed the next morning. So whether a build of the previous release carries these depends on when it was built, because the base image tag floated until this release pinned it, and a running install shows no symptom that would tell you which you have. The toolchain a binary was built with is readable out of the binary itself (`go version -m` against the `wpmgr` binary inside the image), which is the only reliable way to tell what you are running.

### Changed

- The published images now name an exact Go version instead of a floating one. The API and media encoder images built from `golang:1.26`, which resolved to whichever patch release that tag happened to point at on the day the build ran, so the compiler that produced a shipped image was not recoverable from this repository and could move without anyone deciding it should. Both now pin the exact version, as the development Compose overlay does. If you build these images yourself you get the same compiler CI does, and a toolchain change becomes a commit somebody made rather than something that happened to you.
- Internal development tooling for this repository was removed: a set of shell guards that ran against contributor sessions. This affects people working on WPMgr itself and changes nothing about installing or running it.

## [0.61.134] - 2026-08-14

### Security

- An administrator in your organisation could change the owner's role, or remove the owner from the organisation outright. Both actions are gated on the permission to manage members, which an administrator holds, and on the remove path that was the only check made: the acting person's own role was never read at all. The change path compared the acting person only against the role being granted, never against the role the person being changed already held, so nothing anywhere compared an administrator to an owner. An organisation with a single owner looked like it refused, but that refusal came from the separate guard that stops the last owner going, not from any question of rank; add a second owner and both calls succeeded and did exactly what they said. Both now read the target's membership first, before any other branch, and refuse when the acting person does not outrank it. An owner acting on another owner is still allowed, because that is how ownership is handed over. An administrator acting on another administrator, or on anyone below, is unchanged.
- An administrator could create an API key carrying the owner role, which is an owner-grade credential obtained without going near the members page. The role was taken straight from the request with nothing comparing it to the person asking, so an administrator could ask for the owner role and hold a key with it a moment later. Asking for a role above your own is now refused outright rather than quietly lowered to one you may have, so a script that requests something it is not entitled to gets an error instead of a key that silently does less than it believes. The default is unchanged and is still operator, and an owner creating an owner-role key is still supported and still works.
- Existing API keys are NOT revoked by this release. A key created with the owner role while the above was possible keeps working, with that role, until somebody revokes it. Upgrading closes off how new ones are made and does nothing about the ones that already exist, so reviewing them is a step you have to take yourself. `GET /api/v1/api-keys` lists your organisation's keys with the role each one carries, and `DELETE /api/v1/api-keys/{apiKeyId}` revokes one; the same list and the same revoke control are on the API keys page in the dashboard. Revoke any owner-role key that an administrator created. Which account created a given key is recoverable from the audit log, which has recorded the creating account against the key id since long before this release, with two limits worth knowing before you rely on it: that record is written on a best-effort basis, so a failed write leaves a key with no trail, and a hard purge of an organisation takes its audit history with it.

### Fixed

- Two people demoting two different owners at the same moment could leave your organisation with no owner at all. The guard that refuses to remove the last owner counted the owners and then wrote the change in separate database transactions, so both requests could read a count of two, both conclude that one owner would be left, and both go ahead. The race itself is old; what changed is what it costs you. An organisation that lost its last owner used to be able to repair itself from inside the product, by promoting somebody or by creating an owner-role key, and those are precisely the two routes the refusals above close, so the same accident would now need direct database access to undo. Reading the roles, checking them and writing the result now happen in one transaction, which takes an organisation-wide lock before it touches any row, so two of these cannot interleave. The refusal itself is unchanged and is pinned by its own test, so making it atomic cannot quietly switch it off.
- The dashboard no longer offers the owner role to somebody who is not an owner. On the members page and on the API keys page the option appeared on a general "can manage members" test, which an administrator passes, so the dashboard was offering an action the server now refuses. The API keys page had a second form of the same problem: the form would still accept and send the owner role even once the option was hidden from the menu, so hiding it was not on its own enough. Both are now decided by the viewer's own role. The refusals the server returns for all of this are shown as readable messages rather than a generic failure.
- An owner can now hand ownership over. An owner sees a co-owner's row as an ordinary row and can change or remove it, which is what lets an organisation that has reached two owners return to one. An administrator still sees the owner's row as plain text with no controls on it. A previous change had removed the owner option from the members page altogether, which closed the escalation by deleting the only means anywhere in the product of nominating an owner; that is restored, for owners only.
- An owner cannot act on their own row, so stepping down takes two people. An owner promotes somebody else to owner, and that person then demotes or removes the first. Demoting or removing yourself is not possible from the dashboard, and that is deliberate rather than an oversight: it is what stops an owner demoting themselves into an organisation with nobody in charge of it. It is pinned by a test so it cannot be relaxed by accident.

### Changed

- Pushing to `main` in a clone of this repository is refused by a committed git hook. This affects people working on WPMgr itself and changes nothing about running it (`make hooks` installs it, `make hooks-status` reports whether it is live).

## [0.61.133] - 2026-08-12

### Fixed

- Deleting an empty organisation no longer strands its deduplicated backup chunks in storage forever. Deleting an organisation with no sites and no other members removes it immediately, and that same statement destroyed the stored inventory of its backup chunks while freeing no storage at all, so those objects were left named by nothing: not by the collector, whose list of accounts comes from that inventory, and not by you, because the account id was gone too. Delete an account's last site and then the emptied account, and its chunk storage was unreachable permanently. The delete now records what it left behind, in the same database transaction it deletes the account in, and an hourly drain frees every one of that account's storage folders afterwards. Same transaction is the point, exactly as it was for site deletion: the record exists if and only if the account really went, and it deliberately has no link back to the account, because a record of cleanup work that is removed along with the thing it describes is the bug it was written to prevent.
- Chunks shared with a live site in a DIFFERENT account are safe from that drain by construction, not by argument. Chunk storage is namespaced per account and deduplication never crosses an account boundary, so two accounts holding byte-identical WordPress files hold two separate objects, and draining one cannot reach the other. Before deleting anything the drain re-checks that the account, its sites and its chunk inventory are ALL still absent, and refuses if any of them came back, which is what makes a restored database backup or a staging control plane pointed at a production bucket safe rather than lucky. It works in bounded batches and only marks the work done when a fresh listing of every folder comes back empty, so an interruption resumes instead of reporting a half-finished cleanup as complete. If it cannot finish, or is refused by one of its checks, it keeps the record and says so on every sweep: that record is the last thing that knows those objects exist, so it is never discarded.
- The recovery commands for stranded storage now work on the connection you actually have. Both statements shipped for this were written for a database superuser: as the ordinary application role the insert was refused outright, and the update was worse, because row-level security hid the row and the statement reported success while changing nothing at all. There is now a supported command family, `wpmgr-cli reclaim`, which lists outstanding work, hands a deleted site or a deleted organisation to the sweeps, and reopens a stuck record. It runs as the ordinary application role, it changes no permission or policy to do so, and every subcommand exits non-zero when it changed nothing, which is the property that makes it a recovery path rather than another thing that claims success having done nothing. The stuck-record message in the logs now names that command instead of printing a statement that cannot work.
- For organisations already deleted before this release, `wpmgr-cli reclaim backfill-tenants` finds them from the deletion trail the control plane already keeps, which survives the deletion it describes, and queues each one. Organisations removed by the separate superadmin cleanup of orphaned accounts left no such trail; for those `wpmgr-cli reclaim discover --report-only` reads the bucket and prints candidates whose account no longer exists, and it never deletes and never queues anything, so a person decides and the drain's own checks still apply to whatever they hand it.
- The published `api` image now contains `wpmgr-cli`. The install guide has been telling you to run it inside that image since long before this release, and the image only ever contained the server binary, so that instruction could not work.

## [0.61.132] - 2026-08-11

### Changed

- The docs version guard is a script you can run, `scripts/check-version-surfaces.sh`, instead of shell embedded in the CI workflow, and it ships with `scripts/check-version-surfaces_test.sh`: 76 cases that build throwaway trees and assert the guard's exit code. Running the guard before you push now takes one command and no CI cycle. Four defects found by review are closed and covered by tests: a version with no `CHANGELOG.md` heading is now placed in the ordering rather than skipped (24 patch versions of 0.61.x had no heading, so a stale pin could sit on one of them forever), the badge and agent checks survive a reformat instead of switching themselves off, a commented-out install pin no longer counts as present, and required pins are anchored to an explicit marker so ordinary prose and an extra current pin cannot fail the build. A repo-wide sweep also holds any other concrete image tag or `WPMGR_VERSION` value to the same freshness rule, so a new file with a stale pull command is caught even though no list names it.

### Fixed

- Deleting one site no longer strands its backup manifests in object storage forever. The delete cascaded away every snapshot row for that site, and those rows were the only record naming the site's stored manifest files, so nothing could ever find them again: one deleted site left 90 orphaned objects in the field. The delete now writes a reclamation record in the same database transaction it deletes the site in, and an hourly sweep clears the site's storage folder afterwards. Same transaction is the point: a record written separately beforehand could survive a delete that then failed, which would leave a standing instruction to erase a live site's backups. Deduplicated chunks are deliberately untouched by that sweep. They are shared across every site in the account, they sit under a different storage root, and the sweep is structurally unable to reach them, so a chunk another site still needs cannot be deleted by this path however it goes wrong. If the sweep cannot finish, or cannot prove the site is really gone, it leaves the objects where they are and keeps the record: a stuck record is the last thing that knows those files exist, so it is never discarded, and it is now re-reported in the logs on every sweep rather than mentioned once at the moment it gave up and then never again.
- Objects already orphaned before this release are NOT cleaned up by it, and nothing in this release finds them. The reclamation record is written by the delete, so it only exists for sites deleted from this version onwards. Sites you deleted on an earlier version left no record anywhere, which is the whole defect, so those manifests are still in your bucket and still on your storage bill: the account that reported this has 90 of them right now. Clearing them is a deliberate step you take yourself, against `tenant/<tenant id>/site/<site id>/`, one folder per deleted site, with the literal `site/` segment in the middle. Rather than deleting by hand you can hand a known-deleted site to the sweep, which keeps every safety check in play, including its refusal to touch a site whose record still exists: run `wpmgr-cli reclaim site --tenant <tenant id> --site <site id>`. (This release shipped a hand-written insert here instead, to be run on a database superuser connection, because as the ordinary application role the row-level security on that table refused it outright. That command is the replacement and works as the ordinary role; see GH #408 below.) Get the kind field wrong and the database refuses it, on the spot, naming the constraint, because a record the sweep cannot act on is the only thing that knows those files exist.
- A site whose backups go to your own storage bucket has its manifests swept like any other. Manifests are always written to the bucket this instance itself is configured with, whatever destination the backup payload was sent to, so they are all in scope and none of them are stranded. What this instance does not touch is the backup payload sitting in your own bucket, which it holds no credentials for; the reclamation record carries which destination the site used purely so the log can say which of the two it was.
- An account that lost its last backup-carrying site stopped being garbage collected at all, so its stored chunks leaked as well as its manifests. The collector's account list came only from completed backups, and deleting the site holding the final one removed the account from that list permanently. It now also considers accounts that still have stored chunks, which is enumeration only: every existing safety check on what may actually be deleted is unchanged, and a backup running at the time still protects everything it touches. This helps while the account itself exists. Deleting the emptied organisation afterwards still strands its chunks, because the chunk inventory is removed with the organisation and deleting an empty organisation frees no storage: that is GH #408 and is deliberately left open here.
- The `backup_chunks` schema comment described a deletion rule that was retracted a long time ago, that a chunk is removed once its reference count reaches zero. Reference count has not decided a deletion since the mark and sweep collector landed, and the count can legitimately sit at zero while a live backup still needs the chunk, so the comment was wrong in the direction that loses data. It now describes the rule the code actually follows and points at it.
- The README said that turning the media encoder off costs you site screenshots and the Media Optimizer and nothing else. WOFF2 font transcoding stops too; all three of those workers run only in that image. The install guide did not say so either, and now both do.
- The self-host install guide no longer hands you a stack from 190 releases ago. `docs/install.md`, which the README links as the full install guide directly under its own pull instructions, still told you to `export WPMGR_VERSION=v0.19.0`, so following the link rather than the README got you a control plane predating a long list of fixes. The README also described all three published images as multi-arch; the media encoder is `linux/amd64` only, because the image codec library it uses ships prebuilt libraries for that architecture alone. The install guide now also says what running without the media encoder actually costs: site screenshots, the Media Optimizer, and WOFF2 font transcoding, and nothing else.
- Changing what an account can sign in with is now recorded for every account, not only for accounts that belong to an organisation. Setting a first password, or disconnecting a provider, was skipped entirely when the account had no organisation, which describes a site collaborator, a portal user, a brand new account created by signing in with a provider, and anyone whose only organisation is inside its deletion grace window. The accounts with the least oversight were the ones whose credential changes went unrecorded.
- The audit log now says what actually happened when a provider is connected to an account, when an account is created from one, and when a stored provider record changes issuer. All three were filed as "Signed in with SSO", so a credential change was indistinguishable from an ordinary sign-in for anyone scanning or filtering the log.
- A provider that stops reporting an address no longer erases the last address it did report. GitHub reports nothing when a user makes their address private, and that empty report was written straight over the stored one, so the sign-in meant to keep the record current destroyed it instead. The login itself is still stamped.
- Signing in through a provider whose issuer differs only in capitalisation or a trailing slash now updates the stored record. The sign-in always worked, but the "last used" stamp and the provider's current address silently updated nothing, so a connected account could show as never used no matter how often it was used.
- A refused attempt to connect a provider to an unverified account no longer discards the plan that account signed up for. The verification mail it sends replaces the previous one, so sending it without the plan did not merely omit the plan, it destroyed it for good, and the account landed on the free plan after verifying.
- Signing in with Google is now bounded by a timeout, as signing in with GitHub already was. A Google endpoint that accepted the connection and then stopped answering could hold a request open indefinitely.
- Connecting a second account from a provider you already have connected now reports a conflict you can act on rather than a server error.
- A provider switched off between starting and finishing a sign-in no longer loses the page you were trying to reach.
- The list of available sign-in providers is now always a list, including on an instance with no provider configured, where it was previously null and did not match the published API description.
- The public base URL setting now refuses a value carrying a query string, a fragment or user info. Every URL this instance builds is made by appending to that value, so any of those produced verification links and provider redirects that could never work. A sub-path is still allowed.

## [0.61.131] - 2026-08-10

### Fixed

- Sending email from a site no longer stops working on its own, days after it was set up and used successfully. Saving, testing or syncing that site's email settings could send the site an empty password, which the site read as an instruction to delete the working one it already had. The site then tried to sign in to the mail server with nothing, the mail server refused, and what everybody saw was "SMTP Error: Could not authenticate", which is the same thing a wrong password looks like. That is why re-typing the same password fixed some sites: it put back what had just been deleted. The dashboard now says nothing at all about the password unless it has a real one to send, so a site keeps the credential it already holds.
- A site that uses your organisation's email settings rather than its own now keeps using your organisation's password. The moment anything was saved on that site's email page it got a settings record of its own with no password in it, and the password was then looked for only on that record, so a site that had never had its own password came away with none. Your organisation's password is only ever sent to a site whose mail server, mailbox user and encryption still match your organisation's; change any of those on one site and that site needs a password of its own.
- Clearing a site's email password now actually removes it from the site. Emptying the field recorded the removal in the dashboard but never told the site, so a password you meant to revoke stayed on the site indefinitely. The same is true of clearing your organisation's password, which now revokes it from every site that was using it.
- A site now refuses an email settings update it cannot read, instead of acting on the part it understood. A garbled update whose list of mail connections could not be made sense of was treated as though you had removed every connection, and the site deleted all of their passwords. The site now declines the whole update and keeps everything it holds, and the dashboard retries.
- A site now reports it as a failure when your email settings cannot be written to it. The password was stored first and the settings after, so a settings write that silently did not land left the site holding a new password alongside the old mail server, which it would then have offered to that server. The dashboard was told the update had succeeded.
- A save or an organisation-wide update is no longer sent to your sites when the stored password cannot be read back. It used to be sent anyway, without the password, which sites running the currently published plugin version read as an instruction to delete the one they had. One unreadable password could therefore have emptied the password on every site in the organisation. Nothing is sent now, and the failure is recorded for whoever runs the instance.
- The site email page now says when the password shown as configured belongs to your organisation rather than to that site, instead of offering "Configured" and "Replace" for a credential the site does not own. If you edit the mail server, mailbox user or encryption away from your organisation's, the page tells you a password for this site is needed before you save.
- A site with no email password now says so. It used to hand the empty password to the mail server and report whatever the mail server said, which made a credential this product had lost indistinguishable from one your email provider had expired. Sending now stops before that with "SMTP password is missing or unreadable on this site; re-enter it in the dashboard".
- "Send test" no longer reports a failure it caused itself. It pushed the site's settings before testing, so on an affected site it deleted the password and then reported that the password did not work.
- Moving a site to a different email provider, mail server or mailbox user without giving it a new password no longer hands the old password to the new destination. Keeping a password the dashboard could not resend was what stopped sites losing working credentials, but on its own it would also have let a password issued for one account be offered to another. The dashboard now tells the site to drop the stored password whenever it moves the account that password was issued for, and says nothing about the password when it is only re-sending settings the site already has.
- Removing the password from one of a site's named mail connections now actually removes it from the site. The dashboard recorded the removal and then said nothing about that connection's password the next time it contacted the site, which a site reads as an instruction to keep the one it already has, so a password you meant to revoke stayed on the site indefinitely. A connection the dashboard holds no password for now says so too, and one whose password cannot be read back still says nothing, because that is a key that changed rather than a password somebody removed.
- The startup check that warns when the secrets encryption key has changed now also looks at per-site email passwords. An instance with no confirmed two-factor user and no instance-wide SMTP settings had nothing to check, so it warned about nothing and the sites failed one at a time instead.

### Security

- Your organisation's email password can no longer be sent to a mail server chosen on a single site. Managing email is a per-site permission, so somebody invited to one site could name any mail server on that site's email page, save it without a password, and have the dashboard supply your organisation's password for them. A test send then had the site sign in to that server with it. Your organisation's password is now only ever paired with the mail server, mailbox user and encryption your organisation configured; a site pointed anywhere else is told to drop the password it holds and needs one of its own.
- Somebody invited to a single site can no longer change your organisation's email settings from that site's page. A site that uses your organisation's settings has none of its own, so its page was editing the organisation's record: the named mail connections every inheriting site sends through, and the web address your email provider reports deliveries and bounces to. Pointing one of those connections at another mail server, with no password given, would have carried your organisation's password across to it, and replacing the reporting address would have silently taken over the delivery reports for every site at once. Both now need access to the organisation. Give a site email settings of its own to change that site alone, which is also what somebody making a change for one site meant to do.
- A password saved against a named mail connection is no longer carried across when that connection is pointed at a different mail server, mailbox user or provider. It is dropped and a new one has to be entered, exactly as the main email settings already do, because a password issued for one account is not a password for another.
- The same now applies to a site's own email settings, and to your organisation's. Changing the mail server, the mailbox user or the encryption without giving a new password used to keep the stored one and offer it to the new destination, on a record the person editing it was entitled to edit. That mattered most on the organisation's record, which anyone who can manage email could edit even when the password had been entered by an owner or an admin above them. In both cases the password is now dropped and has to be entered again. Editing anything that does not move the account, a display name or how long history is kept, still keeps the password as before.
- The database now enforces on its own that somebody invited to a single site cannot change your organisation's email settings, its named mail connections, or your organisation-wide list of addresses that must not be emailed, and cannot read another site's email history. This is the same boundary the pages already apply, written down one level lower so it holds no matter which page, link or future change reaches for it. Sites that use your organisation's settings rather than their own can still see them, because those are the settings that site actually sends with. Nothing changes for anybody with access to the whole organisation.

## [0.61.130] - 2026-08-10

Finishes signing in with Google and GitHub. Nothing here changes how you sign in with an email address and a password, and nothing here needs anything done to an existing account.

Sign-in with a provider stays off until whoever runs the instance sets up that provider: both the client id and the client secret for Google or GitHub, in the instance's own configuration, at that provider's console. Until then no Google or GitHub button appears anywhere, on purpose, because a button that leads to a provider error page reads as a broken product rather than an unconfigured one. Single sign-on with your own issuer is separate again and equally optional. Upgrading changes the database (it runs when the new version starts, no step for you), and it does not sign anybody out.

### Added

- You are now told when a new way of signing in is added to your account. Connecting Google or GitHub to an account you already have still works without signing in first, because requiring that would put every returning user through a password reset to defend against someone who already needs both a provider that vouches for the address and an account this instance has verified at that same address. What was wrong was that it happened in silence. The message goes to the address this instance verified, names the provider and the time, and links to the page where sign-in methods are reviewed and removed.

- Account security settings now show which sign-in methods your account has, and let you disconnect one or connect another. Until now an account could accumulate providers with no way to see or undo it.
- An account that was created by signing in with a provider, and so has no password at all, can now set one from that same card. It can only be done while signed in. Password reset deliberately will not do it: a reset link that can create a password where there was none would turn "forgot password" into a way for anyone who knows your address to make an account theirs.
- Whoever runs the instance can now read the record of sign-ins that belong to no organisation, through the admin API at `GET /api/v1/admin/system-audit` (superadmin only; there is no screen for it yet). These are the accounts with the least oversight anywhere else: a brand new account created with Google or GitHub, someone who only collaborates on one site, a client with portal access, and anyone whose only organisation is inside its deletion grace window. Their sign-ins were being written down and then read by nothing at all.
- Operators who move single sign-on to a new issuer address now have a way to carry everyone's existing identity across: set `WPMGR_OIDC_PREVIOUS_ISSUER` to the address that was in use before. Without it, changing the issuer stops every single sign-on user on the instance being recognised on the same deploy, because who someone is is only meaningful within the issuer that said so. Each identity moves once, on that person's next sign-in, and the move is written to the audit log. Unset it again once everybody has signed in. Instances that have never changed issuer leave it empty, which is the default, and nothing about them changes.

### Changed

- `WPMGR_SUPERADMIN_EMAILS` no longer marks the listed address as verified. It grants the superadmin flag and activates the account, so the operator can still sign in with a password on an instance whose mailbox domain does not accept mail, and that is all it does now. Confirming an address means this instance watched someone open a link it sent there, which an environment variable is not, and that confirmation is half of what allows a provider-verified identity to attach itself to an existing account. Setting the variable therefore supplied that half on the most privileged account on the instance. Operators listed in it now confirm their address the same way everyone else does, and accounts confirmed under an earlier release keep their confirmation.

### Security

- Pressing a Google or GitHub button no longer makes this instance store anything. That button needs no account and no session, by nature: it is the first thing someone who has never signed in clicks. Each press used to leave a record behind for a week, so a single machine pressing it in a loop could fill the shared store that everybody's signed-in session lives in, and everybody would be signed out or unable to sign in. The handshake now travels in a short-lived, tamper-proof cookie in your own browser instead of a record on the server, so there is nothing left to fill. You will not notice the difference, other than that a sign-in left half finished for more than ten minutes now has to be started again. A handshake that was already in flight when the instance was upgraded has to be started again once, which means pressing the button a second time.

### Fixed

- Disconnecting a provider is now refused when it would leave the account with no way to sign in at all, and the refusal says to set a password first. That state had no way back: there would be no password to reset and no provider to sign in with. The check and the removal happen together, so disconnecting two providers at the same moment from two tabs cannot slip past it.
- A provider that cannot be reached now returns you to the sign-in page with a sentence saying so, instead of a page of raw error text with no way back. Every other failure on that route already did this; the one that fires when a provider or its configuration document does not answer was the exception, and since the instance stopped contacting providers at startup it is also the only place an unreachable provider shows up at all. Pressing a button for a provider this instance does not have set up now does the same thing rather than showing raw text.
- Connecting Google or GitHub to an account that has two-factor authentication turned on no longer attaches anything until the second factor has actually been entered. It used to attach as soon as the provider said yes, so getting as far as a provider consent screen was enough to leave a new way into an account nobody had finished signing in to. The connection is now held until the sign-in completes, and only the challenge that started it can complete it.
- Accepting an invitation now happens in one step that either finishes completely or does not happen at all. Claiming the invitation and granting the access were separate, and an invitation can only be used once, so anything going wrong between the two spent the invitation and granted nothing, with no way for the person it was addressed to to try again.
- An account that signs in with Google or GitHub can now accept an invitation. It has no password and never can have one, so the accept page's password field was a door with no key, and the only advice it could offer led straight back to the same refusal. Being signed in as the invited account is now accepted instead, along with an explicit press of the Accept button that a page on another site cannot forge on your behalf.
- Being invited at one of your addresses while signed in at another no longer destroys the invitation. The page offered the signed-in address as the only possible answer, every attempt was refused for not matching, and ten refusals kill an invitation permanently. The page now offers a way to use the address the invitation actually went to, and a mismatch that comes from your own signed-in address costs nothing, because it tells nobody anything they did not already know.
- Signing in with Google or GitHub no longer creates a fresh organisation for someone whose only organisation is in the deletion grace window. Signing in with a password created nothing for the same person, so the two routes disagreed about what an account belonged to, and the new empty organisation quietly undid a deletion nobody asked to undo.
- An account created through Google or GitHub is now written in one piece. The account, its confirmed address and the provider link were three separate steps, and a failure between any two of them left the address occupied by an account with no way into it, which then blocked the person who owns that address from ever using it.

## [0.61.129] - 2026-08-07

### Added

- You can now sign in and sign up with Google or GitHub. Both are optional and set up separately by whoever runs the instance, so an install that configures neither carries on with email and password only and shows no extra buttons.
- Signing up with a provider skips the verification email: the provider has already confirmed the address, so the account works immediately. It also supplies your name, which is one reason the signup form no longer asks for it.
- Connecting a provider to an account you already have requires that both sides have confirmed the address: the provider must vouch for it, and this instance must have seen you open a verification link. If the second half is missing we send that link at the moment you try, so the message tells you exactly what will unblock it.

### Fixed

- A disabled user could still sign in through single sign-on. Signing in with a password had always refused disabled and unverified accounts; the single sign-on route refused neither, so switching a user off in the admin area did not actually keep them out. It does now, on every route.
- Single sign-on could attach an identity to an account whose email address nobody had ever confirmed. Because anyone can register an address without proving they own it, that let someone claim an address ahead of its real owner and keep access to the account the owner then signed in to. Connecting now requires the address to have been confirmed on this instance first.
- Both of the above came from the sign-on rules existing twice in the codebase, once for each route, and only one copy being kept current. There is now a single set of rules that every provider goes through.
- Accounts that were never sent a verification email, which includes the first account on a new install and everyone added by invitation, had no way to confirm their address at all. They can now request the link, which previously only worked for accounts still waiting on their first one.

## [0.61.128] - 2026-08-07

### Changed

- Creating an account now asks for an email address and a password, and nothing else. The form also collected a display name, an organisation name and an organisation slug. All three were optional, none was needed to create the account, and every one of them is editable in settings afterwards, so they only ever added decisions to the screen where people are most likely to give up.
- An account's organisation is now named from the signup email instead of being called "Default". A work address gives the organisation, so an address at acme.com creates "Acme", while a personal mailbox gives the person, so sarah.jones at a consumer provider creates "Sarah Jones". It is a starting point rather than a claim to be right, and it is renamed in settings like any other. Accounts created before this release keep the name they have.
- The sign-in, sign-up, password reset and email verification pages now show what the product does alongside the form, instead of putting a form on an empty page. On a phone the form still comes first and nothing sits above it, because someone who came to sign in should not have to scroll past an explanation to reach the field they came for.

## [0.61.127] - 2026-08-06

### Fixed

- The password policy on a site's Security page only ever offered the five roles a stock WordPress install ships with: administrator, editor, author, contributor and subscriber. On a WooCommerce store the roles that matter are the ones the store added. A shop manager can edit orders, refund customers and see every buyer's address, and there was no way to require a strong password of them, because there was no way to select them. Reported by an agency running a WooCommerce site whose roles are a shop manager, a translator, two customer tiers and a staff role, none of which appeared. The policy now offers the roles the site actually has.
- The same applies to membership, LMS and booking plugins, which routinely add their own roles, and to any role an agency created by hand for its own staff.
- Enforcement was never the problem and has not changed. The agent has always applied a policy to whatever role a user really holds, so a rule naming a shop manager would have worked from the day it was written. What was missing was any way to write it.
- Role names now read the way they read on the site itself. An Italian site shows "Amministratore" and "Gestore negozio", so those are the names in the policy, not their English originals. The rule itself is still stored against the underlying role identifier, so renaming or translating a role never changes who a policy covers.
- Where a name alone cannot identify a role, the role's identifier is shown next to it. Two plugins can each add a role called "Staff", and picking the wrong one is a silent mistake that leaves people ungoverned.
- A rule that names a role the site no longer has, because the plugin that created it was deactivated, keeps that role on screen and marks it as no longer present. It is not dropped. An operator can now see why a rule stopped applying, and can remove it.
- A site whose agent has not yet reported its roles still shows the standard WordPress roles, but says on screen that it is doing so and that plugin-added roles are missing from the list. The silent version of that fallback is what hid this problem. Updating the agent on the site, or re-checking the site from its page, loads the real list.
- Sites with a large number of roles stay workable: the list of roles is scroll-bounded and gains a filter box, and the number of roles carried per site is capped.

## [0.61.126] - 2026-08-05

### Fixed

- Searching the Sites list only ever searched the sites that page had already loaded, and that page is the fifty most recently added. An agency with more than fifty sites was told "no results" for a site it owns and can reach in two clicks, with nothing on screen to say the search had looked at part of the fleet rather than all of it. Reported by an agency running twenty four sites, where a second, unrelated problem made the same symptom look like one bug. Searching now happens in the control plane, across every site in the organisation, so a page of results is the best matches rather than the newest fifty filtered after the fact.
- This is why raising the number of sites fetched was not the fix. Filtering a list the server has already cut short is wrong at any size: it just moves the point at which the product starts quietly lying about what it searched. The filter and the cut now happen in the same place and in the right order.
- A search still matches a site's name, its address and its tags, the same three things it matched before, and still ignores case. It is a plain substring search: a percent sign or an underscore in the search box now looks for that character instead of behaving as a wildcard.
- Tags are searched from the same list of tags the fleet shows on the site, so a tag an operator can see on a site is always a tag that finds it.

### Added

- Sites can be ordered by name, by date added, or by last check-in, in either direction. The order is applied across the whole organisation before the page is cut, so the first page is genuinely the first page of that order and not the newest fifty rearranged among themselves.
- Ordering by name ignores case, so "Acme" and "acme client" sit next to each other instead of every capitalised name sorting ahead of every lowercase one.
- A site that has never checked in has no last check-in time to order by. Those sites sit at the END of the list in both directions of the last check-in order. They never take the top of a "most recently seen" list, and they never vanish from one either.
- Every order is settled down to the last row. Two sites that share a name, or that were added in the same second, keep a fixed position relative to each other, because paging through a list whose order is undecided between equal rows can show one site twice and skip another entirely.
- Nothing changes for anyone who does not ask for an order: the list is still newest first, exactly as before. An order the control plane does not recognise is refused outright rather than quietly ignored, because silently falling back would show an operator a list in a different order from the one the control says is applied.
- For self-hosted installs and API users, `GET /api/v1/sites` now takes `q` and `sort`, documented in the OpenAPI specification. They combine with the existing tag, status and client filters rather than replacing any of them.

## [0.61.125] - 2026-08-05

### Fixed

- A site's Uptime card could take up to thirty seconds to load the first time it was opened after a quiet period, and then load in half a second on every attempt after that. Measured over a week of production requests, the same page's fleet summary answered in a fifth of a second every single time, including on the page loads where the per-site card took four seconds or more. The cause was not the amount of history, an absent index, a busy database or a cold container: it was that the per-site view was still adding up every individual probe in the window by hand, about forty three thousand of them for a thirty day view, every time it was asked.
- The fleet-wide view stopped doing that some time ago. It reads a running per-day total that is kept up to date as each probe lands, and only looks at individual probes for the two part days at the very edges of the window, which is at most a couple of days' worth. The per-site view predates that work and never received it. It does now, using the same code to decide which days are complete rather than a second copy that could quietly disagree with the first, so the number on a site's Uptime card and the number next to that same site in the fleet views cannot drift apart.
- The reported uptime percentage is unchanged, to the decimal. A day that falls only partly inside the window is still counted only for the part that is inside it, so an outage that started an hour before a thirty day window opens still counts for exactly the minutes that fall within it, not for the whole day and not for none of it.
- The three separate lookups behind the card, the totals, the latest check and the chart, now happen at the same time as each other instead of one after another. They never depended on each other's results, so waiting for them in turn was simply waiting three times.

### Changed

- The uptime chart on a site now draws one point per day for any window of a day or longer, rather than a hundred points of whatever width divides the window evenly. For a thirty day view that is about thirty points instead of a hundred points seven hours wide each, which is the same information at a resolution the chart can actually show: at the size it is drawn, a hundred points were never individually distinguishable. Windows shorter than a day are untouched and still show every minute, because that is the entire purpose of looking at an hour.
- The average response time shown on a site's Uptime card is now the average across successful checks only, which is what the Sites list and the fleet dashboards have always shown for the same site. It previously also included the response times of failed checks, so a site with a spell of server errors had two different average response times in the product depending on which screen it was read from. The uptime percentage was never affected by this and is not affected now.

## [0.61.124] - 2026-08-05

### Added

- "Check now" for the agent release reference is now in the Agent column popover on the Sites page, directly under the freshness text, where the reporter asked for it. That is the moment it is wanted: reading "may be stale, last confirmed 14h ago" is exactly when an operator wants to act on it, and until now acting meant leaving the page for the admin console. 0.61.123 granted the permission to the owner of a single-organisation install but left the only button behind a console that same owner cannot open, so the feature was unreachable for the person it was built for.
- The button appears only for a viewer who may actually use it, and the dashboard does not work that out for itself. The fleet agent response now carries the control plane's own answer for the asking viewer, computed by the same code that decides whether the endpoint would accept the request. There is one decision, so a button that always refuses and a permission nobody is offered are both impossible, rather than merely unlikely.
- Nothing appears on an install with more than one organisation, which is every hosted account: the answer is false there for everyone except a superadmin, and a superadmin is redirected away from the Sites page anyway, so the admin console remains their route to the same action. The answer is also false whenever release mirroring is switched off, since there is no run to trigger at all then.
- The three outcomes read the same here as in the admin console, because both use the same code. Queued is a success, and neither "a check is already running" nor "the mirror must wait before its next request" is shown as an error: being skipped by the thirty minute spacing is the system working as designed, and dressing that up as a failure would be the same overclaim in the opposite direction.
- The button does not claim the check has happened. The control plane answers that a run was queued, not that anything was confirmed, so the wording says the result appears once the view refreshes.

## [0.61.123] - 2026-08-05

### Changed

- On an install with exactly one organisation, the owner of that organisation can now use "Check now" for the agent release reference (Admin > Agent mirror) without being made a superadmin first. Reported by a self-hoster who had to set `WPMGR_SUPERADMIN_EMAILS` and restart the control plane to click what is, for them, a monthly refresh of their own fleet's data, and then found that the seeding only ever adds the flag and never removes it, so getting back out again meant an `UPDATE` against the users table and a second restart. The permission on this action exists so that one organisation cannot spend another organisation's share of the install's shared, unauthenticated GitHub request budget. On an install with exactly one organisation there is no other organisation for that to protect, so what was left was the ceremony without the reason.
- The owner does not become a superadmin as a result. No environment variable, no restart, no flag written anywhere, and none of the side effects: every other admin action still refuses them, including the vulnerability feed sync, which stays superadmin only, and they are not redirected away from the Sites page the way a real superadmin is. This grants one action and nothing else.
- Nothing changes on an install with more than one organisation, which is every hosted account: the action stays superadmin only there, for exactly the reason it always has. The organisation count is read fresh on every request and is never cached, so creating a second organisation closes this path again on the very next call, with no migration to run and nothing to clean up.
- An organisation that has been deleted but is still inside its restore window does not count towards the total. Nobody can act as one: it is hidden from the organisation switcher, its API keys stop working, and its members cannot switch into it, so it has no share of the budget to protect. Restoring it makes the install a two-organisation install again and closes the path immediately.
- Only the owner role passes. An admin, operator or viewer in that single organisation is refused, as is an API key, including one belonging to the owner, because this is an install-level action and the audit record should name a person. A refusal reads identically whichever way it was reached, so it cannot be used to work out how many organisations an install has.

## [0.61.122] - 2026-08-05

### Fixed

- The warning heading inside the Agent column's information popover was almost unreadable in dark mode, rendering near black on the dark panel while the explanatory text below it was fine. It was using the text colour meant for content sitting on an amber background, which is deliberately near black in both themes, rather than the one for amber-tinted text on an ordinary surface. Reported with a screenshot.

### Added

- "Update agent on all sites" is now in the command palette, alongside "Run backup on all sites" and "Sync metadata on all sites". The fleet agent rollout had the same shape and the same audience as those two but was the only one that still needed selecting sites and opening a menu to reach. It opens the Sites page filtered to outdated agents rather than starting the rollout outright, because a wave-gated update that touches every agent in a fleet should not begin from a single keystroke, and it appears only for an owner or admin on an install where the rollout is actually enabled.

## [0.61.121] - 2026-08-04

### Fixed

- A site's Updates tab said "All up to date", with a green check, while that same site's own WordPress dashboard was offering a WPMgr agent update (GH #314). The tab only ever knew about the components WPMgr updates for you: plugins, themes and WordPress core. The agent itself is deliberately not one of them, so its update was never in the count, and the tab claimed everything was current when all it actually knew was that the managed components were.
- The tab now says "All managed components are up to date", and the badge beside the heading says "No managed updates", so neither one speaks for anything WPMgr does not update.
- The agent is now shown on that tab as its own line, whether or not anything else needs updating, with what this install actually knows about it: behind, current, or not determinable. It is deliberately not selectable and has no update button, because an agent update applied the way a plugin update is applied means the plugin overwriting its own running files inside the request that has to report the result, with no rollback armed for its own directory. Where the fleet agent update channel is turned on and you can use it, the line links straight to it. Where it is not, the line says to update the agent from that site's own Plugins screen instead. A site running the build from the WordPress plugin directory is never sent to the fleet channel at all, because that build ships without a self-updater and updates through the directory.
- When this install has no published agent release to compare against, the line says so rather than guessing. It never reports a site as behind on a comparison that did not happen, and where the comparison fell back to the newest agent version this fleet has reported, it says that too, in the same words the Sites list already uses.

## [0.61.120] - 2026-08-04

### Added

- An update run can now be retried from the run page itself, for agent, plugin, theme and core updates alike (GH #336). Retrying a run that failed used to mean going back to the sites list and re-selecting every site by hand, which is how a 21-site fleet agent rollout whose canary failed, correctly cancelling the other 20 sites without touching them, turned into 20 checkboxes to find again. The retry is sourced from the run, so the targets do not have to be re-picked.
- The retry defaults to the updates that never succeeded: the ones that failed, and the ones that were cancelled because an earlier failure stopped the rollout before they were attempted. A skipped update, and one that applied and was then rolled back, can be selected deliberately but is never included by default: a rollback means the update did apply and was taken back, so retrying it walks the same path and may reproduce the same break. An update that succeeded, or that has not finished yet, is never retryable at all.
- A retry always creates a NEW run and never alters the one it came from, so the failure that prompted it is still there to read afterwards.
- The retry response now accounts for every task it was asked about. If 20 were selected and 17 became work, the 3 that did not are each named with a reason: the site is no longer enrolled, the site no longer exists, the same target already has an update in progress in another run, or, for an agent rollout, the site is no longer behind the published agent version. Nothing is dropped quietly.
- Enrollment and the published agent version are re-resolved at retry time rather than copied from the old run. Reverting an agent release mid-incident is exactly what an operator is expected to do, and a retry that copied the old target would have upgraded sites to a build that had deliberately been withdrawn; instead those sites are excluded and say why.
- Retrying an agent rollout re-runs the whole staged rollout with a fresh canary. A retry has proven nothing about the new attempt, so it starts from one site again rather than dispatching every previously-cancelled site at once, and it cannot bypass that gate.
- Each task in a run now carries its site's name from the server. The run page used to resolve site names against a separately-fetched site list, which is paginated, so a run wider than one page showed raw ids for the overflow. Site identity for both display and selection now comes from the task itself.

## [0.61.119] - 2026-08-04

### Fixed

- Self-hosted installs that mirror our agent releases were refusing them, correctly, with "upstream republished the same version with different bytes". The agent's version only changes when the agent itself changes, which is deliberate: a release that only touches the dashboard should not push a new agent to every site in your fleet. But the archive was rebuilt on every release and was not byte-reproducible, because it recorded each file's modification time and the packaging step reinstalls the vendored libraries from scratch each run. So a dashboard-only release republished the same agent version with different bytes, and a mirror holding the previous copy of that version had no way to tell that apart from tampering. Four published releases carried four different archives all naming the same agent version. Packaging is now deterministic: the same source produces a byte-identical archive every time, verified by building it twice and comparing. A release-time check now also refuses to publish an archive whose bytes differ from an already-published release carrying the same agent version, so this cannot return quietly.
- The agent version moves to 0.61.119 in this release even though the agent's own code is unchanged from 0.61.118. It has to: the archive's bytes genuinely differ now that packaging is deterministic, and republishing the same version with different bytes is the exact thing being fixed. This is a one-time re-baseline onto the reproducible archive. From here on, a release that does not touch the agent republishes a byte-identical archive and no mirror has anything to object to.

## [0.61.118] - 2026-08-04

### Fixed

- A fleet agent update could report "the plugin update transient carried no entry for this plugin" and install nothing, on one site, while the same rollout installed cleanly everywhere else (GH #334). The apply looked up the build it was about to install in WordPress's shared plugin update cache, and that cache is one any other plugin on the site is allowed to answer for, rewrite or delete. A security or "disable updates" plugin that answers that read first, a managed host's own must-use plugin doing the same, or simply an ordinary plugin update finishing on that site at that moment, was enough to leave WordPress's installer with nothing to install, after which the rollout stopped at the canary and no other site was touched. The apply no longer looks anything up: it carries the build it verified moments earlier and hands that straight to the installer, so no cache and no other plugin sits between the check and the install. What actually gets installed is unchanged, and is still re-verified from scratch against the signed manifest before a single byte is written.
- The previous release had made this more likely, not less. 0.61.114 correctly made an agent update wait for any other update on the same site to finish first, and the process it waits for is exactly the one that clears the cache the apply was standing on, which widened the window from milliseconds to as much as four minutes. That window is now closed, because the apply no longer depends on that cache at all.
- The update offer shown on a site's own WordPress dashboard is now self correcting. An offer naming a build the fleet has since moved past is rewritten from the fresh signed manifest at the moment an install starts; an offer for a release that has since been withdrawn is retired the first time anything acts on it; and an offer the site has already overtaken, for example because its files were replaced out of band by a deploy or a restore, is retired by the first page load that sees it instead of standing for up to twelve hours. A control plane that is briefly unreachable retires nothing, so an outage can never blank a fleet's update offers.
- When a commanded agent update does fail, the site's own dashboard is now left holding a verified offer for the same build. The one-click update inside wp-admin is the recovery route for a build whose fleet update is broken, and it is now available immediately, without the site having to reach the control plane to rediscover what it was already told.

### Changed

- As with every fix to the agent's own update path, this one cannot be delivered by the path it fixes: a site still running an affected build applies updates using its own installed copy of the old step. A site whose fleet agent update is failing this way needs one update from its own WordPress dashboard, after which fleet updates work normally again.

## [0.61.117] - 2026-08-04

### Fixed

- Starting an update run could report a server error while the run had in fact been created. If the background job queue was briefly unavailable at the moment a run was saved, the run and its tasks were already written to the database, but the response threw them away and returned a failure. You were told nothing happened, while a real run sat there with tasks that nothing would pick up, and they stayed that way until the stale-task sweeper failed them 45 minutes later, or 6 hours later for an agent rollout. Starting the update again could then be refused, because the first run's tasks were still counted as in flight, so a brief queue hiccup looked like a broken product. The run is now returned as created, its tasks are visible on the run page, and the queue failure is logged for the operator.

## [0.61.116] - 2026-08-04

### Fixed

- The client portal showed Cumulative Layout Shift multiplied by a thousand. A perfectly good CLS of 0.1 was displayed to your clients as "100.000", directly beside a badge that correctly read "Good", so the number and its own rating contradicted each other. The score now reads 0.100, matching what you see on the operator dashboard and what every other report in the product shows. Timing metrics in the portal also now switch to seconds above one second, so a client and an operator looking at the same site read the same figure rather than "4200ms" in one place and "4.20 s" in the other.

## [0.61.115] - 2026-08-04

### Fixed

- The Core Web Vitals trend charts (Performance, and a site's Optimize tab) stated the wrong Good threshold (GH #329). The dashed "Good" line on the LCP chart was labelled 3 seconds. The real Web Vitals Good threshold for LCP is 2.5 seconds, and the line was always drawn in the right place; only the label was wrong, because the number was rounded to whole seconds before it was printed. Anyone who read that label and treated 3 seconds as the target was working to a threshold that does not exist. The same rounding mislabelled the FCP Good line as 2 seconds (it is 1.8) and the TTFB needs-improvement line as 2 seconds (it is 1.8). Every threshold label now shows its true value.
- The same charts printed the unit twice, so an axis label read "5sms" and "3sms" instead of "5s" and "3s", and the threshold labels read "Good 3sms" and "NI 4sms". This is the symptom that was reported.
- A single vertical axis could mix two different scales at once, showing "650ms" and "2sms" as neighbouring labels on the same axis, which made the values impossible to compare by eye. Worse, because the seconds labels were rounded to whole seconds, four different heights on one LCP axis could all print the same text. Each axis now picks one scale for all of its labels, and no two labels on an axis can read the same.
- On a site comfortably inside the Good band, neither threshold line was drawn at all, so there was no way to tell "this site is passing" from "no thresholds are configured on this chart". The Good line is now always in frame.
- The threshold lines were drawn in the same colour as the data line on the LCP and INP charts, so on those two charts the target was indistinguishable from the measurement. They now use the standard pass and warning colours, matching the fleet chart on the Performance page, and those colours are defined for dark mode (the previous ones were not).

### Changed

- The small trend sparklines in the fleet tables (Sites, Uptime, Backups, Email) are drawn directly rather than through the charting library. A fleet table showing 100 sites was building 100 full chart engines to draw 100 tiny decorations that have no axes, no tooltips and nothing to interact with; the tables now render an order of magnitude faster and the Uptime and Backups pages no longer download the charting engine at all. They look the same.

## [0.61.114] - 2026-08-03

### Fixed

- Two updates could previously be dispatched to the same site at the same time, for example two plugins updated in the same bulk run, or an update and a rollback overlapping, running more than one WordPress installer against the same site concurrently (GH #328). WordPress's own updater is not built for that: a second installer can delete files the first one is still relying on, corrupting the update in progress and, in the reported case, leaving the site briefly returning errors. Updates, rollbacks and agent upgrades against one site are now serialized: only one may run at a time, whichever channel it came from.
- A site that is busy with another update no longer fails the update that was turned away. It is retried automatically, with the reason shown on the task ("waiting: another update is running on this site"), for up to 6 hours before being recorded as not attempted rather than failed, and being busy never counts as a failure, so it can never fail a canary. If a whole wave was turned away for being busy, the rollout does still stop, because a site that never attempted the upgrade proves nothing about it; the difference is that it now stops saying nothing was attempted rather than reporting a failure that did not happen.
- Separately, when an update fails before it has touched anything on the site (for example a corrupted download), the site no longer runs an automatic restore over a plugin or theme directory it never modified.

### Changed

- Updating many items on one site is correspondingly slower, since they now run strictly one at a time on that site instead of several in parallel: a 30-plugin update on a single site that previously took roughly 6 to 12 minutes now typically takes 15 to 50 minutes. Updates spread across different sites are unaffected and still run in parallel.
- A brief window remains, at the moment a plugin or theme's files are actually swapped in, where a page load could in principle hit a half-updated directory. This is the same exposure WordPress's own core updater has always had for an admin-initiated single-plugin update, and this release does not add a maintenance-mode window to close it: with updates now serialized one at a time, that window is measured in microseconds, not seconds. A single plugin update started from a site's own WordPress dashboard still does not take part in this lock, so that one collision between a fleet-triggered update and a person using wp-admin at the same moment remains open.

## [0.61.113] - 2026-08-02

### Added

- The fleet Agent column's header popover (Sites page) can now say when the upstream agent release reference was last confirmed, instead of just showing a plain "current" badge computed against a reference that, on a self-hosted install running the upstream mirror, could quietly be hours behind (GH #322). GET /api/v1/fleet/agents now carries an agent_mirror object with the mirror's own status (ok, stale, pending, standing down, misconfigured, or disabled), the time of the last successful confirmation against upstream, and the time and outcome of the last attempt, kept as two separate facts on purpose: a run that failed a few minutes ago is never reported as "checked a few minutes ago" while an older confirmation sits behind it unmentioned.
- Superadmins on a self-hosted install with the upstream mirror enabled can now trigger an immediate check from the admin console (Admin, Agent mirror) instead of waiting up to six hours for the next scheduled one, via POST /api/v1/admin/agent-mirror/check. The action is install-level, not per organization, so it lives alongside the other instance-wide admin tools rather than on the Sites page. A request inside the mirror's request-spacing window is refused honestly with a wait time, never a false success, and a check already queued or running is reported as such rather than queuing a duplicate.
- The mirror never treats being rate limited as a failure: that outcome is recorded and reported separately from a real problem such as the upstream being unreachable or this install's own object storage failing to write, so an operator is never alarmed by an outcome that is normal and expected.

## [0.61.112] - 2026-07-31

### Fixed

- Outgoing email failed completely whenever a plugin set a Reply-To header in the ordinary "Name <email@example.com>" form, which WooCommerce, Fluent Forms and many other plugins do by default (GH #312). The agent stored that header exactly as written and then handed the whole string, display name included, to the mail transport as if it were the address. The transport rejected it as invalid and, because one bad address aborted the entire message, nothing was sent at all. The reported symptom was an "Invalid address" error in the email log with no mail going out. Addresses are now parsed properly wherever a bare address is required, the display name is kept rather than discarded, and a single bad address costs that one recipient instead of the whole email.
- The same defect applied to the To, Cc and Bcc headers, not only Reply-To, and affected the SMTP and SendGrid providers. It was reported for Reply-To because that is the header plugins most often set this way, but any of the four could trigger it. The Amazon SES, Postmark and Mailgun providers were unaffected, because those build a raw header where this form is already valid.
- A header carrying more than one address, for example a Cc listing two recipients, was treated as a single malformed address and lost the whole message on the SMTP and SendGrid providers. Address lists are now split correctly, including the case where a quoted display name contains a comma.
- A display name could redirect an email to a different address than the one shown. Because header values are commonly assembled by dropping a user-supplied name into a template, a name that itself contained an address in angle brackets could take over as the real destination while the intended address was still displayed. Any entry of that shape is now refused outright rather than delivered somewhere the site owner did not intend.

## [0.61.111] - 2026-07-31

### Changed

- Version bump only, with no functional change to the agent, the control plane or the dashboard. 0.61.110 removed a restriction that had stopped the fleet update channel from running on common Apache mod_php hosting. That restriction lived in the agent, so a site still running 0.61.108 or 0.61.109 refuses the update using its own installed copy of the rule, before it can ever install the release that removes it. The only way onto a fixed build is to update the agent once from the site's own WordPress dashboard, and this release then gives that fixed build something genuine to install so the update path can be exercised end to end. Sites will be offered an update whose only difference is the version number, which is safe to take.

## [0.61.110] - 2026-07-31

### Fixed

- Fleet agent updates no longer refuse to run on Apache with mod_php or plain CGI hosting, which is common on shared and self managed servers. The previous release added a check that declined to update the agent itself unless the web server could hand the connection back to WordPress before the file swap started, on the reasoning that a lost connection mid swap was unsafe on that kind of hosting. That reasoning did not hold up: WordPress's own plugin and core updater performs exactly the same file swap, protected by exactly the same safeguards, on that same kind of hosting every day, whenever an operator clicks "Update now" in wp-admin. The fleet update now runs that identical, already safe swap instead of refusing outright, so a site on this kind of hosting updates itself from the fleet dashboard the same way it already updates from its own wp-admin.
- Because the control plane now waits out the whole install on this kind of hosting instead of getting an instant acknowledgement, the time it is willing to wait for that one request was raised from 5 to 8 minutes, so a slower host has room to finish both the download and the file swap in the same request, not just the download.
- A control plane request that times out while an agent update is still applying is no longer recorded as a failed rollout. On this kind of hosting the agent's acknowledgement is only written after the whole swap finishes, so a slow answer is not evidence anything went wrong, it usually just means the swap is still running. The rollout now waits for the site's own report of the version it is running before deciding the outcome, exactly as it already does for an ordinary acknowledgement.
- A rollout's halt banner could read "The rollout was halted before any site could be contacted" for a site that was, in fact, contacted and answered, when all that actually happened was the site politely declining the update rather than failing or never receiving it at all. The summary now counts a declined site as contacted and says so plainly.

## [0.61.109] - 2026-07-31

### Changed

- Version bump only, with no functional change to the agent, the control plane or the dashboard. 0.61.108 rebuilt how a fleet agent update actually installs itself, and that path can only be tested by an agent that already has it: a site still running an older build applies updates with the old, broken step, so pointing it at 0.61.108 exercises nothing. Publishing a release that is identical in behaviour gives a site already on 0.61.108 something genuine to install, so the rebuilt path can be run end to end before any operator depends on it. Sites will be offered an update whose only difference is the version number, which is safe to take.

## [0.61.108] - 2026-07-30

### Fixed

- The fleet agent update's apply step has never actually replaced the agent's files on any site, in any release. WordPress's plugin upgrader finds the package it is about to install by reading the update_plugins transient, and the code that started the apply was deleting that same transient nine lines before the upgrader read it. With no offer left to find, the upgrader took its "nothing to do" branch and returned without touching anything, while the run still reported an acknowledgement upstream. Every fleet agent self-update to date has been a no-op that looked like progress. The apply now rebuilds the transient the same way WordPress's own background updater does, immediately before calling the upgrader, so the build this run verified is the build that actually gets installed.
- The apply now runs inside the same request that carries the control plane's command, instead of a separate WordPress cron event, and that change is what makes WordPress's own automatic restore of a failed update usable again. WordPress schedules its restore-from-backup and its cleanup to run at the very end of the request that performs an update; the previous design ran the apply from a point of its own, much later than that, past where WordPress's restore could still fire, so a swap that failed there had no rollback at all. The apply now runs earlier, right after the acknowledgement has been written to the response and the connection to the control plane released, which keeps it inside the part of the request where WordPress's own restore still runs. The earlier design, which staged an apply for a later WordPress cron event to pick up, is gone.
- The site's own maintenance mode now covers the swap, exactly as it does for any other plugin update: visitors see WordPress's maintenance page for the few seconds the plugin directory is actually being replaced, then the site serves normally again. That maintenance mode is guaranteed to clear on every outcome, including a crash partway through, so a failed apply cannot leave a site stuck showing it.
- A new outcome, reported as "sapi_cannot_detach", covers hosting where PHP has no way to release the connection back to the control plane before the swap starts. The apply only proceeds once that connection is confirmed released, which most hosts do through PHP-FPM or LiteSpeed; a host running plain mod_php or CGI offers neither, and running a multi-minute file swap on a connection nothing has actually released is not safe, since a proxy timeout or a process manager could still cut it mid swap. On that kind of hosting the agent now touches nothing on disk and records "sapi_cannot_detach" with a plain explanation and a next step. An operator who opens the task detail for a site like this finds that record rather than a rollout that silently never reaches it; the agent's own one-click update inside wp-admin is unaffected and still works there.

### Changed

- A fleet agent update reported success on its own confirmed version report, without checking whether the agent's own record of the upgrade actually named this run as the cause. In the ordinary case this made no difference: the version moved because this run's own command told the agent to install it. But if a site's version happened to move for some other reason while an unrelated apply record from an earlier attempt was still sitting on the agent, the control plane could credit that unrelated movement to a rollout that never touched the site, and a canary confirming a move it did not cause could open every later wave on evidence that was never real. The control plane now checks a per-apply identifier the agent stamps into its own outcome record and compares it against the one this run sent, so a version movement only counts toward a rollout's evidence when the agent's own record says this run is what caused it. A site whose agent does not yet report this identifier still confirms on its version report alone, exactly as before, since that is the only evidence such an agent can offer and the release that introduces the identifier has to be able to reach the fleet that does not have it yet.
- When a confirmation times out, the control plane already reads whatever apply record the agent left behind to help explain why. That explanation is now held to the same standard: a record that cannot be tied to the run that timed out is still shown in full, its status, versions and detail included, but it is no longer described as an account of what happened in this run. Reading someone else's leftover record as this run's own cause was a real failure mode this closes.

## [0.61.107] - 2026-07-30

### Changed

- Version bump only, with no functional change to the agent, the control plane or the dashboard. The agent's fleet update path was rebuilt across the preceding releases and the final piece, applying an update without depending on WordPress's scheduled task system, has not yet run end to end against a real site. Publishing a release that is identical in behaviour gives that path something genuine to install, so it can be exercised before any operator relies on it. Sites will offer an update whose only difference is the version number, which is safe to take.

## [0.61.106] - 2026-07-30

### Fixed

- A fleet agent update could report that a site had accepted the job, then nothing would happen, and the run would report twenty minutes later that it could not be confirmed. The agent asks WordPress to schedule the actual work for a moment later, in a separate request, and WordPress can decline that request, for example when another plugin on the site blocks scheduling. The agent was not checking whether the request was accepted, so it told WPMgr the work was scheduled whether or not it actually was. The agent now checks, and reports a clear error immediately if the work could not be scheduled, instead of leaving the rollout waiting for a confirmation that was never coming.
- The step that downloads and installs a new agent version ran only from WordPress's own scheduled task system. On a site where that system is blocked, unreliable, or simply never triggered, the update would never be applied, even though everything else about the site worked normally. WPMgr's other background work was made independent of that scheduler some releases ago for exactly this reason; this one step had not been. It now works the same way: it runs on an ordinary page request whenever an update is waiting, so a site with a blocked scheduler updates normally. The install itself still happens in a separate request from the one that starts it, which is deliberate, since the agent cannot safely replace its own files during the same request that has to report the result.
- Two of the situations where the install step decides not to proceed used to leave no record at all, so an operator could see only that nothing had happened. Every outcome is now recorded and reported back, so the dashboard can say why. The agent also now guarantees only one install can run at a time, even if two requests start at once.

## [0.61.105] - 2026-07-30

### Fixed

- The agent's self-update download could fail forever on a slower site. The download was bound by a single 60 second time limit covering the whole transfer, which for the current package works out to roughly 55 kilobytes per second sustained. The sites this feature exists to serve often download at roughly 25 to 40 kilobytes per second, below what that limit demanded, so they would download for the full minute, get cut off partway, discard the incomplete file because it failed its size check, and try again on the next check, failing the same way indefinitely. The limit is now 300 seconds, which asks for roughly 11 kilobytes per second for the same package, so a site downloading at 25 kilobytes per second now finishes comfortably. The control plane side of this was already fixed in an earlier release; this was the remaining limit on the path, and it was the binding one.
- Separately, nothing raised the PHP execution time limit while an agent update was being applied. Many hosts stop a script after 30 seconds by default, so on those hosts the update could be stopped partway even when the download itself finished within its own budget. The apply step now raises the execution limit to 900 seconds, the same bound the ordinary plugin update path already uses, and does so before the download starts, so the download's own budget always begins against a full clock. The 300 second download budget sits well inside that 900 second limit on purpose: a download that is simply too slow now ends cleanly with a clear error and the partial file discarded, rather than the whole script being killed partway through writing files.

## [0.61.104] - 2026-07-30

### Fixed

- Starting a fleet agent update failed immediately with "the agent could not arm its self-update: This command takes no parameters", and the rollout halted after its first site. WordPress stores the values that come with a request in separate groups depending on where they came from, and when a value is added that it has not seen before, it puts it in the first of those groups; for a request sent as JSON, that first group is the request body itself. The agent adds the verified details of a signed command to the request after checking it, so those details landed inside what the agent then read back as the body, and a command that expects an empty body saw something in it and refused. This was never visible in manual testing, because a request sent without the JSON content type does not hit this behavior, and only a caller that sets it is affected. The agent no longer mixes its own verified command details into the request body, and the two commands that expect an empty body now ignore anything that arrives rather than refusing.
- A second command, Refresh inventory on a site, had been failing this way since it shipped and had never actually worked. It returned a success response while doing nothing, and the control plane recorded it as successful because it only checked that the request itself had been delivered, not what the agent said in reply. Refreshing a site's inventory now works, and the control plane reads the agent's answer.
- Investigating the above turned up a wider gap: the control plane checked only whether a command reached a site, not whether the agent accepted it. A rollback the agent refused was recorded as "rolled back", telling an operator that a site had been restored when it had not; an update dry run was always recorded as successful; and media optimization, restore, and delete-originals jobs would wait forever for a result that was never coming, because a refused command sends no follow-up. All of these now treat a refusal as a failure and keep the agent's own explanation, and where a refusal is genuinely acceptable, that is now stated in the code so the difference is clear. Anyone reviewing past update history should be aware that a task recorded as rolled back may not actually have been, for the reason above.

## [0.61.103] - 2026-07-30

### Added

- A self-hosted install can now mirror the published WPMgr agent release from our public GitHub releases into its own object storage (GH #302, driven by GH #310 and GH #255). A self-hosted install previously had no way to get agent updates at all: the published release lives in the hosted service's storage, and a self-hosted install has its own storage that nothing ever writes to, so the dashboard had no agent update to offer and neither did a site's own wp-admin. One operator documented the workaround they had been using instead: build the plugin zip by hand, upload it, verify the storage endpoint, add a setting to every single site's wp-config.php, and clear caches, which does not scale across many sites on many servers. Once mirroring is turned on, everything works exactly as it does on the hosted service: the dashboard knows the current agent version, sites are offered the update, and the fleet update flow works.
- This ships off by default and is turned on with a single setting.
- The control plane downloads the release once, not once per site, and sites themselves never contact GitHub; they only ever talk to the control plane they already trust.
- The download is verified three ways before anything is published: a checksum published alongside the release, the checksum GitHub itself reports for the asset, and a checksum computed over the bytes actually received. All three have to agree.
- Sites no longer need a per-site setting to trust where the package comes from, because the control plane serves it from its own address, which every agent already knows. This is the part that removes the per-site wp-config.php edit the old workaround required.
- WPMgr will never overwrite a release an operator published themselves. If it finds a release it did not publish, it stands down and says so, so an operator running their own builds is never taken over.
- A mirrored release is only ever replaced by a genuinely newer one, so it will not move a fleet backwards.

### Fixed

- Downloading the agent package could fail on a slow connection (GH #302). A site downloading steadily but slowly used to be cut off partway through, leaving it unable to update at all. It now completes as long as it keeps making progress; a download that genuinely stops making progress is still ended, and no single download can run indefinitely.
- Shutting down the control plane now waits for in-progress agent package downloads to finish, within the normal shutdown allowance, so a restart or a deployment no longer cuts one off partway through.

## [0.61.102] - 2026-07-29

### Fixed

- The WPMgr agent plugin is published through two channels: as an asset attached to each GitHub release, and to object storage for control-plane-driven updates. The two were labelling the same code with different version numbers. The agent plugin carries its own version, which only changes when the agent itself changes, but the GitHub release workflow runs on every release tag, including the many that only touch the control plane or the dashboard, and it was stamping the agent asset with the release tag instead of leaving the agent's own version in place. Identical agent code was published as several different version numbers; at the time of this fix the agent's own version was 0.61.98 while the newest GitHub asset was labelled 0.61.101.
- The agent refuses to install anything that is not strictly newer than what it already runs, which is correct and protects against downgrades. A site that installed a tag-labelled asset would then refuse later genuine releases carrying the agent's own, lower number, so anyone who installed the agent from a GitHub release asset could end up unable to take further updates from the object-storage channel.
- The GitHub release asset now carries the agent's own version, the same number the object-storage channel publishes, so both channels describe the same code identically. The agent version moves from 0.61.98 to 0.61.102, deliberately, to clear the numbers published by mistake, so any site that installed one of those assets can update normally again; no manual intervention is needed, the next update offer will simply work. A check now runs on every change that fails if the release workflow ever goes back to stamping the asset with the release tag.

## [0.61.101] - 2026-07-29

### Fixed

- On a site's Email > Log tab, selecting log entries and clicking Delete showed a success message reading "0 log entries deleted" and deleted nothing (GH #307, reported by MrMK0R on a self-hosted install). The dashboard and the control plane disagreed about the name of one field in the request: the dashboard sent the list of selected entries under one name, and the control plane looked for it under another, so it received an empty list, deleted nothing, and truthfully reported that it had deleted nothing. There was nothing wrong with deletion itself. The Resend button on the same screen was broken in exactly the same way and for the same reason, so selecting entries and clicking Resend also quietly did nothing. Both are fixed. A deletion that removed nothing was also being written into the audit log as though it had happened; that is corrected too.
- Sending no entries at all, or an empty list, used to return success with a count of zero, which is the same confusing outcome the bug above produced. Both the delete and resend endpoints now reject that with a clear error instead of reporting a successful deletion of nothing.
- WPMgr has an automated check that compares every API endpoint against its published specification. It was written, and it worked when run by hand, but it was never included in the automated checks that run on every change, so it had never actually run there. That is why a mismatch like the one above could reach a release. The check now runs on every change, and a second check that compares the fields of every request body against the specification runs alongside it; it also now looks at fields nested inside other fields, not only at the top level. Together the two checks cover 149 request bodies and 646 fields.

## [0.61.100] - 2026-07-28

### Fixed

- On a wide screen, the Sites table gave nearly all its spare width to the Site column and none to any other column (GH #261). On a 5120 pixel display the Site column took roughly three quarters of the table while every other column was squeezed into the right hand side. Every column now grows only up to a sensible limit, Tags and Backup share the extra space once Site reaches its own limit, and whatever is left over sits in empty space at the end of the table instead of stretching one column further. On that same 5120 pixel display the Site column now takes about eight percent of the table instead of about seventy five.
- Columns could also clip into each other at ordinary widths (GH #255, reported on a 22 site fleet): the Agent, Updates and Backup columns overlapped, and the Uptime column was pushed off screen entirely. Two things caused it. The header and the table's rows sized each column independently, so the two could drift apart from each other, and some columns were shown more text than they had room for, so it ran into the next column instead of fitting. The header and the rows now share one definition of every column's width, and, as described below, the Agent and Backup columns show less text per row, so both problems go away together.
- The Backup column repeated its own heading: it read "Backed up 10h ago" under a column already titled Backup, and wrapped onto two lines on most rows. It now shows just the time, with the same icon, and a failed backup is still called out clearly.
- The Agent column repeated a status word on every row. On a healthy fleet nearly every row said the same thing, which pushed the version number into the next column. Each row now shows the version next to a status icon instead, and the icon's shape carries the meaning rather than color alone. Screen readers still announce the full state and version for every row.
- The note about what a site's agent version is compared against has moved from every row to the Agent column's heading. Version 0.61.99 added a per row note saying when a site is compared against the newest agent in your own fleet rather than a published release, which matters on a self-hosted install. That is a fact about the comparison itself, not about each site, so it now appears once, on the column heading, instead of taking width from every row.
- The loading placeholder shown while the Sites table is still loading had drifted out of step with the real table and was missing two columns, so the table appeared to shift sideways once it finished loading. It is now built from the same column definitions as the table itself.

## [0.61.99] - 2026-07-28

### Fixed

- The Agent version card could read "0 of 24 sites on unknown, 24 unknown", with the agent-status filter on the Sites list appearing to do nothing (GH #255, reported on a self-hosted install running 24 sites immediately after 0.61.97). The fleet agent-version feature compares each site against the currently published agent version, which it reads from a pointer file the release process writes into the hosted service's object storage; a self-hosted install has its own object storage and never receives that file, so WPMgr had no version to compare against, and with no reference version every site necessarily fell back to unknown. The sites were reporting their versions correctly the whole time, and the filter was not broken either: it only offers values that actually appear, so with every site unknown there was a single option that matched everything, which looked like nothing happening.
- When there is no published reference version, self-hosted installs now compare each site against the newest agent version present in that install's own fleet instead, so a site lagging behind the others shows up as behind. The interface says plainly when the comparison is against the fleet rather than a published release, so "current" is never mistaken for "up to date with the newest release that exists." When there is genuinely nothing to compare against, WPMgr now says so in plain language instead of printing "unknown" as though it were a version number.
- On the hosted service, the published agent version is cached, and a brief object-storage failure used to be cached too, which could make the dashboard fall back to comparing against the fleet and briefly report every site as current when they were in fact behind. The last successfully read version is now kept and used across a brief failure, a failure is retried quickly rather than held, and a version that has gone stale beyond a bound is no longer presented as though it came from the published release. An install whose release channel exists but is temporarily unreachable now says it cannot determine a reference version, which is a different situation from an install that has no release channel at all.
- The control-plane switch that enables the fleet-wide agent update introduced in 0.61.98 was not reported to the dashboard, so the action stayed hidden even when an operator turned the switch on. It is now reported, and the action appears exactly when the control plane would honor it. The feature still ships turned off.
- Comments in generated API client code that had been reflowed incorrectly in 0.61.98 are restored by regenerating the client, with no behavior change.

## [0.61.98] - 2026-07-28

### Added

- Fleet-wide agent updates (GH #255, phase 2 of two). An owner or administrator can now start an agent update across selected sites from the Sites list, instead of visiting each site's wp-admin one at a time to click the update the agent already offers there. Starting a rollout is restricted to those two roles.
- This ships turned off. The capability sits behind a control-plane switch that defaults to off, so upgrading to this release changes nothing on its own. It will be turned on once it has been exercised against real sites.
- A rollout proceeds in waves rather than hitting every selected site at once. The first wave is a single site, the second is a small percentage of the selection, and only once those have gone well does the rest of the fleet follow. A wave only opens after every site in the wave before it has confirmed, and confirmation means the updated agent itself reported back its new version, not that the update was merely scheduled or acknowledged. If the first wave fails, or too many sites in a later wave fail to confirm, the whole run stops and every remaining site is cancelled. A stop control halts every agent update in progress across the fleet.
- Updating the agent works differently from updating any other plugin, because the agent is how WPMgr reaches a site at all. The update itself runs in a separate background request rather than the request that reports the result, and success is only ever recorded once the new agent version reports back in. A site that cannot complete that background step is left untouched and reported as unconfirmed, not failed.
- Sites that cannot take an agent update from this channel are reported with a reason instead of being counted as a failed rollout: the WordPress.org build ships without the self-updater, and a site running an agent older than this release does not yet have the update channel this relies on. Both are skipped, not failed.

### Fixed

- A rollout whose target version stopped being published partway through used to carry on as though it had succeeded; it now stops the run instead, since a site that did not change is not a successful update.

## [0.61.97] - 2026-07-28

### Fixed

- A control-plane update task could target the WPMgr agent's own plugin (GH #255). The agent reports itself in the plugin inventory it pushes like every other plugin, WordPress advertises an available update for it the same way, and neither the control plane nor the agent refused it, so an operator could select the agent in a bulk update run across many sites at once. That is worth avoiding: updating the agent this way means its own code is what performs the update, overwriting its own files from inside the very request that has to report back whether the update succeeded, so if anything goes wrong partway through, the site has no working code left to report it with. Every other plugin update is protected by a snapshot and an automatic rollback, but that protection deliberately does not arm for the agent's own directory, because the thing that would perform the rollback is the thing being replaced, and recovery would mean per-site filesystem access.
- The agent now refuses any update task that targets its own directory, identifying itself by its plugin header name as well as its folder so an install in a renamed directory is still recognized (a different plugin whose name merely resembles the agent's is deliberately not affected). The control plane independently stops offering the agent as an updatable component and no longer counts it toward a site's pending updates. Both sides refuse on purpose, so a site running an older agent is still protected by the control plane's refusal. The agent stays visible in the inventory with its version; only the actionable update is withheld, and the agent's normal one-click update inside wp-admin is unchanged.

### Added

- Fleet-wide agent version visibility (GH #255, phase 1 of two), which answers a question WPMgr could not answer before: how many sites are running an outdated agent. The Sites list shows each site's agent version as a status, current, outdated, unknown, or not self-updating, and it is also a filter. The Updates page gains a summary of how many sites are on the current version and how many are behind.
- Sites running the WordPress.org build are reported as "not self-updating" rather than "outdated", because that build ships without the self-updater and can never take an update from this channel; reporting them as outdated would be telling the operator to fix something they cannot fix.
- Actually triggering an agent update across the fleet is phase two of this work, and is not part of this release.

## [0.61.96] - 2026-07-28

### Security

- Updated the gRPC library the control plane depends on, from 1.81.1 to 1.82.1, to pick up fixes for GO-2026-6061 (GHSA-hrxh-6v49-42gf), which covers issues in that library's authorization engine and its HTTP/2 server transport. The library reaches WPMgr indirectly, through the OpenTelemetry trace exporter, and the affected code was reachable from database connection pool shutdown, so the update is worth taking even though WPMgr does not use the authorization engine itself. Official container images already build against a patched Go release; this update closes the remaining path.

## [0.61.95] - 2026-07-27

### Fixed

- A failed backup left its working files on the site instead of cleaning up after itself (GH #256). When a backup gave up, the control plane deleted its record, but the site kept the half-finished working directory: upload parts, copied plugins and themes, and the database dump. One reported site was left with roughly 1.4 GB spread across seven part files of about 198 MB each. There were four separate paths in the agent's backup watchdog that could give up on a backup (a hard time ceiling, a late-phase stall, running out of resume attempts, and a stale-task guard), and none of them cleaned up the working directory. All four now reclaim it, but only after taking the same run-lock a live backup holds before deleting anything: a backup that is merely slow and still running keeps its working directory untouched, and cleanup is left for a later sweep.
- The routine cleanup that reclaims old backup working directories, and the separate one that clears leftover files after a restore, both depended entirely on WP-Cron, so on a site where WP-Cron is disabled, unreliable, or simply has no visitors to trigger it, neither ever ran and nothing was ever reclaimed. Both now also run on an ordinary page load, throttled to once an hour for backup working directories and once every fifteen minutes for restore leftovers, so a busy site pays effectively nothing for them. The restore cleanup also had a way to wedge itself permanently: it scheduled itself as a one-shot event, and once its scheduled time passed without firing, WordPress still counted it as "already scheduled", so every later restore skipped scheduling a new one and cleanup never ran again. An overdue event is now detected and replaced with a fresh one.
- The backup delete dialog said deleting a backup "reclaims its storage", but the site's own temporary files stayed on the host regardless. The wording now says what actually happens, and notes that the site cleans up its own temporary files separately.
- The Sites grid could show a green "Backups" indicator next to a red "Failed" badge for the same site. The indicator lit up whenever a site had any backup status at all, including a failed one, so a failed backup still looked healthy at a glance. It has been removed; the backup chip beside it already shows the real status.
- Two other backup failures visible in the original report were already fixed in earlier releases: the "runner already in flight" refusal (0.61.84) and the missing local chunk on upload after an interrupted backup (0.61.87).

## [0.61.94] - 2026-07-27

### Fixed

- Uptime checks could be cut off partway through on a larger fleet, leaving gaps in uptime history with nothing to explain them. The job running a sweep was bound by a one minute limit, but a sweep of more than about forty sites can legitimately take longer when sites are slow to answer, which is exactly what happens during the fleet-wide problems the checks exist to catch. Some sites were then checked and recorded while the rest were quietly skipped, with no error. The limit is now derived from how long a sweep can actually take, and a sweep that still runs short of time now stops cleanly and records how many sites it managed rather than being cut off mid-flight.

## [0.61.93] - 2026-07-27

### Added

- Alerts for a site whose WordPress has failed while its cache keeps serving visitors (GH #291, completing the work started in 0.61.90). This is deliberately off by default on any existing install, because switching it on may surface sites that have been quietly broken for a while, and nobody should be woken by an upgrade. A one-time prompt in the dashboard offers to turn it on and explains what to expect, and there is a permanent switch under alert settings.
- An alert only fires on a genuine WordPress failure that has persisted for several checks in a row, and only for a site WPMgr has seen working at least once, so a site whose health check is simply blocked can never raise a false alarm. Uncertain results, such as a cached response or a site in maintenance during an update, are reported as unknown and never alert.
- If a large share of one organisation's sites report a failure at the same time, WPMgr sends a single summary naming every affected site instead of one alert per site, since a fleet-wide reading is far more likely to be a shared host problem or a monitoring fault than many unrelated sites breaking at once. If the situation gets materially worse it sends an update, and it sends a single recovery notice at the end.
- A custom health-check path can be set per site for installs where the default check cannot reach WordPress, and alerts can be muted for one site without turning off its monitoring.

## [0.61.92] - 2026-07-27

### Added

- WPMgr now checks whether WordPress itself is actually running, not just whether the site returns a page (GH #291). A site with page caching can keep serving visitors a saved copy of its homepage even when WordPress is completely broken, which is why such an outage could previously go unnoticed. Alongside the existing check, WPMgr now asks the site for something a cache does not answer, so a site that is serving cached pages while WordPress is down is shown as degraded with an explanation of what that means for logins, forms and the admin area.
- Uptime figures are deliberately unchanged. A cached page that loads still counts as up, because visitors really are being served. Application health is reported as a separate signal, so historical uptime and SLA figures keep their meaning.
- The check costs almost nothing on a healthy site: if the site agent has reported in recently, that already proves WordPress is running and no extra request is made at all. The direct check only runs for sites that have gone quiet, which is exactly when it is needed.
- WPMgr reports this as unknown rather than guessing whenever it cannot be certain, including when a response turns out to have come from a cache, when the site is merely slow, when it is in maintenance mode during an update, or when a security plugin blocks the check. Only a genuine WordPress error counts as broken. No alerts are sent for this signal yet; that follows in a later release with an explicit opt in.

## [0.61.91] - 2026-07-27

### Fixed

- An update that broke a site could be reported as successful and never rolled back (GH #291). After applying an update WPMgr checks the site's homepage to decide whether to undo it, but on a site with page caching that check could be answered from the cache with the pre-update page, so an update that broke WordPress outright still looked healthy. WPMgr now asks the site agent directly first, over a signed request that cannot be served from a cache and only works if WordPress actually loaded, and it still checks the public homepage afterwards so that a theme or front-end problem the agent cannot see is caught too. A response that is identified as coming from a cache is now treated as inconclusive rather than as proof of success.
- Updates are no longer at risk of being cut off partway through. The job running an update was bound by a one minute limit, while a single slow install was allowed up to five minutes, so a slow but successful update could be terminated mid-flight and left stuck. The limit is now derived from the time the work is actually allowed to take.
- An update is only undone now when a failure is confirmed repeatedly, never on a single reading. A site that returns an error briefly while it finishes activating, restarts, or is momentarily behind a failing proxy is given time to recover first, so a good update is not reverted because of a passing blip.

## [0.61.90] - 2026-07-27

### Fixed

- A site whose WordPress had stopped working could still show as fully "Up" on the fleet view (GH #291). The uptime check requests the site's homepage, and on a site with page caching that page can be served straight from the cache without WordPress running at all, so a completely broken site kept returning a healthy response. WPMgr already knew better: when the site agent stops answering, the control plane verifies it over a signed request that cannot be cached, and marks the site disconnected. That signal was being ignored when deciding what to show. A site that is serving cached pages while its agent is unreachable is now shown as degraded, with a plain explanation of what that means and what is likely broken, instead of a reassuring green tick. Sites where the agent was deliberately deactivated or uninstalled are unaffected and still show as normal.
- Uptime figures themselves are unchanged by this release. A cached page that loads is still counted as up, because visitors really are being served; what changed is only what the dashboard tells you about the state behind it.

### Changed

- For self-hosted installs using the optional ClickHouse metrics backend, the uptime table now adds any missing columns on startup instead of silently keeping an outdated shape. A failure to do so is logged and no longer prevents the control plane from starting, since metrics should never block site management.

## [0.61.89] - 2026-07-27

### Fixed

- Plugin vulnerabilities are now detected. The vulnerability scanner compared the plugin identifier WordPress reports internally (a path such as `woocommerce/woocommerce.php`) against the plugin slug the vulnerability feed publishes (`woocommerce`), so the two never matched and no plugin vulnerability was ever reported. Themes and WordPress core were unaffected and continued to match correctly. Since plugins account for the large majority of WordPress vulnerabilities, this means plugin findings will appear for the first time on the next scan, and existing sites may show a number of them at once. Every vulnerability view now also links directly to the matching Wordfence record.
- Plugin and theme file-integrity verification could silently stop working for up to 30 days when the WordPress.org checksum service returned a duplicate entry for a file. The failure was recorded as a success, so no error surfaced. The duplicate is now handled, the result is only cached when the fetch genuinely succeeded, and the failure is logged.

### Added

- The official WordPress.org SHA-256 file hashes are now stored alongside the MD5 hashes already collected. Nothing consumes them yet; this is groundwork for stronger file-integrity verification, since a matching MD5 is no longer sufficient evidence that a file is unmodified.

## [0.61.88] - 2026-07-24

### Added

- Per-site default "Login As User" account for one-click sign in (GH #286). You can now choose, per site, which account one-click wp-admin login uses, so a site with several administrators always signs you in as the account you picked instead of whichever administrator happens to have the lowest user ID. Set it in Site settings under Access, or check "Make this the default for this site" when you log in as a specific user. The default applies whenever you sign in without picking a user, and the site's wp-admin button now shows which account it will use. Leaving the default blank keeps the previous behavior of signing in as the first administrator. The autologin audit trail now records the actual account used, including for the default flow. This is a control-plane and dashboard change only; existing agents honor it with no update.

## [0.61.87] - 2026-07-24

### Fixed

- Interrupted backups now resume cleanly instead of failing with a missing local chunk (GH #283). During the upload stage the agent deletes each chunk from local disk right after uploading it, but it only recorded which chunks it had uploaded once the whole upload finished. If the server stopped the worker partway through, the resumed backup asked the control plane for upload URLs again, was correctly told to send chunks it had already uploaded and deleted, and then failed looking for the missing local files. The agent now records its upload progress durably as it goes, never deletes a local chunk until that progress is safely persisted, and on resume skips any chunk it has already uploaded. This was surfaced by the v0.61.85 backup watchdog change, which lets a slow backup resume rather than aborting.

## [0.61.86] - 2026-07-24

### Fixed

- Archiving a site now stops its scheduled backups (GH #282). Previously the backup scheduler looked only at the schedule row, so a site that had been disconnected and archived kept firing its nightly backup, which then failed because the site is no longer managed and sent a misleading "backup failed" email. The scheduler now skips schedules for archived or revoked sites (and for organizations that have been deleted), while still attempting and alerting for sites that are only temporarily unreachable, since that failure is actionable. A failed backup for an archived or revoked site no longer sends an email, and a manual backup on such a site is refused with a clear message. Restoring a site resumes its schedule automatically on the next scheduler tick.

## [0.61.85] - 2026-07-23

### Fixed

- Full backups on slow servers no longer fail at the upload stage (GH #279). The control plane watches each running backup for progress, and on a slow host a large backup could go quiet long enough during the archiving stage that the control plane wrongly marked it failed while it was still working; the next upload step was then rejected. The progress watchdog now has two stages: a quiet backup is flagged as taking longer than expected but kept running, and it is only failed after a much longer, configurable silence. Any sign of life from the agent (a progress report, an upload URL request, or a manifest submission) clears the flag at once. The agent now also sends progress heartbeats during long archive and encryption passes so the flag rarely appears, reports the exact control plane status and message when a callback is rejected, and removes its local working files when a run ends in failure. The dashboard shows a calm note while a backup is quiet.

### Added

- Two self-host settings tune the backup progress watchdog: WPMGR_BACKUP_STALL_SOFT_TIMEOUT (default 3 minutes, when a quiet backup is flagged as taking longer than expected) and WPMGR_BACKUP_STALL_HARD_TIMEOUT (default 30 minutes, when a truly stuck backup is failed).

## [0.61.84] - 2026-07-23

### Fixed

- Backups no longer time out on OpenLiteSpeed and LiteSpeed servers (GH #274). The agent acknowledges a backup request to the control plane and then continues the work in the background; it previously released that acknowledgment using a PHP-FPM-only mechanism, which OpenLiteSpeed's PHP does not provide, so the connection stayed open for the entire backup and a reverse proxy in front of the control plane eventually timed it out. The agent now releases the acknowledgment using whichever mechanism the site's server actually supports, covering PHP-FPM, OpenLiteSpeed and LiteSpeed, and any other server as a last resort. A backup or restore that was already running when a duplicate request arrived is also no longer misreported as failed to the operator or left orphaned in the dashboard; the control plane now recognizes this specific case and keeps tracking the original run instead.

## [0.61.83] - 2026-07-23

### Fixed

- The Sites dashboard uptime badge could show a green "Up" for a site that had never been probed, or whose probe result had not synced yet (GH #272). The badge only special-cased the literal `false` value, so `null`/absent uptime data fell through to "Up" instead of a neutral state, which is the wrong failure mode during an incident (a fully-down, never-probed site could display green). The badge is now a proper three-state indicator: "Up" (green) only after an explicit successful probe, "Down" (red) after an explicit failed probe, and a neutral "Unknown" whenever the probe result is not yet known, on both the grid and table views of the Sites list.

## [0.61.82] - 2026-07-22

### Fixed

- The "disable file editor" hardening toggle could fatal every request on a Roots/Bedrock site (GH #268). The agent wrote a raw `define('DISALLOW_FILE_EDIT', true)` at the top of wp-config.php, but Roots/Bedrock manages that same constant through its own config layer, which throws when the same constant is defined a second time. The agent now checks whether the constant is already defined (or the config file is otherwise framework-managed) before writing anything; when it is, nothing is written to wp-config.php and the toggle still reports success, since the constant is already enforced. Standard WordPress installs are unaffected: the constant is written exactly as before when nothing else has already defined it. The same protection now also covers the full-page cache's `WP_CACHE` constant, which is written through the same code path.

## [0.61.81] - 2026-07-22

### Fixed

- Agent command updates (and other control-plane commands) could fail on sites running some third-party plugins that globally decode the Authorization header on every request, for example as part of their own JWT-based auth (GH #269). Such a plugin could throw an error on the agent's own signed request before the agent had a chance to verify it, causing the request to fail outright instead of reaching the agent's own authorization check. The agent now moves its own signed Authorization value out of the request before any other plugin's code runs, so this class of conflict can no longer occur, and reads it back internally to authenticate as before. No control-plane or protocol change; self-hosted and hosted control planes are both covered.

## [0.61.80] - 2026-07-21

### Changed

- The managed-update runner no longer adjusts WordPress's upgrader working-directory handling in the WordPress.org distribution build; self-hosted installs are unchanged.

### Fixed

- Reworded a documentation comment in the file-manager write guard that quoted an example attack payload verbatim, which made some security scanners (for example Wordfence) report a false-positive match on the plugin's own file (GH #266). The comment described a payload the guard blocks; no code path or behavior changed.

## [0.61.79] - 2026-07-21

### Fixed

- The agent plugin could throw a fatal error on activation on any WordPress install that was set up from a plain git clone or a GitHub source download rather than a built release package, that is, one without a Composer `vendor` folder present (GH #262). The plugin's own class loader mapped its class names to files using a mechanical rule that did not match every actual filename on disk (some interface files, a renamed folder, and several integration and utility files with brand names or digits in them). This was previously masked whenever the `vendor` folder was present, because a second, more complete loader ran first and covered for it; without that folder, the plugin's own loader was the only one available and could fail to find some of its own files, so activation could fatal. The loader now resolves every one of the plugin's own files correctly on its own, with no dependency on the `vendor` folder.

### Security

- Pinned transitive dev and build tooling dependencies (`brace-expansion`, `js-yaml`) to versions that patch recently disclosed high-severity denial-of-service advisories. Same-major updates with no API change, and no change to agent, API, or dashboard runtime behavior.

## [0.61.78] - 2026-07-20

### Fixed

- The agent could fail to set up its encryption key on some managed hosts (for example Hostinger and other CloudLinux/CageFS hosts), leaving the plugin active but unable to connect or enroll (GH #257). This had two causes that often occurred together on these hosts: a wp-config.php without real secret salts, and a fallback key-file location that landed outside the account's allowed write paths. The agent now tolerates a wp-config.php that is missing some (or has only one) of its secret salts, tries the uploads folder as an additional fallback location before falling back to a location outside the site's own folder, and as a final safety net can store a dedicated encryption key in the database on hosts where no file location is writable and no secret salts are usable. This last option can be turned off for stricter setups. The key file and database fallback are created and read back safely so that two requests setting up the key at the same time, or a write interrupted partway through, can never leave the site with a broken or mismatched key. Sites that already connected successfully are unaffected: their existing key is always re-read from the same place, never regenerated. The setup notice shown in the WordPress admin, when the key still cannot be established, now explains the actual cause instead of one generic message.

## [0.61.77] - 2026-07-20

### Fixed

- Internal: made a Sites-page UI test deterministic so it no longer fails intermittently in CI. No user-facing change; carries the 0.61.76 dashboard fixes.

## [0.61.76] - 2026-07-20

### Fixed

- The fleet Email log now shows each site's name instead of its internal ID (GH #251). Dashboard only.
- Removed a duplicate "Add site" button on the Sites page; the button also no longer appeared twice on the empty state (GH #252). Dashboard only.

## [0.61.75] - 2026-07-19

### Added

- Vulnerability alerts (GH #247). WPMgr now notifies operators when a new vulnerability is found, instead of the finding only appearing on the dashboard:
  - A single email per scan that summarizes the new findings across your sites (site, component, installed and fixed versions, severity, and CVE), batched so one feed update that matches many sites sends one email, not one per site or per finding.
  - An operator-configurable minimum severity (High and above by default); findings that do not have a severity score yet are always included.
  - A signed webhook fires the same event for Slack or custom integrations (best effort; email is the guaranteed channel).
  - An open-findings section in the daily email digest.
  - Configured on the Alerts page alongside downtime alerts, sharing the same recipients. Opt-in and off by default; existing findings are not retroactively alerted when you enable it.
  - Also fixes a bug where saving alert settings could silently reset the security-events toggle and a couple of other fields.

### Fixed

- Follow-up to GH #245: the vulnerability CVSS enrichment feed now downloads reliably, so real severities populate. Once the earlier rate-limit fix let the enrichment feed request through cleanly for the first time, the download surfaced two latent problems that had always been hidden behind the rate limiting: the enrichment feed (much larger than the detection feed, since it carries CVSS, CVE, CWE, and reference data for every analyzed vulnerability) was loaded into memory all at once, which exhausted the control plane's memory and killed the process mid-download; and the fetch shared a short timeout intended for other traffic, which would have cut off the large download regardless. The enrichment feed is now streamed and written to the database in bounded batches so memory stays flat no matter how large the feed grows, and the fetch is given its own adequate time budget (with the network egress protections unchanged). The detection feed path is unchanged. Control plane only.

### Fixed

- Follow-up to the vulnerability-severity fix (GH #245): the CVSS enrichment feed now refreshes reliably. The previous release alternated the two Wordfence feeds across syncs to respect the feed's rate limit, but that only worked if syncs were spaced far apart; when two syncs happened close together (for example a manual refresh shortly after a scheduled one, which are tracked in separate schedules and so are not de-duplicated against each other), the enrichment request was still rate-limited and skipped, leaving severities unrated for longer. The control plane now enforces the minimum spacing by wall-clock time: a sync that would arrive too soon after the previous request is skipped entirely (no request is made), and the feed it intended to fetch is retried on the next eligible run rather than passed over, so CVSS severities populate on the first available cycle regardless of how the syncs are timed. Control plane only.

### Fixed

- Vulnerability findings no longer all show as "Low" (GH #245). Severity comes from the Wordfence Intelligence feed's CVSS data, but a request-spacing bug rate-limited the enrichment feed on every sync, so every finding was stored without a CVSS score and fell back to the lowest severity, meaning a critical core vulnerability could appear with a "Low" badge. The feed requests are now spaced correctly (the two feeds alternate across syncs, one request per sync) so real severities land and existing findings re-derive their severity on the next scan. A finding that genuinely has no severity data is now shown as "Unknown", ranked for attention above Low and Medium, never silently bucketed as Low. And when the enrichment feed is unreachable, the Vulnerabilities page and the admin feed status now say so explicitly instead of understating risk without a trace. Control plane and dashboard.

## [0.61.71] - 2026-07-18

### Fixed

- The dashboard no longer under-reports a working page cache (GH #243). Three distinct gaps, all reporting or navigation defects; the cache itself was serving correctly:
  - The cache hit-ratio chart showed a flat 0% on sites where the agent's managed web-server rules serve cached pages directly from disk, because those hits never execute PHP and cannot be counted there. The chart is now honest about what it measures: on such sites the Cache tab shows a "Served at the web-server level" state (that is the fastest path working as intended), the series is labeled as the PHP-layer ratio, and the copy explains how to verify caching via the `x-wpmgr-source` response header. No numbers are fabricated or altered.
  - The site card's "Page Cache" and "Object Cache" configuration dots were gray for every site since they were introduced, because they looked for plugin entries that can never exist (both features are drop-ins, which WordPress does not list as plugins). The dots now read the real per-site cache configuration, exposed on the sites API.
  - The agent's admin-bar "Manage in WPMgr" link pointed at a dashboard page that never existed and rendered "Not Found". The agent now links to the site's Cache tab, the dashboard redirects the old link target so already-installed agents work immediately after this control-plane update, and unknown dashboard paths now render a proper page with a way back instead of a bare "Not Found".

### Changed

- Documentation catch-up release. The API reference at wpmgr.app/docs now documents the full control-plane surface: about 97 previously missing endpoints were added (dashboard two-factor, security suite, file-integrity scans, vulnerability scanner, organizations, admin console, database cleaner, public endpoints, and the agent protocol), stale paths were corrected, and a new full-engine contract test keeps the reference and the live routes in lockstep from now on (a new endpoint fails CI until it is documented). New user guides: file manager, security suite, monitoring and fleet dashboards, clients and the portal, object cache, and the audit log; the backups guide gained a destinations section and the sites guide an accurate tags section. `.env.example` now documents every accepted environment variable, with hosted-only settings clearly marked. No runtime behavior change (one test-only accessor added to the server).

## [0.61.69] - 2026-07-17

### Added

- Site tags (GH #230). Sites can now be organized with colored tags across the whole dashboard:
  - Create and assign tags from a keyboard-first picker (type to search, Enter to create) on the site card, the sites table row, or site settings; a site can carry multiple tags, and tag edits apply instantly.
  - Every tag gets a consistent, automatically assigned color (the same tag always renders the same color for everyone, readable in light and dark themes), with an optional per-tag custom color chosen from a curated palette.
  - Filter the Sites list by one or more tags with match-any or match-all semantics, persisted in the URL so filtered views can be bookmarked and shared; clicking any tag chip filters to that tag.
  - Bulk tagging: select multiple sites and add or remove tags across all of them in one action, with a mixed-state indicator when only some selected sites carry a tag and a per-site result report.
  - A tag management page under Settings: rename a tag everywhere at once, merge duplicates, change colors, and delete with the usage count shown before anything is removed.
  - Existing site tags are registered automatically on upgrade (a tenant-level tag registry with row-level security backs the feature; tag data stays on the site rows, so nothing about existing tags changes). Registry changes (create, rename, merge, recolor, delete) require organization-scoped access and are audit-logged.

## [0.61.68] - 2026-07-17

### Changed

- The Sites list's uptime enrichment no longer scans the full uptime-probe history on each load, so it stays fast even after idle (the durable fix for the post-idle Sites-list slowness, replacing the interim keep-warm from 0.61.67). The uptime worker now maintains a compact per-site daily rollup; the sites-list query reads that rollup for whole days plus a small, index-bounded read of just the two partial edge days, so its cost is proportional to the number of sites rather than the amount of uptime history. The uptime percentages and latencies remain exact to the second (identical to the previous window aggregate) so the numbers do not change. Control plane only; the rollup tables are created and backfilled from existing data automatically on upgrade, and are pruned on the same retention as the raw probes.

### Fixed

- The Sites list no longer stalls for several seconds on the first load after a period of no traffic. The sites list enriches each row with uptime data via an aggregate over recent uptime probes; on a small database instance that aggregate was being re-read from disk (a cold cache) once the short-lived in-memory cache expired during idle, so the first request after ~15-30 minutes was slow while later requests were fast. A background refresher now keeps that query (and the database's cache for it) continuously warm, so the Sites list stays fast even after idle. Control plane only; a follow-up will roll the uptime data up so the query is inherently cheap regardless of cache state.

### Changed

- The Sites list now shows each site's last backup as a human-readable relative time (for example "2h ago"), with the exact date and time on hover, matching how the Backups page already displays it, instead of a raw timestamp (GH #231). Dashboard only.

## [0.61.65] - 2026-07-16

### Changed

- Two-factor and other stored secrets that can no longer be decrypted (because the secrets-at-rest encryption key changed) now fail loudly and clearly instead of looking like a wrong code (GH #215). If the encryption key is not pinned on a self-hosted install and the platform regenerates it across restarts, the previously-stored secret can no longer be read. The control plane now logs a fingerprint of the resolved key at startup (so a changed key across restarts is visible), warns at boot when stored secrets no longer decrypt (with the exact remediation: pin a stable `WPMGR_SITE_DEST_AGE_SECRET`), and shows a precise "the server's encryption key changed, sign in with a recovery code and re-enroll" message at the two-factor prompt instead of a generic failure. This is a diagnosability improvement; the underlying encryption behavior is unchanged (a pinned key was and remains stable across restarts).

### Fixed

- Website screenshots for slow-loading or uncached pages are no longer blank (GH #229). The capture worker previously snapshotted almost immediately; it now waits for the page load, network idle, and DOM to settle (with a short additional render delay), all bounded by a hard timeout so a slow page degrades to a best-effort partial capture rather than hanging or coming back blank.

## [0.61.64] - 2026-07-16

### Fixed

- Scheduled backups no longer stall permanently at "queued" and are now reliably recovered (GH #232). A scheduled backup started only through WordPress cron (with a single in-process fallback that was active only on hosts where the loopback was gated), and its self-healing watchdog ran on that same cron; on a quiet, off-peak, or `DISABLE_WP_CRON` site the run could silently never start and the watchdog could silently never fire, leaving the task at "queued" forever with no error and the built-in resume never engaging (reported across a fleet as a rotating ~10-30% of sites failing nightly). The backup now always starts in-process (independent of WordPress cron), a request-driven sweeper re-dispatches any genuinely stalled task without needing a cron tick (so resume actually engages), and each stalled row now records how far it got for diagnosis. Concurrent execution of the same backup is prevented by a connection-independent file lock in addition to a database advisory lock, so a dropped database connection mid-dump can never let a second runner corrupt the in-progress backup. Agent only.
- The dashboard no longer shows a "no website" error when you switch organizations while viewing a single site (GH #233). If the site you were viewing does not belong to the newly selected organization, you are now routed to that organization's Sites list instead of a dead page.

### Changed

- The real-user-monitoring collector now also ships a readable, non-minified build (`assets/wpmgr-rum.js`) alongside the minified `assets/wpmgr-rum.min.js`, matching how the delay script already ships its readable source, so the distributed package includes human-readable source for every bundled script (the TypeScript source and build command remain documented in the readme and public repository). WordPress.org-directory transparency only; no runtime or behavior change.

## [0.61.62] - 2026-07-15

### Fixed

- Pre-update rollback snapshots (captured under `wp-content/uploads/wpmgr-snapshots/` before each plugin, theme, or core update so a failed update can be rolled back) are now reliably cleaned up instead of accumulating indefinitely (GH #226). Previously the only cleanup ran either on a WordPress cron event (which never fires on sites with `DISABLE_WP_CRON` or very little traffic) or at the start of the next update, so a site that ran a batch of updates once and then went quiet kept every snapshot forever, quietly consuming disk space and inodes. Cleanup now runs opportunistically on ordinary agent activity (no dependency on WordPress cron), a snapshot whose update succeeded is reclaimed within about an hour once it is safely past the rollback window, and any already-accumulated snapshots are swept on the first request after upgrading. Snapshots are never removed while a rollback could still be needed (a fixed minimum retention protects the control plane's post-update health-probe window and the update-safety watchdog), and a snapshot left behind by a failed rollback is still retained for a few days for manual recovery. Agent only; no change to update, backup, or rollback behavior.

### Changed

- Second WordPress.org-directory-compliance hardening pass for the agent, in response to the directory review, with no behavior change for managed sites. Every item lands in both the self-hosted agent and the WordPress.org build (fleet-agent-site-manager), which keeps the operator features (auto-login, updates, backups) and strips only the self-updater:
  - The two-factor and forced-password-change login screens now escape at the output boundary through `wp_kses()` with an explicit tag allowlist, and the remaining output-escaping suppressions are removed (swept plugin-wide).
  - The media-library helper script is now enqueued through `wp_enqueue_script()` on the upload screen instead of being printed inline.
  - The admin settings screen is fully internationalized, and its displayed name follows the build identity: the self-hosted build keeps "WPMgr Agent", and the WordPress.org build shows "Fleet Agent Site Manager" to match its listing.
  - The object cache's internal error store now uses a plugin-prefixed global, and the optional error monitor registers no error or shutdown handlers at all unless it is explicitly enabled (a true no-op when off, which is the default).
  - Directory sizing in the WordPress.org build uses a pure-PHP fallback with no shell-outs; the self-hosted build keeps its faster native path.
  - A dead raw-cURL multi-request path was removed so every outbound request goes through the WordPress HTTP API; page-cache-key assembly now unslashes cookie input while staying byte-identical to the pre-WordPress serve path (covered by a new parity test); a critical-CSS output edge case was tightened; and the readme changelog and upgrade notices were brought current.

## [0.61.60] - 2026-07-14

### Fixed

- Stored secrets (SMTP password, per-site email and destination credentials, object-cache credentials, and two-factor secrets) now survive a restart on a self-hosted install that has not set an explicit secrets-at-rest key. Previously, if `WPMGR_SITE_DEST_AGE_SECRET` was empty, the control plane generated a fresh random encryption key on every boot, so every container restart or reboot orphaned everything encrypted at rest. The most visible symptom was having to re-enter the SMTP password almost daily (and notifications silently stopping until you did); it could also make two-factor sign-in fail after a restart. The key is now derived, stably, from the already-required `WPMGR_SESSION_SECRET` when no explicit key is set, so a self-host works without extra configuration and nothing is lost on restart. The encryption key is still never stored in the database.

  Upgrade note: an install that was running without an explicit `WPMGR_SITE_DEST_AGE_SECRET` (so it was on the old per-boot key) will switch to the new stable derived key on this upgrade. Any secret that was encrypted under the old key needs to be re-entered once (re-save the SMTP password, and re-enroll two-factor from a recovery code if it was set up); after that it persists permanently. Installs that already set an explicit key are unaffected. Control plane only; no agent change.

## [0.61.59] - 2026-07-14

### Fixed

- A failed, empty backup snapshot can now be deleted on its own without being forced to also delete a healthy, unrelated later snapshot (GH #221). The chain-safety guard (and the dashboard "Delete + N dependents" warning) previously decided whether a snapshot had dependents by generation number alone, so a failed attempt was wrongly treated as a dependency of any later-generation snapshot in the same chain, even when that later snapshot's actual parent was a different, successful sibling at the same generation. Both the control plane and the dashboard now check the real parent-snapshot chain of custody, so only a snapshot that something genuinely descends from is protected from deletion. Control plane and dashboard only; the agent has no functional change in this release.

## [0.61.58] - 2026-07-14

### Fixed

- Bulk update runs no longer trigger a separate WordPress.org update check for every item in the batch (GH #218). The earlier fix that forces a fresh check before deciding "nothing to update" was running once per item, so a batch of N items made N full-catalog checks back to back, each discarding the previous result, which could intermittently leave a genuinely-pending update reported as up to date. The fresh check now runs once per run per component type, so the whole batch shares one guaranteed-fresh result.
- Plugin and theme updates no longer fail intermittently with "Could not copy file" on hosts whose system temporary directory is shared with PHP session storage and has grown to tens of thousands of stale files (GH #216). The agent now treats a writable-but-pathologically-overloaded default temp directory as unusable and falls back to its own dedicated, clean upload-directory temp location for the unpack step (only when the default is genuinely unhealthy).
- The fleet backup health endpoint no longer returns a 500 for the whole fleet when a site has never completed a backup (GH #214). A site with no completed snapshot now reports as Unprotected with a zero size instead of failing the aggregation. Affects installs that rely on the instance-wide storage settings and have any new or monitoring-only site.
- The bulk Update-sites wizard's Plugins/Themes tab badges now count only components that actually have a pending update, matching the list shown below them (GH #217). Previously a tab with components but no available updates showed a nonzero badge that contradicted the "Showing 0 with available updates" text.
- The hide-login feature no longer blocks front-end AJAX (GH #219). It was 404ing logged-out requests to `admin-ajax.php` (and `admin-post.php`), which many themes and page builders use for front-end forms; those endpoints are now excluded from the block while the actual login and dashboard pages stay hidden.
- Two-factor sign-in is clearer and slightly more forgiving (GH #215). The "incorrect code" message now explains that a code can only be used once (re-submitting the same code, for example from a password manager, is correctly rejected until your authenticator shows the next one); the accepted time window was widened to plus or minus 60 seconds for minor clock drift; and the code used to finish 2FA setup can no longer be reused for the first login. The single-use replay protection is unchanged.

## [0.61.57] - 2026-07-14

### Changed

- Agent code-quality and WordPress.org-compliance hardening, with no behavior change for managed sites. The real-user-monitoring collector now loads through the standard `wp_enqueue_script` async mechanism instead of a hand-built script tag; the two-factor and forced-password-change login screens now escape their output through `wp_kses` with an explicit tag allowlist at the output boundary; the long-running backup, restore, dump, and media routines now use a bounded 900-second time limit instead of an unlimited one; the CloudPanel integration inline-sanitizes its server-variable reads; and a stale storage-path note and a broken readme link were corrected. These improvements land in both the self-hosted agent and the WordPress.org build (fleet-agent-site-manager), which keeps the operator features (auto-login, updates, backups) and strips only the self-updater.

## [0.61.56] - 2026-07-11

### Fixed

- Automatic update rollback now recovers a site that an update left fataling on every request (GH #210). Previously, when a plugin or theme update applied cleanly but then caused a site-wide PHP fatal on every WordPress request, the automatic rollback could not run: it was dispatched to the same WordPress endpoint that was now fataling, so the site stayed fully down (front end and admin) until someone recovered it manually at the filesystem level. A new update watchdog loads before regular plugins and, when it detects a genuine post-update fatal, restores the pre-update snapshot directly at the filesystem level without needing WordPress to boot. It fires only for a real fatal attributable to the just-updated plugin or theme, within a short window after the update, and disarms itself as soon as the site boots healthily, so it cannot revert a working update. If the site is still reachable, rollback continues to work exactly as before. The dashboard also now shows this "site not responding, recovery attempted" condition distinctly instead of a generic rollback failure.
- The "Available updates" panel no longer shows a phantom update whose target version equals the version already installed (GH #211), for example a theme listed as updatable from a version to the same version. WordPress's own update cache can occasionally hold such an entry; nothing in the pipeline compared the offered version to the installed one, so it surfaced as a real update and inflated the update count. The agent now suppresses same-version entries, the control plane drops them defensively (which also clears any already-stored phantoms on the next view, with no re-sync), and the update wizard no longer pre-selects them.
- The "Refresh" button on a site's available updates now forces a real check against WordPress.org instead of sometimes returning the same cached list (GH #212). The refresh only bypassed WPMgr's own short lock, not WordPress core's separate, roughly twelve-hour check throttle, so it could take several clicks to match what the site's own Updates screen showed immediately. An explicit refresh now clears the relevant update caches and forces the underlying checks, so a single click returns current data. The scheduled background refresh stays gentle and is unchanged. A related gap in the update-apply path's own freshness check is fixed too, and a rollback now clears the update cache so the rolled-back-from version is not left showing as available.

## [0.61.55] - 2026-07-11

### Fixed

- Self-hosted media-encoder no longer crash-loops on boot after upgrading to v0.61.54 (GH #207). The v0.61.54 River-schema safety change began preparing the dedicated `media_encoder` schema on every boot, which needs database-level CREATE privileges even when the schema already exists. On installs where the media-encoder connects with the unprivileged application role (no owner migration DSN configured for that service), this failed on every start. The media-encoder now detects an already-prepared schema and skips the privileged setup entirely, so a role with only the standard read and usage grants boots normally. Genuine first-time creation still needs an owner role and now reports a clear, actionable message instead of a raw database error.
- Screenshot capture now works on the bundled media-encoder image (GH #207). The image did not create a home directory for the non-root user it runs as, so Chromium's crash handler could not start and every capture failed before the page loaded. The image now provides a real, writable home directory.
- A failed screenshot capture is no longer recorded as a successful job (GH #207). An infrastructure failure, such as Chromium failing to launch, is now retried a bounded number of times so a transient or since-fixed environment recovers on its own, while a genuine site-level failure (an unreachable site, or one that blocks headless browsers) is still recorded without pointless retries. Every failure continues to update the dashboard's "didn't finish" state.
- Core, plugin, and theme updates targeting "latest" no longer report "already up to date" when the site's cached update information is momentarily stale (GH #208). The agent now forces a fresh update check before deciding there is nothing to do, and an inability to determine availability is no longer treated as "up to date". The update is attempted, and its own fresh check settles it, rather than being silently skipped.
- Update runs no longer spuriously report "failed" when a heavy update takes longer than 30 seconds on the agent (GH #208). A real update performs a pre-update snapshot, download, extraction, and, for core, a database migration synchronously, which can exceed the shared 30-second dispatch timeout even though the update itself completes. Update dispatch now uses a dedicated client with a longer timeout, matching the treatment already given to backup and media commands.

## [0.61.54] - 2026-07-10

### Fixed

- On self-hosted installs where the media-encoder ran using the API's default database schema (an unset or `public` `WPMGR_RIVER_MEDIA_SCHEMA`), it could silently take over background-job leader election and stop every scheduled fleet job, including backups, uptime checks, and cleanups, with no error anywhere (GH #205). The media-encoder now refuses to start in that misconfiguration instead of risking it. The safe, dedicated schema is now the built-in default, so no configuration change is needed on a fresh or existing install; the API also logs a clear warning if it ever sees itself configured to share its schema with a media-encoder. Any media or screenshot jobs left behind in the shared schema from before this fix are cleaned up automatically.

## [0.61.53] - 2026-07-10

### Added

- Sign up directly into a paid plan from the marketing site. Choosing Starter, Agency, or Scale on the pricing page now carries that choice through signup and email verification (persisted server-side, so it survives even if you open the verification email on a different device) and lands you on a "Complete your subscription" screen that opens checkout for the plan you picked, with a "Skip for now" option that keeps the free account. Previously every plan button led to the same generic signup and left you on the free plan to find billing yourself. Hosted only; self-host is unaffected.
- Live pricing on the marketing pricing page. Prices are now fetched from the payment providers (Razorpay and Stripe) at build time rather than hardcoded, with a USD/INR currency toggle, so the amounts shown always match what is actually charged. A new public, cached pricing endpoint serves these amounts (no secrets, hosted only), and the page falls back to its built-in prices if the endpoint is unreachable so a fetch hiccup never breaks the site.

### Fixed

- The Stripe checkout return link pointed at `/billing` instead of `/settings/billing`, which showed a not-found page on return from Stripe; it now forwards correctly so the "finalizing your subscription" confirmation appears as expected.

### Fixed

- The fleet Core Web Vitals "worst offenders" table now shows each site's name and URL (GH #202). The rows were built from the metrics data alone, which is keyed only by site id, so the Name/URL columns rendered blank. The final (top ten) rows are now enriched with a single site lookup, so you can see which sites they are.
- Audit-log entries for backups, restores, and updates are now associated with their site (GH #201). Previously only events whose target was the site itself carried a site association, so backup/restore/update entries showed no site name and the "filter by site" control silently returned zero rows for them. The site filter now also matches the site id recorded in those entries' metadata (and the schedule target), and the log shows the site name for them. This is a read/display change only, the tamper-evident audit chain is untouched, and no migration is needed.

### Fixed

- The fleet "Sites with items to review" database-health stat is no longer a dead end (GH #197). It was an inert number, and the data behind it only covered the ten largest databases, so sites flagged for review outside that window could not be reached at all. The stat now expands into the full list of every site with database items to review, each linking directly to that site's Orphaned-items view.
- The database-size "90-day history" trend and the fleet "90-day growth" stat now populate automatically (GH #196). They read a size-history table that was only ever written on a manual "Scan database" click, so the trend stayed frozen. A size sample is now recorded from each site's daily diagnostics push (best-effort, so it can never affect the diagnostics itself), so the history builds over time for every site with no manual scanning. No agent update or database migration is required.

### Fixed

- Switching organizations now takes effect immediately instead of only after a browser refresh (GH #186). The switch was clearing the cached data but not re-fetching it, so the view stayed on the previous organization until something else happened to trigger a reload; this was most visible when switching to an empty organization (which produced no live events to nudge a refresh). The same latent issue in the organization-delete path is fixed too.
- Fleet-wide Core Web Vitals pass rate and the "worst offenders" table now consider all three Core Web Vitals, not just LCP (GH #195). A site with a good LCP but a failing CLS or INP was previously counted as passing in the fleet headline and never surfaced as a worst offender, even though its own per-site page correctly showed "Does not pass"; the fleet view now marks a site as passing only when LCP, INP, and CLS all pass, and surfaces sites failing on any of the three.
- The CLS good / needs-improvement / poor distribution bar no longer contradicts the p75 rating shown next to it (GH #185). The distribution was classifying whole histogram buckets by their lower edge, but the CLS thresholds fall inside a bucket, so a value that the p75 correctly rated "needs work" could still show as 100% good. The distribution now splits a straddling bucket proportionally at the threshold, using the same assumption the p75 calculation already uses, so the two agree. LCP and INP distributions are unchanged.

### Fixed

- Backup breadcrumbs (GH #188). When viewing a specific site backup, the navigation trail now reads Sites > [site] > Backups (mirroring the Updates section) with the "Backups" crumb linking back to that site's own backup list, instead of a dead-end trail that forced you to use the browser back button. The snapshot detail now lives under the site's own URL.
- Refresh screenshot no longer silently does nothing (GH #187). If the screenshot could not be captured (most often because the media-encoder service is not running), the card no longer spins forever after a false "queued" message; it now stops and shows a warning, and the server returns clearer, specific errors ("service isn't running" / "not configured") instead of a generic failure.

### Changed

- Self-hosted installs now run the media-encoder by default (GH #187). It was previously opt-in behind a Compose profile, which silently disabled site screenshots and the Media Optimizer on a default `docker compose up`. It now starts with the base stack (it runs headless Chromium, so it adds some memory/CPU; you can opt out with `docker compose up -d --scale media-encoder=0`). The hosted service is unaffected.

### Fixed

- Plugin and theme updates could still fail and automatically roll back on ordinary hosts, even after the previous fix in this area (agent 0.61.20, GitHub issue #131). That release pinned WordPress's temporary download/unpack directory to a folder inside wp-content whenever that folder was confirmed writable, but on a standard host the folder it pinned to is also the exact working directory WordPress itself uses to unpack an update, and the pin caused the two to collide: WordPress cleared that folder as its first unpacking step, wiping out the update package the agent had just told it to download there, so the update failed before any files were ever copied. The agent no longer pins that directory unless WordPress's own default temporary location is confirmed unusable in this hosting environment (proven with a real write test, not just a permissions check); when the default already works, which is the case on most hosts, it is left completely alone. On the small number of restricted hosts (for example open_basedir or certain managed-hosting setups) where the default genuinely does not work, the agent still falls back to a writable location inside the site, but now a dedicated one that WordPress's own unpacking step never touches, so the collision cannot recur. Separately, when an update does fail, the agent-reported log now includes WordPress's own explanation of what went wrong (for example a download or unpacking problem) instead of a generic "Update failed" message, making any future occurrence of this class of issue immediately diagnosable from the control plane's logs.

## [0.61.47] - 2026-07-08

### Fixed

- Plugin and theme updates could fail and automatically roll back on some hosts, even though the same updates worked before a recent hardening change (agent 0.61.20). That change started pinning WordPress's temporary download/unpack directory to a folder inside the site's own wp-content directory, to keep updates working on hosts that restrict the system-wide temp location. On a host where that wp-content folder exists but is not actually writable in the context the update runs in (for example some open_basedir or managed-hosting setups), the pin itself broke the download and unpack step for every plugin and theme update, including free and premium plugins alike. The agent now only applies that pin when the folder is confirmed writable; otherwise it leaves WordPress to fall back to its own default temporary location, restoring the behavior that worked on this class of host before the change. Hosts where the folder is writable are unaffected.
- The update run detail now shows the full agent log for a failed or rolled-back task. Previously the reason an update was reverted was clipped to a short preview and effectively hidden; each failed task now has a "View log" toggle that reveals the complete agent diagnostic (including the exact reason and the on-disk version it found), and a button to copy it.

## [0.61.46] - 2026-07-08

### Fixed

- A WordPress dashboard bulk update could fatal error and strand the whole site in maintenance mode when it included a premium plugin that manages its own updates (agent 0.61.19, GitHub issue #182). The agent's self-update integration hooks into a shared WordPress filter that runs for every plugin and theme download, and some premium plugins legitimately leave that download's package location empty until their own license check succeeds; the agent's code was not written to expect that and crashed instead of skipping the download. It now recognizes and skips any download that is not its own, leaving WordPress to continue handling the other plugin normally. The agent's own signed, verified self-update path is unaffected.
- When a plugin or theme update is applied but then detected as incomplete and automatically rolled back, the control plane's "View logs" now shows the specific reason (for example, the installed files did not validate, or the update package never actually landed) instead of only recording it in a debug log most users never see.

## [0.61.45] - 2026-07-08

### Fixed

- A good plugin or theme update could be reported as failed and automatically rolled back on some hosts, even though it had actually applied correctly (agent 0.61.18). After applying an update, the agent re-checks that the plugin or theme is still readable before declaring success; on some hosting environments that re-check could read a stale, cached view of the filesystem left over from just before the update and see the just-updated files as missing, treating a perfectly good update as incomplete and reverting it. The agent now clears that stale cache before the check, so this false failure can no longer happen; a genuinely incomplete update (a real half-written plugin or theme) is still caught and rolled back exactly as before. A failed check now also records the specific reason and what was actually found on disk, so any future case is easy to diagnose from the log alone.

## [0.61.44] - 2026-07-08

### Fixed

- Plugin and theme updates failing with a snapshot error on some hosts (agent 0.61.17). A hardening change had made a pre-update safety snapshot mandatory and refused the update outright when the snapshot could not be captured, which broke every plugin and theme update on hosts where the snapshot path could not be resolved (for example open_basedir or symlinked wp-content setups); core updates were unaffected. The agent now resolves the snapshot source correctly on those hosts, and if a snapshot still cannot be taken it proceeds with the update anyway (relying on WordPress's own rollback plus the post-update health check) instead of blocking it. The full snapshot-and-auto-restore protection is unchanged on hosts that can take a snapshot.

## [0.61.43] - 2026-07-08

### Added

- Razorpay as a second payment provider for hosted billing, alongside Stripe (behind WPMGR_HOSTED, so self-hosted installations are unaffected and billing stays disabled). It supports dual-currency subscriptions (USD for international customers, INR for India), an in-app Razorpay Checkout.js payment modal, subscription cancellation at the end of the billing period, and a signature-verified webhook that activates the plan. Customers choose their payment provider at checkout. The adapter is hand-rolled on the standard library (no third-party SDK), the webhook is the sole authority that changes a plan (raw-body HMAC-SHA256, constant-time), and tenant attribution is server-stamped so a payment can never affect another account.

## [0.61.42] - 2026-07-08

### Fixed

- Completed the admin two-factor-access fix from the previous release: the instance superadmin was still redirected back to the admin area when opening the Security (2FA) settings. Superadmin accounts are intentionally kept out of the tenant-scoped app, but that route guard also blocked their own personal account pages. The superadmin can now reach their own Account and Security settings (to enable 2FA) while still being kept out of the tenant-scoped screens.

## [0.61.41] - 2026-07-08

### Fixed

- Admin panel improvements. Two-factor authentication settings are now reachable for every account: they were hidden for any account without an organization (including the instance superadmin), and a "Security" shortcut was added to the account menu so it is one click away. The instance-admin console is now full width with a single, cleaner sidebar navigation instead of the previous boxed, narrow layout with a wasted gutter. And the Accounts list pagination now advances correctly, the Next and Previous controls were resetting back to the first page on every click.

## [0.61.40] - 2026-07-08

### Fixed

- Deleting an empty organization (which is removed immediately) no longer shows a misleading "recoverable during the grace window" message; that wording now appears only for a soft delete that actually has a grace window. Internally, the organization-purge and billing-reconcile background sweeps now run on their dedicated single-worker queues as intended (they were falling back to the shared default queue), and an invalid `WPMGR_ORG_PURGE_GRACE_DAYS` value is now logged rather than silently ignored.

## [0.61.39] - 2026-07-08

### Fixed

- The Real User Monitoring (RUM) beacon key could get permanently stuck. If the one-time delivery of the key to a site was ever lost, the site was left showing RUM as enabled while silently collecting nothing, with no way to recover short of editing the database. The control plane now tracks whether the site has actually confirmed it holds a key (rather than only whether one was ever generated) and automatically re-issues the key when the site reports it is missing one. A "Rotate beacon key" action was added for manual recovery, and the dashboard now shows a warning when RUM is on but the site has not confirmed its key. The site agent also refuses to overwrite a working key with an empty one. Requires agent 0.61.16 (older agents will re-issue the key on the next settings save until updated, which is harmless).

## [0.61.38] - 2026-07-08

### Fixed

- Real User Monitoring (RUM) data was never being collected on self-hosted installs: the bundled reverse-proxy configuration had no route for the `/rum/` endpoint, so every beacon request was rejected with a 405 error before it reached the application. The same gap also silently dropped inbound email-provider and billing webhooks (`/webhooks/`). The proxy now routes both `/rum/` and `/webhooks/` to the API. A new CI check exercises the real proxy configuration against every public endpoint, so a future public route added without its proxy entry is caught before release. Redeploy the web image to pick this up.

## [0.61.37] - 2026-07-07

### Fixed

- Fixed the "hide login" security feature, which had two bugs. First, turning it on showed a scary "policy stored but agent push failed" error even though the policy was actually saved and applied correctly. That error was a harmless mismatch (an empty value was sent as an array instead of an object) and is now gone; the control plane also tolerates both shapes so an older agent can't trip it. Second, and more seriously, the secret login URL did not actually show a login form (it bounced to the home page), which could leave you unable to log in through the browser. The secret URL now serves the login form correctly while the default wp-login.php stays hidden (returns 404). The fix also closes two smaller security gaps found in review: the secret URL is no longer exposed in ordinary page links to logged-out visitors, and the internal access cookie is now signed so it can't be forged. Requires agent 0.61.15.

## [0.61.36] - 2026-07-07

### Fixed

- Fixed a backup-integrity bug where an incremental backup could fail with a "stalled" error because retention cleanup had deleted a parent snapshot's file-list data chunk while it was still well within the retention window, permanently breaking the incremental chain. The cause was that the internal reachability check which decides what to keep could be confused by duplicate snapshot rows at the same chain position (left behind by failed retries), letting a failed attempt shadow the real completed snapshot and drop its chunk from the "keep" set. Retention now always protects the completed snapshot at each chain position, and as a ground-truth safety net it never deletes a data chunk that any surviving snapshot still references, so a reachability mistake can never again cause silent data loss. A broken incremental chain now fails fast with a clear "run a full backup" message instead of stalling silently for two minutes. A migration de-duplicates any existing same-position snapshots and adds a constraint that prevents recurrence.

## [0.61.35] - 2026-07-07

### Fixed

- The Vulnerabilities and Performance tiles on a site's Health tab were placeholders that always read "Not scanned yet" and "Not measured yet" regardless of the real data. They now show live results: the Vulnerabilities tile shows the open-vulnerability count and worst severity (or that the feed is not configured, or that no known vulnerabilities were found), and the Performance tile shows the site's Core Web Vitals (LCP p75) from real-user monitoring. Each tile links through to the full view (the Security tab and the Performance dashboard). A site that has not been scanned yet, or has no visitor data yet, shows an honest empty state instead of a fabricated number.

## [0.61.34] - 2026-07-07

### Added

- Hosted service only: managed backup storage is now a paid-plan feature. On a paid plan, backups can use WPMgr's managed storage as before; on the free plan, backups must target your own storage (a local folder or your own S3-compatible bucket, configured under Destinations). This is inactive by default, ships behind the hosted-billing switch, and does not affect self-hosted installs, which always keep managed storage. Restoring an existing backup is never restricted, even after a plan change, and tenants that already exist when the gate is enabled keep their managed storage.

## [0.61.33] - 2026-07-07

### Fixed

- Backup destinations other than managed storage now actually work. You could configure a local folder or your own S3-compatible bucket as a backup destination and the "Test connection" check passed, but every backup still went to managed storage, because the control plane never told the site which destination to use. Backups (full and incremental) now run to the configured destination and restore reads back from it, for all three types: managed storage, a local folder on the server, and your own S3-compatible bucket. For your own bucket the control plane signs the uploads and downloads, so the site never holds your storage credentials. Requires agent 0.61.14.

## [0.61.32] - 2026-07-07

### Fixed

- The backup working directory (`wpmgr-agent/runs`) was only cleaned up after a successful backup, so a failed or interrupted backup run left its temporary files behind permanently, slowly consuming disk on the site (a small fleet had accumulated over a gigabyte this way). A daily janitor now removes the scratch directories of finished runs once they are safely past being active (older than six hours and not a currently-running backup), reclaiming that space. It never touches an in-progress backup and fails safe on anything it cannot read. Requires agent 0.61.13.

## [0.61.31] - 2026-07-07

### Added

- Restore now verifies the site actually loads afterward and automatically rolls back if it does not, instead of reporting a broken restore as complete. After the files and database are swapped, a first check confirms the restored database is intact while the site is still in maintenance mode (so a bad database restore is reverted before anyone sees it); then, once the site is live again, a second check confirms it is not serving a fatal error. If either check finds a genuine failure, the restore reverts both the files and the database to their pre-restore state and reports the run as failed with the reason, rather than leaving the site down. A pre-restore database snapshot is captured before the swap and, together with the pre-restore files, is kept for the retention window so a manual rollback stays possible. The checks fail open on an unreachable or ambiguous response so a network blip can never roll back a good restore. Requires agent 0.61.12.

## [0.61.30] - 2026-07-07

### Added

- Uptime incidents are now recorded and kept, so the Incidents panel shows real history: past incidents with accurate durations and a flapping indicator for sites that go down repeatedly. Previously the panel was derived from current state only and forgot an incident the moment a site recovered. Clicking an incident opens a detail view with the site's live status, the probe result sequence over the incident window, a timeline of what else happened on the site around that time (recent updates, backups, activity, and PHP errors), uptime over 7 and 30 days, and quick actions (log in to the site, re-check the connection, run diagnostics, and links to the site's Health, Backups, Updates, Activity, and Errors tabs). Incident history starts accruing from this release.

## [0.61.29] - 2026-07-07

### Fixed

- The Uptime page Incidents panel showed wrong information for an ongoing incident: a site that was down was labeled "Degraded", its duration read "NaNh", and the site name was blank. Incident rows now show the correct severity ("Down"), read "ongoing" while an incident is open, and always show the site name. Each row is also now labeled ("started ... ago", "for ...h") and links to the site so you can drill in.
- Native dropdown menus were unreadable in dark mode because the expanded option list rendered on a light background. Native controls now follow the app theme in both light and dark mode.
- Two breadcrumb links (Restores, Schedule runs) pointed at pages that do not exist and returned a 404 when clicked. Those segments are no longer clickable.
- The "Open site" action in a site's row menu did nothing. It now opens the site's detail page, from both the list and grid views.

## [0.61.28] - 2026-07-07

### Added

- Organization owners can now delete an organization from Settings, Organization (a Danger Zone with a type-the-name confirmation). Deletion is scheduled with a grace window: the organization is hidden immediately and permanently removed after the window passes, during which it can still be recovered. Removing an organization disconnects its sites' agents and deletes its backups and all stored data. An empty organization is removed immediately.

### Fixed

- Switching the active organization did not refresh live data. The dashboard's real-time event stream stayed connected to the previously active organization, so live updates for the newly active one did not arrive until a full page reload. Switching organizations now reconnects the live stream to the new organization (the cached data was already refreshed correctly; the live stream was the missing piece).

## [0.61.27] - 2026-07-07

### Fixed

- Real User Monitoring collected no data on sites served by a third-party page cache. The collector script was injected only inside WPMgr's own page-cache output, so when a different cache served the pages (the common setup when a dedicated caching plugin is active, or when WPMgr's own cache was off) the script was never delivered and the Performance dashboard stayed empty with no warning. The collector is now injected on a standard WordPress hook during page generation, so whichever cache serves the page captures it and RUM works independently of WPMgr's own cache. Requires agent 0.61.11. As part of this the beacon now sends the page path without its query string, so per-page metrics no longer carry query parameters.

## [0.61.26] - 2026-07-06

### Fixed

- Uptime downtime and recovery alert emails were silently skipped when SMTP was configured in the dashboard (Settings, Email/SMTP) but not also set as environment variables. The alert mailer only read the environment relay and ignored the saved dashboard relay that the "Send test email" button and backup notifications already use, so alerts were dropped even though a test email delivered. Uptime alerts now send through the same saved SMTP relay, resolved per send, with environment variables kept only as a fallback. This path is now SSRF-hardened like the rest of the mail system.
- The audit log recorded "Emailed: Yes" for a downtime alert whenever recipients were configured, even when the send was skipped or failed, so operators could not tell that notifications were not being delivered. The audit entry now records the true outcome (Sent, Skipped, or Failed) with the reason, for both email and webhook delivery. Alert reasons never include SMTP hosts, credentials, or endpoint responses.

## [0.61.25] - 2026-07-06

### Fixed

- Restore could silently drop plugin or theme files whose path contained a reserved drop-in name (for example a plugin's own `class-db.php`), leaving the site with a fatal error while the restore reported success. The restore now matches its protected-file exclusions by exact path instead of substring, so only genuine WordPress root drop-ins (`db.php`, `object-cache.php`, `advanced-cache.php`) and config files are held back and every other file is restored. This also stops a nested `.htaccess` from being dropped on restore. Affected agent updated to 0.61.10. Sites already broken by an earlier restore can recover by re-restoring from the same snapshot with the updated agent, or by reinstalling the affected plugin.

## [0.61.23] - 2026-07-06

### Fixed

- The superadmin billing admin panel could still fail to load because its pages use tooltips but the admin area did not provide the tooltip context they require. The admin layout now provides it for every admin page. Also corrected the account list's idle filter so it takes effect. Superadmin-only; no customer impact.

### Changed

- The admin billing endpoints are now described in the API specification, so the dashboard's types are generated from it and checked against the server, preventing the shape mismatches that caused the earlier admin panel load failures.

## [0.61.22] - 2026-07-06

### Fixed

- The superadmin billing admin panel could crash on load because the frontend read some response fields under names that did not match what the server sent. Aligned every field to the server shape and guarded optional values so a missing value renders as a placeholder instead of failing. Superadmin-only; no customer impact.

## [0.61.21] - 2026-07-06

### Added

- A superadmin billing admin panel (behind the same off-by-default hosted flag): an accounts overview with plan, usage-versus-limit, and payment status; a per-account detail screen with a full activity timeline; a revenue overview; and operator controls to comp an account, adjust per-account limits, extend a grace period, force a billing state, or suspend and restore access, each requiring a reason and recorded in the audit log. Self-hosted installations are unaffected; no user-facing change in this release.

## [0.61.20] - 2026-07-06

### Changed

- More hosted-plans groundwork landed behind the same off-by-default flag: subscription checkout, a billing management page, and payment-webhook handling, built to support more than one payment provider. Self-hosted installations are unaffected and remain unlimited; no user-facing change in this release.

## [0.61.19] - 2026-07-06

### Fixed

- Hardened two internal database functions that could leave an elevated permission context active for the remainder of a transaction after being called. Not exploitable through any shipped code path, found by internal security review; fixed as defense in depth.

### Changed

- Groundwork for hosted plans landed behind a flag that is off by default. Self-hosted installations are unaffected and remain unlimited; no user-facing change in this release.

## [0.61.18] - 2026-07-05

### Fixed

- TLS certificate expiry now shows for monitored sites. The uptime prober only read the certificate during a fresh TLS handshake, but its keep-alive connection meant a fresh handshake almost never happened after the first probe, so the TLS column on the Uptime page, the per-site Uptime tab, and the sites-list SSL badge stayed empty from the start. The prober now reads the certificate from the response on every probe, fresh or reused connection. Values appear on their own within a minute of upgrading; no other changes needed.

## [0.61.17] - 2026-07-05

### Added

- Bulk snapshot delete, chain-aware. Select multiple backups with checkboxes (including whole incremental chains via a tri-state chain checkbox), use one-click filters for all failed or all zero-byte runs, and delete the batch in one action. Selecting a snapshot automatically includes the later generations that depend on it, shown before you confirm, so a chain can never be left broken. The server deletes newest generation first, re-checks dependents live, skips locked, running, or actively-restoring snapshots individually (one bad row never blocks the rest), and reports a per-snapshot outcome.
- Right-sized delete confirmation. A batch of failed or zero-byte runs needs one plain confirmation instead of typing an id per snapshot. A batch containing any completed backup asks you to type one phrase for the whole batch.
- Deleting a snapshot now refuses while a restore that reads it is in progress. This guard also applies to single-snapshot delete, which previously had no such check.

## [0.61.15] - 2026-07-04

### Added

- Audit log integrity re-baseline. A "Chain break" that predates the integrity-locking fix is a permanent artifact, because the audit log is append-only and its rows can never be altered. An owner can now acknowledge such a break, which moves the integrity anchor to the current point so verification runs forward from there. New tampering is still detected, the acknowledgment is itself recorded in the audit log, and no existing entries are ever removed.

### Changed

- Each audit row now shows its date, not just the time, so a row keeps its date when scrolled away from its day header.

### Fixed

- The audit log Reload button now visibly refreshes both the event list and the integrity check, with a spinner while it works.

## [0.61.14] - 2026-07-04

### Fixed

- Fleet Audit log appeared to stop recording: the event list was ordered oldest-first while presented as newest-first, so once a tenant passed one page of events the recent activity was paged off the end. It now lists newest events first. No audit data was ever lost.
- The Audit "Chain break" integrity warning could fire from two operator actions being recorded at the same instant (a harmless race, not tampering). Audit writes are now serialized per tenant so the integrity chain stays intact.
- The Audit page could fail to load for accounts that had any automated activity (uptime alerts, backups, scans, updates), because a query change did not guard a type cast on system-generated rows.

### Changed

- Redesigned the fleet Audit log. Events now read as plain sentences (no raw internal event codes), the operator who performed each action is shown by name instead of an ID, and long runs of routine file reads collapse into one expandable line so that writes, deletions, and denied actions stand out. Added an outcome filter (Denied, Writes, Sensitive), a search box, exact timestamps, and a per-event detail view that shows the reason an action was denied.

## [0.61.10] - 2026-07-02

### Fixed

- **Multi-site bulk update now only creates a task for a site that actually has the selected update pending.** Previously a bulk update created a task for every selected site regardless of whether that site had the chosen plugin, theme, or core update available, so a site without the update simply failed. A target that does not apply to a site is now reported as skipped, not failed. (#126)

## [0.61.9] - 2026-07-02

### Fixed

- **A failed plugin or theme update no longer leaves a site stuck in WordPress maintenance mode (critical).** The post-update health check treated a transient 503 from an in-progress database migration as a failure and rolled back an update that had actually succeeded, leaving the site's maintenance page (`.maintenance`) showing 503 to every visitor. The health check now tolerates a brief, migration-related 503 instead of rolling back a successful update. (#127)

## [0.61.6] - 2026-07-02

### Fixed

- **Downtime email alerts now fire reliably.** A race in the alert state machine meant a sustained outage could go unalerted; alert state now transitions atomically so every qualifying outage sends its alert. (#124)
- **The Notifications settings page now saves correctly**, including the daily email digest toggle, which previously did not persist. (#123)
- **Fixed a slow first load of the Sites list after the dashboard had been idle.** A pooled database connection could be handed back to a request without being revalidated, and the 30-day uptime aggregate query was not using its covering index; both are fixed, so the first request after idle now loads at normal speed.

## [0.61.3] - 2026-06-26

### Fixed

- Saved CDN rewrite settings now reach the WordPress agent with the selected asset type scope while CDN credentials remain control-plane only.
- Backups now preserve plugin and theme vendor code in directories named `cache`, `upgrade`, or `upgrade-temp-backup` while still excluding runtime cache and update staging roots.
- Advanced page-cache bypass and variant settings now reach the WordPress agent so saved URL, cookie, and query rules affect the rendered cache drop-in.
- Isolated media-encoder River jobs into a dedicated schema in the bundled self-host profile so the encoder cannot take leadership for API uptime and cron jobs while image optimization and screenshot work still run.
- Backup schedule form no longer rejects valid input: selecting a day of week (weekly), a day of month (monthly), or an interval (every N hours) now satisfies the cadence requirement and saves. The backup scheduler also now logs when a due schedule is skipped because its site could not be resolved, so a missed scheduled run can no longer fail silently.
- Update runs list now shows each run's real task count, a marker when a run had failed tasks, and how many sites the run covered. On a finished run the detail progress bar now fills to completion instead of showing a sliver.
- Bulk update from the Sites page: the plugins / themes / core target now drives the update modal, the modal defaults to only items that have an available update, and plugins and themes are separated into tabs.
- Closing a dialog no longer leaves the page unclickable. A shared overlay could leave a pointer-events lock on the page after certain modals closed; the dialog now always clears it.
- A failed admin bundle no longer takes down the Sites app for superadmins: the admin area is isolated behind an error boundary with a link back to Sites, and superadmin can now be revoked with the `WPMGR_SUPERADMIN_REVOKE_EMAILS` environment variable (mirroring the grant) instead of a manual database change.
- Plugin "Changelog" links now use the plugin slug, producing a valid wordpress.org URL instead of a 404.
- The Sites overview Uptime column now shows per-site uptime instead of staying blank.
- "Open in wp-admin" for a multi-site selection now lists every selected site in a persistent panel with a per-site Open action, instead of a few auto-dismissing toasts.
- A site's "Backup schedule runs" panel now shows past completed and failed runs that already exist instead of always reporting none.

## [0.61.0] - 2026-06-23

### Added

- **File Manager.** Operators can browse, edit, upload, download, and manage files on any managed WordPress site directly from the dashboard, under a new Files tab on each site, without needing SFTP or cPanel access. Browse the full file tree; preview text files inline or download binary files via presigned URL. A sensitive-file deny-list (wp-config.php, .env, key files) requires owner confirmation before access. A separate per-site write toggle (off by default) unlocks editing, uploading (drag-and-drop), creating folders, renaming, deleting (typed confirmation), and chmod (safe modes only). Write operations reject executable content (PHP files, any file containing `<?php`) and refuse to touch protected roots (wp-admin, wp-includes, WordPress core). Zip any file or folder and download the archive; extract a zip back into the site with zip-slip and zip-bomb protection. Search files by name or content across the tree. Every edit and overwrite auto-saves an encrypted prior version that operators can browse and restore from a per-file version history panel. The file manager is off by default per site, restricted to owner and admin roles, and every read, write, delete, upload, extract, restore, and denial is written to the operator audit log. The Audit page is now a filterable timeline with filter-by-action-group (including "File manager") and filter-by-site; a "View activity" link in the file manager jumps straight to that site's file trail.
- **CloudPanel cache purge support in the agent.** On CloudPanel sites, WPMgr now clears its disk page cache and CloudPanel Varnish together: full-site purges send both host and cache-tag Varnish purges, per-URL purges clear the matching Varnish URLs, and full-site purges also clean up the host PageSpeed cache when writable. The optional CloudPanel WordPress plugin is not required.

## [0.57.7] - 2026-06-21

### Changed

- **New marketing website.** The public site at wpmgr.app is now a multipage Next.js application with server-side rendering and static generation for SEO, replacing the previous single-page site. It adds a dedicated page for every feature, solution pages by audience and by job, pricing, a searchable changelog, a resources area with guides and articles, a desktop megamenu plus a mobile drawer, and a self-hosted API reference at /docs generated from the OpenAPI spec with no external network at runtime. Faster, fully crawlable, and easier to extend.

## [0.57.0] - 2026-06-21

### Added

- **Connect the vulnerability feed from the dashboard.** Instance administrators can now set the Wordfence Intelligence API key from a new "Vulnerability feed" page in the admin area, instead of an environment variable. The page shows the live connection status (connected with the vulnerability count and last sync time, not configured, or an error), lets you save or remove the key, and has a "Sync now" action. The key is encrypted at rest and is never shown again after saving. Self-hosted instances can still set the key by environment variable.

## [0.56.0] - 2026-06-20

### Added

- **Vulnerability scanner.** WPMgr now checks every managed site's installed plugins, themes, and WordPress core version against the free Wordfence Intelligence vulnerability feed and flags anything with a known security issue. Each finding shows severity, the affected version range, the fixed version, and CVE references. Findings appear per-site on the Security tab (a new Vulnerabilities card and overview tile) and across the whole fleet on the Vulnerabilities page. One-click remediation updates a vulnerable plugin, theme, or core install to the fixed version using the existing update flow. Operators can dismiss a finding and restore it later. Requires a free Wordfence Intelligence API key to connect the feed; the UI shows a clear "feed not configured" state until the key is added.

## [0.55.0] - 2026-06-20

### Added

- **Two-factor enrollment flow for WordPress site users.** After an operator requires 2FA for a user role, the affected user now sees a guided setup screen on their next login: scan a QR code with any authenticator app, enter a confirmation code to activate it, then save a set of one-time backup codes before continuing. Users can also start enrollment proactively from their WordPress profile at any time. Previously the policy could require 2FA but there was no way for site users to actually complete enrollment.

### Changed

- **Redesigned per-site Security tab.** The Security tab is now a card-based layout. A status overview strip at the top shows active protection at a glance. Settings are grouped into collapsible cards: Login and Two-Factor, Password policy, Hardening, File integrity, Bans and login protection, and Hide login. Each card has a plain-language description of what it does and a color-coded status indicator. Replaces the previous flat list of toggles and eliminates the large empty area that appeared when few settings were active.

### Fixed

- Consistent success and error toasts across all security actions on the per-site Security tab.

## [0.54.0] - 2026-06-20

### Added

- **Two-factor authentication for WordPress site users.** Operators can require 2FA for chosen user roles on any managed site, enforced at the WordPress login screen. Supported methods: authenticator app (TOTP), email one-time code, and single-use backup codes. Configurable grace logins let users enroll before enforcement kicks in, and a remember-this-device window reduces friction for trusted machines. The WPMgr control plane and wp-config recovery constants can always bypass enforcement, so operators can never be locked out of a site they manage. Note: enabling 2FA applies to new logins; existing authenticated sessions remain valid until they expire naturally.
- **Password policy for WordPress site users.** Per-site policy options: minimum password strength, blocking known-compromised passwords (checked by a privacy-preserving prefix query against a free public breach corpus so the plaintext password never leaves the site), blocking password reuse, and optional password expiry with a forced-change screen on next login.
- **Hide login page.** Operators can move the WordPress login page to a secret address per site, reducing automated login attempts without breaking the WPMgr control plane's access to the site.

All three controls are per-site, opt-in, and off by default.

## [0.53.0] - 2026-06-20

### Added

- **File integrity monitoring.** Operators can run a file-integrity scan over the whole WordPress install or just wp-content, in addition to the existing core-file check. Scan scope is selectable per run: Core files, wp-content, or Full install. The control plane compares scanned file hashes against WordPress.org checksums for WordPress core and for wp.org-hosted plugins and themes, and against a learned per-site baseline for everything else. The scan reports changed, added, and removed files, and flags modified or unrecognized files in plugins and themes. A flagged file stays flagged on every subsequent scan until an operator reviews and explicitly accepts it; the baseline never silently advances past an unreviewed change. Uses free WordPress.org checksum data only, no external paid service. Results surface in the per-site Security tab alongside existing scan findings.

## [0.52.0] - 2026-06-20

### Added

- **Per-site WordPress hardening controls.** A new Security tab on every site lets operators push hardening settings directly from the control plane to the agent. Controls cover: disable the built-in file editor; XML-RPC (on, limited to pingbacks only, or fully off); REST API restriction; login identifier restriction (username, email, or both); force unique nicknames; block author-archive user enumeration; force SSL with HSTS; disable directory browsing; block PHP execution in uploads; and protect system files (wp-config.php, .htaccess, readme files). All controls are off by default and opt-in. The control plane and the operator's own session can never be locked out by a hardening rule.
- **Per-site ban list.** A durable, operator-managed list of blocked IP addresses, CIDR ranges, and user agents, stored on the control plane and enforced in the agent at early-boot and at the web-server config level. Broad blocks covering all addresses and private/loopback ranges are rejected. The operator's own allow-list IPs always bypass the ban, so a misconfigured ban can never lock the operator out. The ban list is also default-off and opt-in.
- **Site Security tab.** A dedicated Security area on each site in the dashboard surfaces hardening controls and the ban list in one place, gated to operators.

## [0.51.5] - 2026-06-20

### Fixed

- **Bulk and update-triggered backups now actually run.** The Sites "Run backup" bulk action, the command bar's "Run backup on selected sites" and "Run backup on all sites", and the Updates tab's "Take backup first" option previously showed feedback but never enqueued a backup. They now enqueue a real backup for each selected site (the same job a single-site backup creates), report accurate per-site results (including sites skipped because a backup is already running), and the toast's "View activity" link now goes to the right place. "Take backup first" waits for the backup to be queued before starting the update. The Sites "Open in wp-admin" bulk action now opens all selected sites instead of only the first. (#76)

## [0.51.4] - 2026-06-19

### Fixed

- **Insights to Uptime now shows real uptime for every site when the ClickHouse metrics backend is used.** The fleet uptime status endpoint, and the uptime/SSL column in the Sites list, read probe data directly from Postgres, so deployments using ClickHouse for metrics saw every site as "Unknown" with empty uptime, latency, and TLS fields even though the data existed (the per-site Health view and the uptime summary endpoint, which read through the metrics store, worked correctly). Both now read through the metrics store, so ClickHouse and Postgres deployments display correct status, 7-day uptime percentage, average latency, TLS expiry, and last-check time. (#74)

## [0.51.3] - 2026-06-19

### Fixed

- **Scheduled backups no longer re-fire every few minutes.** A schedule whose next-run time had slipped into the past (most often after being disabled and re-enabled) was re-triggered on every scheduler tick, producing many runs per night, overlapping runs, and a daily incremental chain climbing several generations in one night. Re-enabling or an already-overdue schedule now advances to the next future run slot; the scheduler claims and advances each due schedule in a single atomic step; only one backup per site can be in flight at a time; and any schedules already stuck in the past are healed automatically on startup. (#68)

## [0.51.2] - 2026-06-19

### Changed

- **The agent now installs its early-boot security helpers only when you turn the feature on.** The login firewall and the fatal-error trap each run as a small must-use helper file. Previously the error-trap helper was written on activation; now neither helper is written until you explicitly enable the corresponding feature, and both are removed when you turn the feature off or deactivate the plugin. A freshly activated agent writes nothing outside its own plugin folder. Error capture inside a normal request continues to work regardless.

### Added

- **Crawlable Terms and Privacy pages.** manage.wpmgr.app/terms and /privacy now serve full static content to link checkers and reviewers that do not run JavaScript, while the in-app pages keep working for normal navigation.
- **"Get started for free" call to action** on the marketing site, linking to hosted signup.

### Fixed

- **Client IP resolution validates every candidate** before it is recorded, and the optional public-IP lookup now links the provider's terms and privacy pages.

## [0.51.1] - 2026-06-16

### Changed

- **Destinations and Alerts moved to their natural homes in the sidebar.** Destinations (where backups are stored: control-plane managed storage, a local folder, or an S3-compatible bucket) was previously under Settings but is not an account setting. It now lives at /destinations under the Operations group, next to Backups. Alerts (how the tenant is notified when monitored sites go down) was previously under Settings but is a monitoring concern. It now lives at /alerts under the Insights group, next to Uptime. Settings now holds only true account and organisation configuration: Account, Security, Organisation, API keys, Email / SMTP, and Members.

Dashboard only at 0.51.1; no control-plane, migration, or agent change.

## [0.51.0] - 2026-06-16

### Changed

- **Settings has a real page.** Visiting /settings now renders a dedicated Settings area with a left vertical side-menu listing every account and organisation settings section; the content fills the right panel. On mobile the side-menu collapses to a horizontal scroll strip. Visiting /settings lands on Account. Previously /settings rendered nothing.
- **Main sidebar sections are collapsible.** The grouped navigation sections (Operations, Insights, Security) can now be opened and closed individually. They start collapsed to keep the sidebar short; the group that contains the page you are currently on auto-expands on load; manual open and close choices are stored and remembered across visits.
- **Eight settings links in the sidebar became one.** The individual settings links (Account, Security, Organisation, API keys, Email / SMTP, Members, Destinations, Alerts) that occupied the sidebar separately are replaced by a single "Settings" entry that opens the settings area.

Dashboard only at 0.51.0; no control-plane, migration, or agent change.

## [0.50.5] - 2026-06-16

### Fixed

- **Sites table no longer overlaps its columns on mobile.** The table's minimum width was smaller than the sum of its columns, so on a narrow screen the fixed layout squeezed every column together and the Site and Client headers overlapped. The minimum width now matches the columns, so on a small screen the table keeps its widths and scrolls sideways instead. The grid view remains the more compact option on a phone.

Dashboard only at 0.50.5; no control-plane, migration, or agent change.

## [0.50.4] - 2026-06-16

### Fixed

- **Sites table no longer misaligns when rows are selected.** Selected rows used a relatively-positioned row with an absolute accent strip, which pulled the row out of the fixed table column grid so its cells drifted from the headers. Selection is now shown with a background tint that does not affect layout, so selected and unselected rows stay aligned.

Dashboard only at 0.50.4; no control-plane, migration, or agent change.

## [0.50.3] - 2026-06-16

### Fixed

- **Sites table columns now line up with their headers.** In the list view the row cells could drift one column to the right of the header labels (the site name appeared under Client, versions shifted over). The table now uses a single colgroup as the authoritative column geometry shared by the sticky header and the virtualized rows, matching the fleet tables.

Dashboard only at 0.50.3; no control-plane, migration, or agent change.

## [0.50.2] - 2026-06-16

### Fixed

- **"Remember this device" now actually persists.** Signing out was clearing the trusted-device marker, so every sign-in asked for the second factor again and each time added a duplicate trusted device. A trusted device now survives sign-out for its full window (you still enter your password; only the second step is skipped), and is cleared only when you change or reset your password, disable two-factor, or revoke the device.

Control plane only at 0.50.2; no migration, no agent change.

## [0.50.1] - 2026-06-16

### Fixed

- **Two-factor sign-in landed on the login page instead of the dashboard.** After entering a valid code, the dashboard navigated before the new session was confirmed, so a route guard bounced back to sign-in until a manual refresh. It now fetches the authenticated session first and then routes, matching the password-login path.
- **Passkeys could not be added or used.** The browser passkey ceremony double-wrapped the options the server sent, so the browser reported a missing relying-party key. The options are now passed through as-is for both registering and signing in with a passkey.

Dashboard only at 0.50.1; no control-plane, migration, or agent change.

## [0.50.0] - 2026-06-16

### Added

- **Two-factor authentication for the dashboard.** Operators can now protect their account with a second factor: an authenticator app (TOTP) and/or a passkey or security key (WebAuthn/FIDO2). Setup is a guided flow (scan a QR code or enter the key, confirm a live code, then save one-time recovery codes), and a new Settings to Security screen manages factors, recovery codes, and trusted devices. At login, a second step asks for the code or passkey; "remember this device" can skip it for 30 days, and every trusted device is listed and revocable. This matters because the agent intentionally bypasses 2FA on the WordPress sites it manages (for one-click login), so the dashboard is the single front door to every site and is now hardened accordingly. Two-factor is optional per user; superadmins see a reminder to enable it.

### Security

- Second factors are built on the standard primitives (RFC 6238 TOTP and WebAuthn). The TOTP secret is encrypted at rest, recovery codes are hashed and single-use, used codes are burned to prevent replay, and a cloned authenticator is detected and rejected. Verification attempts are rate-limited and locked out across attempts. A two-factor account cannot obtain a session on any login path (password, SSO, email verification) without completing the second step, changing or resetting the password revokes trusted devices, and disabling a factor or regenerating codes requires re-entering the password. All two-factor events are written to the audit log.

Control plane plus dashboard at 0.50.0; one migration (auto-applied on boot); no agent change. Passkeys require accessing the dashboard on its primary domain; the authenticator-app factor works everywhere.

## [0.49.2] - 2026-06-16

### Changed

- **Sites grid card redesign.** The grid card was rebuilt for clarity and consistency. The unlabeled icon row is now a labeled "Site configuration" group (Page Cache, Object Cache, HTTPS, Backups, Multisite), each with a text label and an on/off state shown by a filled-versus-hollow dot, not color alone. All metadata is now a labeled key/value list (Versions, Host, Client, Tags, Screenshot) so no value is bare. Every section reserves its height with a calm empty state, so cards line up row-for-row regardless of which optional data a site has. The screenshot freshness moved off the image (no more caption overlapping the thumbnail) into a labeled footer line, and the card action buttons carry clear labels.

Dashboard only at 0.49.2; no control-plane, migration, or agent change.

## [0.49.1] - 2026-06-16

### Fixed

- **Site screenshots now appear in the grid.** The enricher that adds the presigned image URL to each site in the list response was wired onto a different repository instance than the one serving the Sites list, so list enrichment never ran and every card fell back to the favicon placeholder even when a ready screenshot already existed in storage. The enricher is now wired onto the list service itself, with a regression test that fails if it is ever attached to the wrong instance.
- **Screenshot capture stopped failing with a tunnel error.** The in-process SSRF proxy that headless Chromium connects through rejected the browser's `CONNECT` requests because the request multiplexer did not accept authority-form targets, so every capture failed before reaching a site. The proxy now handles `CONNECT` directly and dials over IPv4 (Cloud Run has no IPv6 egress), covered by a new end-to-end tunnel test.
- **The Sites grid refreshes itself after a capture.** After a screenshot is requested, the dashboard polls the list until the capture finishes (or times out), so the card moves from "capturing" to the finished image without a manual reload.

Control plane, media worker, and dashboard at 0.49.1; no migration, no agent change.

## [0.49.0] - 2026-06-16

### Added

- **Sites grid view with website screenshots.** The Sites dashboard now has a list/grid toggle. The grid shows each site as a rich card led by a real screenshot of the site, captured server-side and refreshed on connect, weekly, and on demand. Each card also shows connection state, a capability strip (page cache, object cache, HTTPS, backups, multisite), pending updates, backup health, SSL expiry, uptime and latency, WordPress, PHP, and agent versions, host, client, and tags, with comfortable and compact densities. The screenshot degrades to the site favicon or a monogram until a capture lands.
- **Uptime percentage, latency, and SSL expiry on the Sites list.** These are now returned with each site (joined from the uptime monitor) so the grid card and other surfaces can show them without a per-site request.

### Fixed

- **Sites filters now work.** The Status and Tags filters were inert (they logged but did not filter). They are now real multi-select filters that compose with search, client, and the archived toggle, with an applied-count badge and a clear-all control, and all filters plus the chosen view persist in the URL so a filtered grid can be shared or reloaded.

### Security

- Screenshot capture runs headless Chromium behind an in-process SSRF guard that re-validates every connection (navigation, redirect, and sub-resource) at dial time, rejecting private, link-local, loopback, and cloud-metadata addresses. QUIC, HTTP/3, and non-proxied WebRTC are disabled so no connection can escape the guard over UDP. Captures run unprivileged with bounded memory and time, the screenshot table is tenant-isolated with a restrictive row policy, and only control-plane-signed image URLs are served (never the raw site URL).

Control plane, media worker, and dashboard at 0.49.0; one migration (auto-applied on boot); no agent change.

## [0.48.3] - 2026-06-15

### Added

- **Activity log integrity report.** The "Chain break at seq N" badge is now a button that opens a report explaining why the tamper-evident audit chain failed to verify. The control plane classifies the break into one of four causes (missing events, a broken link between two entries, modified content, or a missing chain start) and the report states it in plain language, names the events involved, and shows the technical hash detail on demand. A chain break most often means older entries were pruned or cleared rather than tampering, and the report says so honestly instead of only flagging a number. A "Re-check" action re-runs verification.

### Changed

- **The `GET /activity/verify` response now includes a `break` object** when a chain break is found: the failing sequence, the cause classification, the prior verified sequence, the size of any sequence gap, the expected-versus-stored hashes, and the offending event. The existing `break_at_seq` field is unchanged.

Control plane plus dashboard at 0.48.3; no migration, no agent change.

## [0.48.2] - 2026-06-15

### Fixed

- **One-click login no longer triggers a second 2FA challenge.** On sites running Solid Security (and the official Two Factor plugin), one-click login landed on the plugin's own 2FA interstitial instead of the dashboard. The agent's autologin was firing WordPress's `wp_login` action, which is the sole trigger those plugins use to arm a post-login challenge. The autologin path now establishes the session without firing `wp_login`, so it lands straight in wp-admin. The signed single-use token plus the control-plane role allow-list remain the authorization gate (a stronger proof of operator intent than an interactive challenge). The session two-factor markers are still set as a convenience so the operator can edit the 2FA settings screen without re-verifying.

### Added

- **Operator one-click logins are recorded in the activity log.** Because the autologin no longer fires `wp_login`, it is logged from a dedicated success signal instead, tagged as a one-click login so it stays in the audit trail.
- **SecuPress sites are refused with a clear message.** SecuPress replaces the login flow with its own passwordless/magic-link scheme that distrusts externally-set sessions, so one-click login cannot work there. The agent now declines with an operator-readable error ("sign in normally") instead of looping, and does not consume the single-use token.

Agent 0.48.2. No control-plane or migration change.

## [0.48.1] - 2026-06-15

### Fixed

- **A locked backup can no longer be deleted.** Locking a snapshot already exempted it from retention pruning, but a manual delete still removed it, so the lock only protected against the auto-pruner. The delete path now refuses a locked snapshot ("this backup is locked; unlock it before deleting") the same way it already refuses an in-progress or chain-depended-on one. In the dashboard, deleting a locked backup opens a short explanation with an "Unlock to delete" action instead of failing at a server error, so a lock now genuinely protects the backup end to end.

Control plane plus dashboard at 0.48.1; no migration, no agent change.

## [0.48.0] - 2026-06-15

### Added

- **Fleet email and deliverability dashboard.** The Email view is rebuilt on the same operator-grade language as the other fleet dashboards. Sent, failed, bounced, and complained totals are filter tiles, with fleet bounce rate and complaint rate shown against the limits providers enforce (bounce at 5 percent, complaints at 0.1 percent) so a site harming sender reputation stands out. A per-site deliverability table lists every site with its provider, volume, bounce and complaint rates (color coded by threshold), last send, and a send-volume sparkline, sorted riskiest first, each row drilling into that site. A deliverability trend draws the danger thresholds on. The cross-site email log, suppression list, sandboxed message preview, and notification settings are kept, and a site selector switches the whole page between the fleet view and a single site, all live over SSE.

### Changed

- **Fleet email stats now include bounced and complained counts** in the summary and the daily series, and a new per-site deliverability endpoint backs the table. Tenant-scoped and org-level.

Control plane plus dashboard at 0.48.0; no migration, no agent change.

## [0.47.2] - 2026-06-15

### Fixed

- **Fleet table columns now align.** The shared fleet table was rebuilt as a single sticky table with a column group driving column widths, so the header and the rows share one geometry and cannot drift apart (the previous virtualized header could desync from the body). Affects the uptime, backup, and performance tables.

### Changed

- **Performance dashboard brings back per-site Core Web Vitals behind a site filter.** A site selector in the header switches scope: "All sites" shows the fleet aggregate, and picking a site shows that site's full per-site detail (LCP, INP, CLS, FCP, TTFB p75 with distribution bars, the 28-day trend, and the per-URL breakdown). The fleet Core Web Vitals table now lists every reporting site sorted by LCP and each row drills into that site. The selected site, device, and window are kept in the URL.

Dashboard only; no control plane, migration, or agent change.

## [0.47.1] - 2026-06-15

### Fixed

- **Performance dashboard no longer errors out.** The fleet Core Web Vitals endpoint now returns the per-metric object and the daily trend the dashboard expects, so the page renders instead of showing "Something went wrong".
- **Fleet uptime shows site names and latency again.** The fleet status endpoint field names now match the dashboard, so the Site column is populated and average latency reads correctly instead of "NaN ms".
- **Fleet table columns line up.** The shared fleet table now pins column widths with a column group, so the sticky header and the rows align across the uptime, backup, and performance tables.

Added JSON-shape contract tests for both fleet endpoints and defensive guards in the dashboards so a future field rename fails the build rather than reaching the browser. Control plane plus dashboard at 0.47.1; no migration, no agent change.

## [0.47.0] - 2026-06-15

### Added

- **Fleet uptime and status overview.** A new cross-site view: up / degraded / down summary tiles that filter the page, a dense status matrix (one cell per site, grouped) for spotting the one red site in a sea of green, a virtualized per-site table with a 90-day uptime strip and a response-time sparkline per row, unified agent connection state plus probe state, and a cross-site incident feed.
- **Fleet backup browser.** A new cross-site view centered on backup health: Protected / Stale / Failed / Unprotected tiles, a virtualized one-row-per-site table led by the age of the last good backup (color-coded), with next scheduled run, latest size, a size-trend sparkline, run-backup, browse-snapshots drill-in, and per-snapshot restore. Full-archive download is planned as a follow-up.

### Changed

- **Performance dashboard redesigned as a true fleet view.** The single-site picker is gone. The page now leads with fleet headline figures, a sortable worst-offenders table with an inline Core Web Vitals distribution bar and p75 sparkline per site, a 28-day fleet CWV trend with threshold lines, and the database-health rollup folded in as one section. Device and window are shareable URL parameters.

### Notes

- New tenant-scoped read endpoints: fleet backup list and backup health, fleet uptime status and incidents, and a fleet RUM aggregate. All are site-scope aware (a collaborator sees only their granted sites) and fail closed. Control plane plus dashboard at 0.47.0; no migration, no agent change.

## [0.46.0] - 2026-06-15

### Changed

- **Local backups are stored under the uploads directory.** The local backup destination now writes to uploads/wpmgr-backups (falling back to wp-content only when uploads is not writable), with a deny-all .htaccess and an index.php guard so archives are never directly downloadable, plus a best-effort migration of any existing local backups. Snapshots and the media quarantine already used the uploads-based location.
- **Database queries hardened with prepared placeholders.** The object-cache drop-in installer's transient cleanup and the media URL rewriter's postmeta lookup now bind their values through $wpdb->prepare().

### Added

- **External services fully documented.** The readme now lists every outbound service the agent's own code can contact, including the Amazon SES, SendGrid, Mailgun, and Postmark email providers, each with what is sent, when, and links to its terms and privacy policy.

### Fixed

- **WordPress.org distribution packaging.** The directory build no longer ships vendor CLI entrypoints, vendor license files, or hidden dotfiles; a .distignore is included for source-level archive tooling.

This is an agent-focused compliance release; the control plane and dashboard images are rebuilt at the same version with no functional change.

## [0.45.0] - 2026-06-13

### Added

- **Agent: page-cache drop-in now nudges WP-Cron on cache hits.** On a cache hit the drop-in stats the cron marker file; if it is more than 60 seconds stale, the cached page is flushed to the visitor first, then a fire-and-forget loopback GET to `wp-cron.php` fires with a 1-second timeout. The decision is a single filesystem stat with no database work, keeping the cache-hit fast path intact. Same-host only; the drop-in self-heals to this version on next boot.
- **Control plane: low-frequency cron-kick pass.** A separate sweep GETs `wp-cron.php` on every connected site at a configurable interval (default every 5 minutes, tunable via environment). It records no metrics and never changes connection or health state, so uptime and latency numbers are unaffected. Reuses the existing SSRF-hardened HTTP client.

Control plane, web, and agent 0.45.0; no migration. Builds on the active reachability verification shipped in 0.44.0: connected idle sites stay connected (0.44.0) and their scheduled work actually runs (0.45.0).

## [0.44.0] - 2026-06-12

### Fixed

- **Healthy idle sites on a page cache no longer show as disconnected (critical).** Agent heartbeats ride WP-Cron, which only runs when PHP boots. On a fully page-cached low-traffic site the web server serves every request from disk, WordPress never boots, and a healthy site showed as disconnected. The connection sweeper now dials each quiet site directly with a signed ping command before it degrades or disconnects the site, so dashboard liveness is no longer traffic-dependent. A captive portal or other generic 200 response is never counted as alive. Sites confirmed unreachable after the dial disconnect with the new reason "agent_unreachable", distinguishing them from sites that are simply idle.

### Added

- **Active reachability verification in the connection sweeper.** The sweeper dials each quiet site with a signed ping command (falls back to the metadata command for older agents) and treats a shape-verified 200 as a heartbeat, keeping the site connected. The dial also wakes WP-Cron so overdue scheduled work drains. Bounded: 8s per-dial timeout, 8 concurrent dials, 12s wall budget per sweep tick. Three environment knobs: `WPMGR_SWEEP_ACTIVE_VERIFY` (default on), `WPMGR_SWEEP_VERIFY_TIMEOUT`, `WPMGR_SWEEP_VERIFY_CONCURRENCY`.
- **Agent: signed ping command.** A cheap liveness answer that spawns WP-Cron so overdue scheduled work drains on every verify dial.
- **Agent: shutdown catch-up heartbeat.** Fires when WordPress boots and the last heartbeat is more than two minutes overdue. Stampede-locked, 5s timeout so it never holds a worker.
- **Dashboard: accurate connection badge copy.** "Agent unreachable" when the control plane dialed and got no answer; "No heartbeat" when the agent is quiet but the site may just be idle. The degraded tooltip explains verification is in progress.
- **Dashboard: Health-tab cron callout.** A dismissible callout recommends disabling WP-Cron and adding a real server cron entry when diagnostics show WP-Cron starvation on a cached site.

Control plane, web, and agent 0.44.0; no migration.

## [0.43.3] - 2026-06-12

### Fixed

- **Database cleanup progress now survives missed events and page refreshes.** Cleanup results are stored server side (migration m71) and a new endpoint reports the active job and the last result, so the page restores state on load and after a stream reconnect. A running cleanup shows correctly after a refresh, and the completion event is published before the watchdog clears so failures still surface. The late frame guard fix applied to scans in 0.43.2 now covers cleanups as well.
- **Font processing banner can no longer stick.** The banner reconciles against stored per-font statuses on page load and on stream reconnect, clearing itself when the server shows no conversion in flight.

Control plane and web 0.43.3; migration m71 applies automatically on boot; no agent changes. This completes the live update hardening started in 0.43.2: every dashboard surface now recovers from missed events.

## [0.43.2] - 2026-06-12

### Fixed

- **Live updates silently stopped for tenants with a connected object cache (critical).** The event stream protocol requires time-sortable event identifiers, but object cache events minted a different identifier format that sorts after every normal identifier. One delivered object cache event advanced the stream cursor past all future events, so database scan results, backup progress, and connection changes stopped arriving; reconnecting made it permanent because the browser echoed the poisoned cursor. The publisher now enforces the correct identifier format for every event regardless of caller, the stream treats an invalid cursor as a fresh start so affected browser tabs self-heal, and the white-label report event had the same defect fixed.
- **Database scan results now return directly in the scan response** instead of relying only on a live event, and the page loads stored results on mount and after any stream reconnect. A running scan survives a page refresh, a scan stuck without updates resets after three minutes, and a result arriving after a missed start event is no longer discarded.
- **Live update hardening across the dashboard:** all performance surfaces backed by server queries now refresh automatically when the event stream reconnects, closing the missed event gap for the object cache pill, cache statistics, font results, and real user monitoring summaries.
- **Scan bookkeeping failures are now logged** instead of silently discarded, and the completion event is published before the watchdog clears so a failed publish still triggers the failure rescue.

Control plane and web 0.43.2; no agent changes.

## [0.43.1] - 2026-06-12

### Fixed

- **Object cache: configuration files written by privileged command-line processes are now readable by the web server.** When WP-CLI provisioning ran as root, the 0600 configuration file was unreadable by the web server user. Web requests silently served the cache in array mode while command-line checks reported connected. The configuration and cool-down state writers now align file ownership with the owner of the WordPress core entry file. Caught by the new end-to-end harness.
- **Object cache: a Redis write failure during PHP shutdown no longer arms a recovery flush.** A failure while the process is already shutting down is not an outage; previously it could persist an outage marker and schedule an unnecessary cache flush at the next boot.
- **Object cache: the recovery flush deletes its marker before flushing**, closing a window where the flush removed its own coordination lock and a second request could flush again.
- **Object cache: the cool-down side channel uses APCu only when it is actually enabled** in the running interpreter, fixing silent cool-down loss in command-line contexts.

### Added

- **The end-to-end Docker harness ran green for the first time across all eighteen stages**, including cross-request persistence, the boot recursion guard with descriptor counting, codec fallback against a runtime without igbinary, and the live debug header on a real front-end response. Stage fixes along the way: provisioning runs as the web user, config changes go through the engine writer so opcache is invalidated, and the teardown order respects the plugin autoloader.

Agent 0.43.1 with drop-in 2.2.1; no control plane changes beyond 0.43.0.

## [0.43.0] - 2026-06-12

### Added

- **Object cache: live debug response header.** With the new "Debug response header" setting enabled, front end responses carry an `x-wpmgr-object-cache` header showing the live cache state plus per request hit, miss, read, and write counts and the total Redis wait time. Administrators always receive the header on front end pages while logged in, so the cache can be verified without enabling it for visitors. Pages served by the page cache do not carry the header because WordPress does not run on those responses. The header never includes connection details, key names, or version numbers.
- **Cross system configuration hash contract.** The agent and the control plane now share a pinned test fixture proving both compute identical configuration hashes, including values containing slashes and special characters where the two JSON encoders previously diverged. This removes a false drift warning for sites connecting to Redis over a unix socket.
- **End to end stages for the debug header** covering header presence, the disabled default, and the page cache interaction.

### Changed

- The cool down state file path override used by tests is now inert unless a test only constant is defined.
- Control plane and dashboard: the new setting is available in the object cache configuration dialog (migration m70 applies automatically on boot).

Control plane, web, and agent 0.43.0 with drop-in 2.2.0; existing installs refresh the drop-in automatically after the agent updates.

## [0.42.2] - 2026-06-12

### Fixed

- **Object cache: dashboard status froze once the cache went live (stats reports rejected).** The agent reports cache operations per second as a JSON number with decimals; the control plane typed the field as an integer and rejected the entire stats report, so the dashboard kept showing the last state from before the cache was enabled. Idle sites passed (whole numbers encode without decimals), which is how it escaped testing. The control plane now accepts the decimal value, and the agent reports a whole number for compatibility with older control planes.
- **Object cache: a malformed status block can no longer reject the whole stats report.** The block is now decoded separately and skipped with a logged warning, so page cache stats always land even if the object cache block is unparseable. This applies the same tolerant ingest approach used in 0.35.3.

Control plane and agent 0.42.2; the drop-in stays at 2.1.1 (no site-side cache changes). Sites already running agent 0.42.1 are fixed by the control plane update alone.

## [0.42.1] - 2026-06-12

### Fixed

- **Object cache: recursive boot caused file descriptor exhaustion and fatal errors on affected sites (drop-in 2.1.1).** Drop-in 2.1.0 ran its failback safety check before the cache global was assigned. That check called into the WordPress options API, which re-entered the boot path before the global existed, opening a new persistent Redis socket at each recursion level. On affected sites the result was a fatal error on every request and stuck worker processes that required a web server restart even after the drop-in was removed. The boot-time failback check now runs only after the cache global is assigned, and a re-entry guard returns a safe in-memory fallback for any cache call that arrives while boot is still in progress.
- **Object cache: sockets leaked on failed or aborted connections.** Connections that failed or were abandoned during failback now close explicitly; boot falling back to array mode closes the connection it did not finish.
- **Object cache: unsupported serializer or compression codec no longer aborts the connection.** When the server cannot honor the configured serializer or compression codec, the engine falls back to the PHP serializer or no compression, reports the effective codec to the dashboard, and the integrity check validates against effective values so stored data is never deserialized with the wrong codec.
- **Object cache: AUTH and SELECT results are now verified.** A half-established connection can no longer be reported as connected.
- **Object cache: a per-request connection attempt budget (12) converts any future connection loop into a single degraded request** instead of a site outage.
- **Object cache: a persisted reconnect cool-down (15 seconds, doubling to 5 minutes) stops a down Redis from being re-dialed on every request.** The dashboard shows the cool-down state.
- **Object cache: connection retry settings are now bounded** at both the agent and the control plane (retry count 0 to 10, retry interval 1 to 5000 ms).

### Added

- **Object cache: new regression coverage.** An artifact-level boot test fails on any recursive boot. End-to-end harness stages cover descriptor counting and codec fallback.

### Changed

- **Control plane:** object cache config saves now validate retry count and retry interval bounds before persisting.
- **Web:** readable labels for the reconnect cool-down and connection attempt limit degradation causes; previously raw cause strings are now human-readable throughout the cache status surface.

Agent 0.42.1 with drop-in 2.1.1. If a site was affected by the 2.1.0 boot loop: delete wp-content/object-cache.php, restart PHP or the container to release leaked descriptors, update the agent, then re-enable the object cache. Existing installs that were not affected refresh automatically after the agent updates.

## [0.42.0] - 2026-06-12

### Fixed

- **Object cache: full behavioral parity audit against the category-leading implementation, with every accepted fix shipping alongside the test that proves it.** Headline corrections: the in-request cache layer is now keyed identically to Redis, eliminating a multisite scenario where switching blogs could serve one site's cached values as another's; counter operations on missing keys now return false exactly as WordPress core does instead of fabricating values; serializer and compression settings the server cannot honor now fail loudly into safe mode with a named cause instead of silently mixing storage formats; the post-outage cache flush is rebuilt around a persisted outage marker and a Redis lock so exactly one request flushes after a genuine recovery and never during normal traffic; and install-mode detection no longer suppresses cache writes during WordPress upgrades.
- **Sixteen further contract corrections** covering delete-on-missing return values, force-refresh reads on memory-only groups, write-through ordering, key validation, batched-read result ordering, back-compat property access, version-aware flush flags, multisite transient cleanup, and a guard against a performance plugin disabling our drop-in.

### Added

- **Configuration drift detection.** The agent now reports the fingerprint of the configuration file it is actually reading, and the dashboard flags when it diverges from the saved settings, ending the class of silent mismatch between what the control plane believes and what the site runs. Failed configuration pushes to the site now surface as a visible warning instead of being discarded.
- **Codec capability gate.** Saving a configuration that requests a serializer or compression codec the site's own connection test reported as unavailable is now rejected up front with a clear message.
- **Named diagnosis for unreadable credentials files and honest cache-flush results in command-line contexts**, plus complete teardown on deactivation and uninstall.
- **Four new integration-harness stages**: multisite isolation, install-mode writes, file-ownership drift from command-line sessions, and outage-recovery flushing exactly once.

Migration m69 applies automatically on API boot. Agent 0.42.0 with drop-in 2.1.0; existing installs refresh automatically after the agent updates. Security reviewed (verdict ship, no findings).

## [0.41.6] - 2026-06-11

### Fixed

- **Object cache: the cache no longer flushes itself on every request.** The recovery mechanism that clears potentially stale keys after a Redis outage misread its per-request state and treated the first successful operation of every page load as an outage recovery, wiping the entire site keyspace each request. With the cache enabled this made wp-admin dramatically slower than no cache at all: every read missed, every option re-queried the database, and all transients died per request. The flush now fires only after a genuinely recorded outage-to-recovery transition, with regression tests asserting no flush ever happens without a prior failure.
- **Object cache: non-activation diagnosis is accurate and names the culprit.** The previous cause detection used a leftover substring check that misread the current drop-in and made four causes unreachable. The rewritten diagnosis distinguishes a replaced cache object (reporting the replacing class and file), an incomplete boot, a stale opcode cache, a suppression filter, an early definer (reporting its file), and missing, outdated, or foreign drop-ins, in the correct precedence order.

### Added

- **A real-WordPress integration harness** (docker compose: WordPress, MariaDB, Redis) that installs the built agent zip and asserts what unit tests structurally cannot: the engine actually serving as the active cache, keys surviving across requests (the direct regression net for the per-request flush bug), loose-typed plugin call shapes against the installed drop-in, heartbeat correctness in web and cron contexts, and a negative test for early cache definition. Runs nightly and on demand; not part of the default CI gate.

Agent-only release. Drop-in 2.0.2; existing installs refresh automatically after the agent updates.

## [0.41.5] - 2026-06-11

### Fixed

- **Object cache: a loose-typed cache call can no longer take the site down.** The 0.41.4 self-contained drop-in activated correctly but enforced strict parameter types on the WordPress cache API surface; the first plugin call passing an integer group name (a pattern WordPress core tolerates by casting) became a fatal error on every request. All public cache methods now accept what core accepts and normalize internally, the generated drop-in no longer carries a strict-types declaration, and every cache wrapper catches unexpected errors and degrades to a cache miss instead of crashing the request. The exact call shape that caused the outage is now a permanent regression test that runs against the generated drop-in itself.

Agent-only release. Drop-in version 2.0.1. If your site was affected: delete wp-content/object-cache.php to recover, update the agent, then enable the object cache again.

## [0.41.4] - 2026-06-11

### Changed

- **Object cache: the drop-in is now fully self-contained.** Instead of a small locator file that finds the engine inside the plugin directory at runtime, the installer now ships one generated file containing the complete engine, connection layer, and config loader. The file is produced at build time, byte-identical per release, and has zero runtime dependence on the plugin folder name or location, which removes the entire class of "drop-in present but engine never active" failures the locator design allowed. The encrypted credentials file stays separate and 0600.

### Added

- **Object cache: non-activation now names its cause.** When the drop-in is installed but the engine is not the active cache, the heartbeat reports a specific reason instead of a generic flag: a stale opcode cache, an early cache definition by another component, a suppression filter, an outdated or foreign drop-in, a missing file, or an explicit kill-switch or install-mode bail. The heartbeat also reports the site PHP version and SAPI, and opcache invalidation results are verified and reported rather than silently suppressed.

Agent-only release. Drop-in version 2.0.0; existing installs refresh automatically on the next heartbeat after the agent updates.

## [0.41.3] - 2026-06-11

### Changed

- **Object cache: the status heartbeat now reads the live engine, not a persisted option.** The dashboard pill previously depended on a fragile chain (an analytics-gated shutdown write into a WordPress option, read back by a later request) where several links could silently fail and present as "Disabled". The reporting request has the drop-in active too, so the heartbeat now asks the running cache object for its state directly; the persisted option only carries the analytics counters. The heartbeat also reports the engine's own version on the wire, so "which code is actually executing on this site" is always visible.

### Fixed

- **Object cache: agent updates can no longer leave stale engine bytecode running.** On hosts with aggressive opcode caching, replacing the plugin files did not guarantee the new engine code executed. The agent now invalidates the engine and its supporting files on every version change at boot, and the drop-in installer invalidates them on every install.
- **Object cache: the drop-in self-heal actually fires.** The installed-stub version check read only the first 512 bytes of the file while the version header sat past byte 1100, so outdated stubs were always misread as current. The header now sits at the top of the file and the check reads further regardless.
- **Object cache: array mode always records a named reason** (such as a missing config or unloadable classes), the state snapshot persists regardless of the analytics toggle, and a connection-retry path no longer calls a WordPress function that may not exist at drop-in load time. A single invalid number can no longer silently drop an entire stats report.

Agent-only release.

## [0.41.2] - 2026-06-11

### Fixed

- **Object cache: the engine's supporting classes now load at drop-in time.** The 0.41.1 drop-in located the engine correctly, but the engine file then loaded its config and connection classes through a plugin constant that does not exist that early in the WordPress boot, so it silently fell back to the in-memory array cache on every request and kept reporting itself idle. The engine now resolves its sibling class files from its own directory, which is always available. Agent-only fix.
- **Object cache: the stamped engine path in the drop-in is honored.** The installer's placeholder replacement also rewrote the guard that detects an un-stamped stub, turning the stamped path into dead code; standard installs survived only via the content-directory fallback. The guard token is now built so stamping cannot touch it, and the drop-in version bump makes existing installs self-heal on the next agent heartbeat.

Requires agent 0.41.2. No control plane or dashboard changes.

## [0.41.1] - 2026-06-11

### Fixed

- **Object cache: the engine now actually starts on real sites.** The object-cache.php drop-in installed by 0.41.0 located the engine through constants that WordPress does not define yet at the moment drop-ins load, so the engine silently never booted: the status pill stayed "Disabled", analytics stayed empty, and Redis never received a key even though Enable reported success. The installer now stamps the resolved engine path directly into the drop-in at install time, with a content-directory fallback, and the agent automatically refreshes an outdated drop-in on its next heartbeat, so existing installs self-heal after updating the agent. No manual disable and re-enable needed.
- **Object cache: flush no longer fails with a 422.** Five Redis SCAN call sites (the flush and disable commands, the connection test's capability probe, and two engine flush paths) called SCAN with the wrong client API shape, which threw on every invocation. The connection test also misreported this as an ACL denial. All five now use the correct phpredis iterator pattern, pinned by a signature-enforcing test double.
- **Object cache: the saved connection test result now survives reloads.** The config response never included the stored test result, and saving any unrelated setting wiped it. The Server capabilities card now renders from the stored result, which is preserved across saves and intentionally discarded only when connection fields change.
- **Object cache: analytics can now populate.** The agent heartbeat previously never included hit and miss counts, so the charts could never receive data. The engine now accumulates per-request counters and the heartbeat reports them as consume-and-reset deltas alongside average latency and operations per second.
- **Object cache: honest status reporting end to end.** The dashboard pill now distinguishes "configured but not serving" (a real reported state) from never configured; an unrecognised state from an agent no longer blanks the stored state; agent command failures now surface as error toasts instead of success; and command failures carry the exception class name (never the message, which could contain connection details) for diagnosability. Swallowed ingest and command errors are now logged with bounded, length-capped detail strings.

Requires agent 0.41.1 for the on-site fixes; the dashboard fixes apply on the API and web update alone. Security reviewed (verdict ship; the two log-hygiene notes were fixed before release).

## [0.41.0] - 2026-06-11

### Added

- **Per-site Redis object cache for agency operators.** The performance suite gains a persistent object cache that accelerates the dynamic, uncacheable side of WordPress: logged-in users, admin screens, carts and checkout, REST API responses, and every database round-trip the page cache cannot serve. Configure a connection per site from the Cache tab: TCP host and port, unix socket, database number, ACL username and password, TLS, and a key prefix that scopes all of that site's keys on a shared Redis instance. A "Test connection" flow runs before the cache can be enabled: the agent dials the candidate config without persisting it, probes phpredis version and extension capabilities (igbinary serializer, lzf/lz4/zstd compression, TLS support), reads the eviction policy with guidance (allkeys-lru recommended; noeviction surfaces a warning chip), and returns a structured result. Enable is blocked until a test passes for the current config. The credential is encrypted by the control plane (age, X25519) and written to a 0600 private PHP file on the site after delivery over the signed command channel. The plaintext never appears in GET responses, logs, SSE payloads, test results, heartbeats, or backups. The cache degrades safely at two levels: a boot failure swaps in a pure in-memory array cache so the site never goes down, and mid-request Redis errors become misses with one reconnect attempt, then degrade for the rest of that request. Full WordPress cache API surface: add/get/set/replace/delete, multi-key variants, flush_group, flush_runtime, wp_cache_supports, and switch_to_blog for multisite. A live status indicator in the dashboard (connected, degraded, down) streams over SSE with a 10-second debounce, updated every heartbeat. Charts track hit ratio, used memory, average command latency, and operations per second over the last 7 days with a 90-day daily downsample. A flush control scopes to only the site's own prefixed keys on a shared Redis so a flush never touches another site's data. Connections use phpredis persistent sockets with an explicit identity to prevent the classic pooled-socket database-confusion bug, finite connect and read timeouts (1.0s defaults), and decorrelated-jitter connect retries with AUTH and SELECT inside the retry loop. v1 topology: single instance or unix socket with TLS. Sentinel and Cluster come in a later release; the config schema reserves fields for both. Migration m68. Requires agent 0.41.0. Security reviewed adversarially: one blocking finding and several hardening items were fixed before release; credentials never enter backups and group scan aggregates return counts only, never cached values or raw keys.

## [0.40.0] - 2026-06-11

### Added

- **The client portal overview is now a real dashboard.** Instead of a thin header and a plain sites list, portal users land on a live summary of everything their agency does for them: a status banner ("All sites operating normally" or "N sites need attention"), five headline numbers with animated counters (sites monitored, average uptime, backups, updates applied, site speed rating), a month-at-a-glance section with the fleet uptime trend and a Core Web Vitals distribution band, a callout for the latest white-label report with HTML and PDF downloads, richer site cards (brand-colored avatar, 30-day uptime sparkline, speed rating chip, TLS expiry, last backup, per-period backup and update counts), and a day-grouped "Recent work" timeline showing each update and backup the agency performed. A period switcher covers the last 7, 30, or 90 days. The data comes from one new read-only summary endpoint that reuses the report aggregator; everything is strictly scoped to the client's own sites, and agency-internal details (email logs, error logs, raw metrics) are never exposed. Security reviewed (verdict ship, no findings to fix).

### Fixed

- **Client portal invitations never sent the email.** The invitation email template existed and the send was wired, but the template was missing from the mailer's subject registry, so every send failed silently while the screen claimed the invitation was emailed. Invitations now send when instance email is configured, and the confirmation is honest either way: "Invitation emailed to {address}" only when it actually went out, otherwise a clear prompt to share the copyable invite link. A new completeness test prevents any future template from shipping without its subject registration.

## [0.39.1] - 2026-06-11

### Fixed

- **The WooCommerce cart-aware caching toggle could never be enabled, on any site or theme.** The agent's theme support detection ran only inside scheduled background jobs and remote command handlers, two contexts where WooCommerce never loads its storefront scripts, so every check reported "unsupported" and re-stamped that result on every heartbeat. Detection now runs during real storefront page renders: any positive detection enables the toggle immediately, a negative verdict requires three different pages to agree (cart fragments often load only on cart pages), the check repeats after theme or plugin changes, and until a real check has happened the dashboard now says "Checking your theme" instead of pretending the theme is unsupported. Existing stored verdicts were reset since none were trustworthy. Requires agent 0.39.1; migration m67.
- **Enabling the CDN failed with "cdn_url is required" before you could type a URL.** The CDN switch saved immediately on flip, but the URL field only appears after the switch is on, so the save was always rejected and the switch snapped back, hiding the field again. Flipping the switch on now reveals and focuses the URL field without saving; the setting saves in one step once a valid URL is entered, and validation problems show inline on the field instead of a generic error message.

## [0.39.0] - 2026-06-11

### Added

- **Read-only client portal: give each client their own branded login and dashboard.** From a client's detail page, open the new "Portal access" tab to invite client users by email. Existing users are added immediately; new email addresses receive a tokenized invite link with a 7-day expiry. The invite accept link is always shown as a copyable fallback so the flow works even when instance email is not configured. Revoke any member instantly, revoke or regenerate a pending invite, and all of this is also available to the agency when the invite is regenerated (the link rotates and the old one stops working). Clients sign in on the same login page and land automatically at `/portal` after authentication, with no agency screens visible and no way to navigate to them. The portal shell shows the client's logo, brand color applied as a scoped accent, and an agency attribution footer ("Managed by {agency}"). Two-item navigation: Sites and Reports. No sidebar, no org switcher, no write controls anywhere in the portal tree. The sites overview lists each client site with its last backup date, 30-day uptime percentage, and TLS expiry. Site status wording is softened for client audiences: "Monitoring active" instead of connected, and "Needs attention" instead of degraded or disconnected. Each site links to a detail page with four read-only cards: uptime summary and incident history (24-hour, 7-day, 30-day, and 90-day ranges), backup inventory (completed backups only, no restore or download controls, no destination or blob keys), applied updates log, and Core Web Vitals p75 field data with per-metric ratings. The Reports page lists all completed white-label reports for the client and provides HTML and PDF download links. Portal users hold a new `client` role ranked below viewer with zero permissions. They can see only their own client's sites and reports, cannot access any agency endpoint or event stream, and lose access the moment they are removed, when the client is archived, or when the client is deleted. Migration m66. Security reviewed in two rounds including live row-level-security isolation tests.

### Fixed

- **Deleting a client that still had sites assigned failed with a database error since 0.37.0.** The composite foreign key on the clients-to-sites relationship nulled the wrong columns on delete, causing a constraint violation instead of cleanly unassigning the sites. Sites are now correctly unassigned when a client is deleted, matching the documented behavior.

## [0.38.1] - 2026-06-11

### Fixed

- **On-demand reports were stuck in pending forever.** The report job started but every status transition failed because the `generated_reports` table was missing its `updated_at` column (the m64 migration omitted it while all report mutations write it; the query compiler does not validate UPDATE SET column names, so it only surfaced at runtime). Migration m65 adds the column; stuck reports recover automatically on the job's next retry.
- **Client rows were not clickable.** The Clients page listed clients with only Edit and Delete actions and no way to open a client's detail page (sites + reports). The client name is now a link, and the Client badge on the sites table also links to the client.
- **Completed reports showed "Storage not configured" instead of download links.** The report list endpoint never minted presigned download URLs (only the per-report detail endpoint did), so the dashboard's report table had no HTML or PDF links to render even when object storage was configured and the artifacts were stored. The list endpoint now presigns URLs for every completed report (a local signing operation, no storage round trip).

## [0.38.0] - 2026-06-11

### Added

- **White-label client reports (scheduled and on-demand).** Every client record now has a Reports tab. Enable a monthly (default) or weekly schedule per client, choose the send day and hour in the client's own timezone (a new per-client timezone field, defaulted from the agency), and recipients default to the client contact email. A "Generate now" button builds a report for any period from presets or a custom range of up to 92 days. The report aggregates data WPMgr already tracks: uptime and response time, backups completed, updates applied, Core Web Vitals real-user p75, and email deliverability. Each section has an on/off toggle; a custom intro and closing text block can be added to any report. Reports are delivered as a branded HTML email digest, a print-optimized page, and a downloadable PDF rendered server-side with vector charts and full Unicode support (no headless browser). The client's brand color and logo appear on every output; the "powered by" footer can be removed free of charge on any plan. Delivery uses the instance mailer; the schedule card shows a warning when instance email is not configured, but reports still generate and download regardless. Reports and download links are tenant-isolated; logo URLs are SSRF-guarded; report periods are bounded at 92 days; security-reviewed (two rounds, green verdict). Migration m64.

## [0.37.0] - 2026-06-11

### Added

- **Clients (Foundation): group managed sites under named client records.** Create, edit, and delete clients (name, company, contact email, phone, notes, brand color, logo URL) from a new Clients page in the sidebar. Assign one or many sites to a client with the bulk "Set client" action on the sites list, replacing a long-standing placeholder stub. Filter the fleet by client and see each site's client from a dedicated Client column in the sites table. Each client has a detail page listing its assigned sites, with a Reports tab placeholder for the coming white-label reports phase. Deleting a client unassigns its sites; no sites are ever deleted. Clients are tenant-isolated with row-level security; site-scoped collaborators cannot enumerate the client roster; a database-level composite constraint makes cross-tenant assignment impossible. Also fixes a mislabeling: the column previously headed "Client" was rendering each site's tags; tags now have their own column back.

## [0.36.0] - 2026-06-10

### Added

- **Multiple named email connections with automatic failover.** A site can now define any number of named connections alongside its primary provider (for example, a backup SES account with the slug "ses-backup"). Each connection has its own provider, settings, and encrypted credential. The Routing tab is fully rebuilt: a Connections card lists every connection with its provider badge and identity, per-connection test sends, and an add/edit dialog; a Routing card lets you map specific FROM addresses to a connection and choose a fallback connection that is retried automatically on primary failure. The email log records which connection was actually used for each send. Behavior change: saving an email config now validates `default_connection`, `fallback_connection`, and per-FROM mapping values against the connections you have defined. Documented v1 limitation: bounce and complaint webhooks remain bound to the primary provider in this release; bounces routed through a non-primary connection's provider are not ingested until per-connection webhook tokens ship in a later release.
- **Org-wide email default now propagates automatically to every site.** Previously, saving the org-wide email default had no effect on sites that were already enrolled; each had to be synced manually. Now, saving the org default enqueues a background job that pushes the config to every connected and degraded site that inherits it (up to 8 in parallel, 15 seconds per site). A live SSE toast shows "Org email default synced to N/total sites" and warns when any site could not be reached. Sites with a per-site override are unaffected. This closes a consistency gap: the dashboard was already showing the org config as those sites' effective config before this release.
- **Attachment metadata in the email log.** Each logged email now records the names and sizes of any attachments (file names only, never paths or contents). List views show a paperclip and count chip next to the subject when attachments are present; the detail view shows name and formatted-size chips. Works for both the per-site log and the fleet-wide log. Agent local schema bumped to v11.
- **Failure alerts and scheduled deliverability digest.** Opt in to email alerts sent to operator-chosen recipients when a site's sends start failing (throttled to one alert per site per 60 minutes by default, configurable from 15 minutes to 24 hours). A separate weekly or monthly deliverability digest summarises sent, failed, and bounced counts per site with a top-failures list. Both are delivered via the instance mailer; the Notifications card on the Email tab shows a warning banner when instance email is not configured. Documented v1 limitation: per-failure alerts fire only on agent-reported failures (status=failed); bounces and complaints reported via provider webhooks count in the digest but do not trigger the per-failure alert in this release.

## [0.35.4] - 2026-06-10

### Added

- **Rendered HTML email preview in the email log.** A logged email's body now shows a real rendered preview (Preview / HTML source tabs) instead of raw markup. The preview renders inside a locked-down sandboxed iframe (no scripts, no same-origin) with a strict content-security policy, and the body is sanitized first. Remote images and tracking pixels are blocked by default with a per-message "Load remote images" opt-in. Plain-text bodies render as text. Security reviewed.

## [0.35.3] - 2026-06-10

### Fixed

- **Email logs never reached the dashboard** even though the site was logging sends locally. The agent pushes each batch to the control plane, but the ingest endpoint rejected every push with HTTP 422 because a provider `response` value that was a plain string (for example an SMTP "send OK" summary) did not match the expected JSON object shape, which failed the whole batch. Because the failed batch never advanced the agent's cursor, it retried the same rejected batch indefinitely and no logs were ever accepted. The ingest endpoint is now tolerant: a string, array, or scalar `response` is wrapped into an object, a missing or non-standard timestamp falls back gracefully, and a single odd entry can no longer block the batch. Existing buffered logs flow in automatically on the next push. The agent also now sends a clean object-shaped `response` and always-valid timestamps.

## [0.35.2] - 2026-06-10

### Fixed

- **Saved email config was never pushed to the site agent**, so sending a test email failed with "no email config — run sync_email_config first" and real outgoing mail would not route through the configured provider. Saving an email config now dispatches the signed `sync_email_config` command to the site so the agent receives the provider settings and credential immediately. The push is best-effort: if the agent is briefly offline the save still succeeds and the config syncs on the next save, test, or manual sync. Sending a test email now also re-syncs the config first, so a fresh save is always reflected.

### Added

- **"Sync to site" button** on a site's Email tab (Provider section) that pushes the stored email config to the site agent on demand, for re-syncing after the agent was offline at save time or after rotating a credential. New endpoint `POST /api/v1/sites/{siteId}/email/sync`.

## [0.35.1] - 2026-06-10

### Fixed

- **Email tab showed "Could not load email configuration" on sites that had never set up email.** A site with no per-site email config and no org-wide default returns a 404 by design, but the dashboard rendered it as an error instead of the first-run setup state. The Email tab now shows the provider setup form with a short "not configured yet" hint when no config exists.
- **Provider bounce and complaint webhooks could not reach the API behind the hosted load balancer.** The `/webhooks/*` path was not routed to the API service, so provider callbacks fell through to the web app. Self-hosters are unaffected (single service); the hosted load balancer now routes `/webhooks/*` to the API.

## [0.35.0] - 2026-06-10

### Added

- **Per-site email delivery (SMTP and providers):** configure any managed site's outgoing email from the WPMgr dashboard. Pick from Amazon SES, SendGrid, Mailgun, Postmark, or any generic SMTP server. Config is per-site or inherited from an org-wide default. Provider credentials are encrypted at rest with age(X25519) and never returned by the API (a `secret_set` flag is returned instead). Send a test email from the dashboard before saving.
- **Central email log:** every outgoing email from every managed site is logged centrally with full detail: to, from, subject, headers, status, provider response, and retry count. The log is paginated with free-text and column-scoped search, status and date filters, row-level detail with previous/next navigation, single and bulk resend, and CSV/JSON export. Email bodies are not stored by default; opt-in per tenant. Log entries auto-prune after 14 days.
- **Fleet-wide deliverability dashboard:** a cross-site view showing sent, failed, bounced, and complained counts across every managed site in one place. Per-site deliverability charts are also available on each site's Email tab. Live updates stream to the log and dashboard over SSE so a bounce flips an entry's status without a manual refresh.
- **Bounce and complaint handling with suppression list:** connect a provider's webhook (Amazon SES SNS, SendGrid, Mailgun, Postmark) and WPMgr automatically suppresses hard-bounced and complained addresses fleet-wide. The suppression list is consulted before each send. Manual add and remove are supported. Suppression can be scoped per-site or shared org-wide.

## [0.34.3] - 2026-06-10

### Fixed

- **Dialogs taller than the screen could not be scrolled** (most visible on the long backup dialog) on both desktop and mobile: the popup was frozen with its top and bottom cut off. The dialog component was rebuilt on Radix UI, which scroll-locks the page background correctly, and the dialog panel now caps to the viewport height and scrolls internally.

## [0.34.2] - 2026-06-10

### Fixed

- **One-click wp-admin login could still 502 on a second click while already signed in.** The 0.34.0 fast-path relied on `is_user_logged_in()`, which returns false inside a REST request reached by a plain browser navigation (no nonce), so it never fired and the login was re-issued over the live session and crashed the worker. The agent now detects the existing session by validating the `logged_in` cookie directly (nonce-independent), so the re-click just redirects. A shutdown-trap also converts any uncatchable fatal during login into a clean redirect instead of a 502.

## [0.34.1] - 2026-06-10

### Fixed

- The "Re-check connection" button now also appears for **disconnected** sites, not just connected and degraded ones. Disconnected is the case where a manual re-check is most useful, since it is the quickest way to recover a site that simply fell behind on its heartbeat.

## [0.34.0] - 2026-06-10

### Added

- **Re-check connection button** on the site row and site detail header. Clicking it forces an immediate liveness probe so you can resolve a stale connection badge on demand instead of waiting for the next heartbeat cycle.
- **Uptime pill** next to the connection badge on each site. Distinguishes "agent is quiet" (the site is up but heartbeating slowly) from "site is actually down" so the two states are never ambiguous.

### Fixed

- **One-click wp-admin login reliability**: clicking "Login to wp-admin" while already logged in could return a 502. The autologin now detects the existing session and redirects immediately, and the control-plane timeout is shorter so a slow site fails fast rather than hanging the browser tab.
- **One-click login now bypasses common 2FA plugins**: the autologin token was being intercepted at a second-factor prompt by several popular two-factor plugins (the official Two Factor plugin, WP 2FA, Wordfence Login Security, and miniOrange). The token exchange now lands past the 2FA gate for those plugins. The signed, single-use, expiring token and role allow-list are unchanged. Plugins that render a full interstitial page after WordPress authentication, such as Solid Security or Shield Security, may still show a prompt (ADR-055).
- **Connection badge flapping on low-traffic sites**: the per-site connection indicator could briefly flip to "degraded" on sites that are perfectly healthy but receive little traffic, because a single missed heartbeat beat would immediately trigger the state change. Missed beats are now debounced over several consecutive intervals and grace windows are wider, so transient heartbeat gaps on quiet sites no longer produce false alarms.

## [0.33.9] - 2026-06-10

### Changed

- WordPress.org plugin-directory compliance hardening for the agent (raised by the directory pre-review): request inputs including `$_SERVER` and `$_COOKIE` are sanitized; the media quarantine and database-snapshot data now write under the uploads directory, with a read fallback to the legacy `wp-content` location so existing installs keep working; the diagnostics info REST endpoint now binds its signed token to the site and endpoint (not just signature-valid); the login-screen branding CSS is enqueued instead of echoed; and the agent readme now documents every external service it can contact (control plane, object storage, ipify, Cloudflare, Google Fonts, Gravatar, and the optional third-party asset self-hosting) plus the public source of the bundled minified scripts. The streaming `mysqli` backup/restore connections and local file reads are kept and justified to the reviewer (the same pattern approved backup plugins use). No change to backup, cache, or optimization behavior.

## [0.33.8] - 2026-06-10

### Fixed

- Resolved 15 code-review findings (raised by automated review on earlier merged PRs, each re-verified against current code before fixing):
  - Agent: WooCommerce cart-fragments now inject on themes whose body tag carries attributes (the shim previously matched only a bare body tag); the cart-fragments load replay fires on the window; the cache hit tally counts 304 and HEAD responses; cache stats are staged and deleted only after a confirmed upload (with recovery of an interrupted batch); the stats consumer counts events by file size instead of reading whole files; and the Unused Image Cleaner bounds its in-use list.
  - Control plane: the cache hit-ratio history now returns the most recent data, daily-downsampled, instead of the oldest 366 hourly rows; a backup status no longer regresses after a failure is published; Media Cleaner thumbnail URLs are sanitized server-side; the OpenAPI auth documentation was refreshed and the missing auth paths documented; and a brittle deprecated-refcount test assertion was removed.
  - Web: Media Cleaner guards agent-supplied thumbnail URLs to http and https only; the agent-plugin download opens in a separate tab so a failed cross-origin download cannot replace the dashboard and lose the pairing code.
  - Build: the landing copy gate now runs as part of the landing build and uses a portable file path; the release Makefile validates the version as semver before stamping it into the plugin.

## [0.33.7] - 2026-06-10

### Fixed

- Optimize tab: changing one setting no longer makes every toggle flicker. The saving spinner and disabled state are now scoped to the row being changed instead of being applied to all rows at once, and a fast double-toggle no longer momentarily reverts.

## [0.33.6] - 2026-06-10

### Changed

- The site header "Open wp-admin" button now logs owners and admins straight into wp-admin in a new tab (one-click auto-login using a signed, single-use token) instead of landing on the WordPress login form. Non-admin viewers keep a plain wp-admin link.

## [0.33.5] - 2026-06-09

### Fixed

- The Real User Monitoring dashboard's default "All devices" tab showed "No data" even when the per-device tabs had data. The summary read path returned one row per device and country but never the device-agnostic aggregate the "All" tab reads, so the default view found nothing. The summary now returns, per metric, one country-collapsed row per device plus one all-devices aggregate (device-agnostic, summed across every device and country), and the 28-day trend collapses to a single series per metric for the selected device segment (or across all devices for "All"). The all-devices aggregate also crosses the minimum-sample floor sooner, so the dashboard populates with fewer total pageviews. Per-device tabs now also sum correctly across countries instead of showing a single country's slice. Control-plane only; no agent, migration, or data change.

### Added

- Core Web Vitals distribution bars on the Real User Monitoring dashboard. Under each p75 metric card (LCP, INP, CLS, and the secondary FCP and TTFB) a single stacked bar now shows the share of real pageviews in the good, needs-improvement, and poor bands, the way PageSpeed Insights and Search Console present field data. The bands are folded server-side from the histogram rollups already stored, at the standard Core Web Vitals thresholds, and respect the same minimum-sample floor that suppresses the p75 (a low-sample slice shows "insufficient samples", never a misleading bar).
- A 28-day p75 trend chart per metric on the Real User Monitoring dashboard, with the good and needs-improvement threshold lines drawn on it, so the operator can see where each metric sits relative to passing over time. Days below the sample floor render as a gap rather than a zero. A new read endpoint, `GET /api/v1/sites/:siteId/perf/rum/trend`, serves the daily series from the existing rollups, with no new tables and no agent change. Both the distribution and the trend follow the selected device tab and update live over SSE.

### Fixed

- The Real User Monitoring collector script is now served from a versioned URL, so a CDN or browser cache refetches it whenever the agent updates. The collector was served from a static, unversioned filename, so a long-lived edge cache (for example a one-year CDN TTL) could keep serving the previous collector build after a plugin update until the cache was manually purged, masking collector fixes. Versioning the URL changes it on every update, so the edge and the browser pick up the new bytes automatically.

## [0.33.3] - 2026-06-09

### Fixed

- Real User Monitoring now reliably collects CLS (Cumulative Layout Shift) on cached pages. In web-vitals, the CLS reporter is armed inside the First Contentful Paint callback; the browser collector was registering CLS before FCP, which on an already-cached page widened the timing window in which a load-and-leave visitor could hide the page before the CLS reporter was armed, dropping the measurement. The collectors are now registered in the canonical web-vitals order (TTFB, FCP, LCP, CLS, INP) so the CLS reporter is armed in the same delivery task as FCP, before any page-hide can interrupt it. Verified with a headless-Chromium repro test that induces a guaranteed layout shift then forces page-hide. Agent-only; no server or data change.

## [0.33.2] - 2026-06-09

### Fixed

- Real User Monitoring now collects CLS (Cumulative Layout Shift), completing Core Web Vitals coverage. In web-vitals, the CLS reporter is armed only after First Contentful Paint resolves, and the collector was loaded as a deferred script at the end of the page, so on a load-and-leave visit the page could be hidden before the CLS reporter was ever armed and no CLS measurement was sent. The collector is upgraded to web-vitals 5 (which resolves the paint gate correctly on briefly-hidden pages) and is now loaded early and asynchronously from the page head, so CLS is captured on every visit. Loading the collector earlier also slightly improves LCP and FCP accuracy. No server or data change.

## [0.33.1] - 2026-06-09

### Fixed

- Real User Monitoring now collects CLS and INP. The browser collector queued metrics and sent them in one batch when the page was hidden, but CLS and INP only finalize at page-hide and could be dropped by that flush, so only LCP, FCP, and TTFB were reported. The collector now sends each metric the moment it is finalized, so all Core Web Vitals are captured. INP still requires a real visitor interaction to exist, and CLS reports 0 on pages with no layout shift.

## [0.33.0] - 2026-06-09

### Added

- Real User Monitoring (RUM). Per-site, opt-in, off by default. When enabled, a
  tiny first-party collector script is injected into cached pages by the agent at
  cache-write time. The site visitor's browser beacons Core Web Vitals (LCP, INP,
  CLS, FCP, TTFB) plus page-load timing directly to the control plane. Data is
  anonymous: the page path is stored with the query string stripped, the IP is
  used only transiently for coarse country lookup then discarded, and no cookies
  or cross-site identifiers are set. Measurements are stored in Postgres histogram
  rollups (hourly and daily, with ClickHouse available as an opt-in scale backend
  via the same boot-selection pattern as the existing metrics store). The operator
  dashboard shows p75 per metric with per-URL and per-device breakdowns, live
  updates over SSE, and a minimum-sample floor that suppresses any slice below the
  configured count so noise is never presented as a metric. On a self-hosted
  control plane, all RUM data stays on the operator's own infrastructure.

## [0.32.1] - 2026-06-09

### Fixed

- The Cache and Optimize settings pages failed to load with an internal server error for every site after 0.32.0. The font-subsetting change in 0.32.0 added three new per-site columns but the read and save queries for the performance config were not regenerated to match, so the database read returned more fields than the query selected and errored. Both queries are now aligned; loading and saving performance settings works again, and the font-subsetting toggle now persists correctly (it was silently not saving in 0.32.0). Control-plane only; no agent, migration, or data change.

## [0.32.0] - 2026-06-09

### Added

- Font subsetting (experimental, default OFF). When both WOFF2 transcoding and font subsetting are enabled, the media-encoder produces a subsetted WOFF2 covering the latin-ext unicode range (U+0000 to 00FF, U+0100 to 024F, U+1E00 to 1EFF) alongside the full WOFF2. The browser fetches the subset for in-range codepoints and falls back to the full WOFF2 for anything outside that range, so no codepoint is ever broken. Typical savings on top of WOFF2 transcoding are 60 to 90 percent for body-text Latin fonts. Variable fonts and icon fonts are detected and skipped automatically; the full WOFF2 serves for those. Subsetting is gated behind the new `fonts_subset` per-site flag (default OFF) because OpenType shaping features (GPOS/GSUB ligatures and contextual kerning) are not preserved in the subset output.
- Per-font processing table on the Optimize tab. Each self-hosted font discovered on the site appears as a row showing its family name, original format, original size, WOFF2 size, subset size when available, savings percentage, and current state (pending, converting, ready, subsetted, skipped, or failed). A live indicator in the card header streams aggregate progress during an active page build. Skipped and failed rows show the reason so you can verify that icon or variable fonts were correctly left alone.
- External-stylesheet font discovery. The agent now scans fonts loaded by classic themes and plugins via enqueued external stylesheets, in addition to the inline style block scan added in ADR-052. This closes the main discovery gap for sites that load fonts through `wp_enqueue_style` rather than printing inline font-face rules.

## [0.31.2] - 2026-06-09

### Added
- WordPress.org distribution build ("Fleet Agent Site Manager") that passes the official Plugin Check with zero errors. A build-time variant excludes the control-plane self-updater from the WordPress.org package, since those builds update through WordPress.org; the self-hosted and SaaS builds keep control-plane self-update.
- Public Terms of Service and Privacy Policy pages on the control plane (manage.wpmgr.app/terms and /privacy), linked as the external-service disclosure from the agent readme.

### Changed
- Agent code hygiene for WordPress.org compliance: all diagnostic logging now routes through a debug-gated helper that writes only under WP_DEBUG_LOG or WPMGR_DEBUG; swapped to WordPress wrappers where appropriate (wp_parse_url, wp_delete_file, wp_mkdir_p, wp_rand, wp_remote_get); added request unslashing and sanitization; and annotated the intentional streaming file and plugin-owned table database operations. No behavior change to backups, restore, cache, or performance.
- The WordPress.org build declares GPLv2 or later. The source stays MIT, which is GPL compatible.

## [0.31.1] - 2026-06-08

### Fixed

- Cancelling enrollment of a site that never connected now removes it cleanly so you can add the same URL again immediately. Sites that have connected are still archived with their history, as before.
- The Sites page now surfaces disconnected sites even when you have no active sites, with Reconnect and Remove actions, so a previously connected or stranded site is never trapped on an empty screen.
- Adding a URL already on your account now offers to reconnect that site (or open it if already connected) instead of returning a raw error.

## [0.31.0] - 2026-06-08

### Added

- Font transcoding to WOFF2. Per-site, opt-in, default OFF. When enabled, WPMgr transcodes self-hosted fonts (TTF, OTF, WOFF) to WOFF2 and serves the compressed variant with the original as a format() fallback. Typical savings are 50 to 65 percent for TTF and OTF, and 20 to 30 percent for WOFF. Transcoding runs in the background in the media-encoder service; the original font is served until the WOFF2 is ready, so pages never wait, and any transcoding failure falls back to the original so a font never renders broken.

### Fixed

- Google Fonts setting copy that incorrectly said "and combine": WPMgr self-hosts each Google Fonts stylesheet individually and does not combine them.

## [0.30.0] - 2026-06-08

### Added

- WooCommerce cart-session page caching (#169). Per-site, opt-in, default OFF. When enabled, catalog pages (shop, category, home, blog) are served from the page cache for shoppers who already have items in their cart; cart totals and the mini-cart update live via WooCommerce's own cart-fragments mechanism. Cart, checkout, and account pages are always bypassed. WPMgr auto-detects whether the active theme supports cart fragments and only surfaces the toggle when it does. Conservative by design: any uncertainty about theme support, cart state, or a sensitive form token causes the full uncached page to be served so a shopper never sees the wrong cart.

## [0.29.0] - 2026-06-08

### Added

- `validate-env` command (also `make validate-env`) that checks your configuration and prints every problem at once before you start the stack, so you discover missing or invalid environment variables before the first container starts instead of one restart at a time.

### Changed

- The control plane no longer restart-loops when a required setting is missing or invalid. It stays up in a degraded state: `/healthz` keeps answering, and `/readyz` returns a 503 that names exactly which environment variables are misconfigured (names and reasons only, never values), so you can read the endpoint to diagnose the problem instead of watching a crash loop.

### Fixed

- A failed backup now shows the real reason in the dashboard (for example a database connection failure) instead of only a generic "stalled, no progress" message. The agent's failure detail is preserved on the snapshot so you can see what actually went wrong.

## [0.28.1] - 2026-06-08

### Fixed

- Backups on hosts that expose MySQL over a Unix socket (for example a `DB_HOST` of `localhost:/var/run/mysqld/mysqld.sock`). The database dumper now parses the host, port, and socket path from `DB_HOST` the same way WordPress core does, and connects over the socket instead of dropping the path and failing the dump. Sites that connect over TCP are unaffected.

## [0.28.0] - 2026-06-08

### Added

- Cache hit-ratio history (#162). Per-site page-cache hit and miss counts are now recorded as a time-series and shown on the performance dashboard as a trend chart with 7, 30, and 90 day windows. The agent tallies hits and misses to lightweight per-hour files so no database work is added on a cache hit; the control plane mirrors the rollup into its own time-series so you can track how cache effectiveness changes over time without slowing down cached responses.

### Changed

- Guided "Connect your site" onboarding. After signing up, the first-site flow now leads straight into the real connect step. It walks through downloading the agent plugin, opening the WPMgr menu in wp-admin, pasting the control-plane URL (shown inline for one-click copy), pasting the one-time pairing code, and clicking Enroll. The wording matches the labels in wp-admin so there is no guesswork. Earlier experimental auto-install options are hidden for now and will return once the agent is published on the WordPress.org plugin directory.

### Fixed

- Unused Image Cleaner quarantine path safety. If the WordPress content directory cannot be determined, the unused-image quarantine now refuses to run instead of falling back to a path at the filesystem root, which previously could cause permission failures or writes outside the wp-content directory. Normal sites are unaffected.

## [0.27.0] - 2026-06-08

### Changed

- Unified versioning: the open-source release tag, the api/web/media-encoder container images, and the WordPress agent plugin now all share one version number. The number jumps from 0.20.0 to 0.27.0 to land above the agent's prior 0.25.x line so the agent self-update applies cleanly. From this release forward a single tag controls what ships everywhere.

### Fixed

- Unused Image Cleaner: a re-scan no longer resurfaces attachments that are already in quarantine. Isolated items (files moved to quarantine, post still present) are excluded from scan candidates and reported as a separate quarantined count.

## [0.20.0] - 2026-06-08

### Added

- Incremental backup engine v1 (ADR-048) and chain restore (ADR-049). Schedule incremental backups per-site via a toggle on each backup schedule. An increment compares the live file tree against the parent snapshot's file list by size and modified time, packs only changed and new files into standard part archives, and streams deletions to a tombstone sidecar on disk. The state the agent must carry across requests dropped from thousands of per-file records to roughly 25 part names, the same tiny cursor a full backup uses, which was the root cause of the previous 0-files silent data-loss bug. Restore overlays each generation in order with newest-wins extraction and then applies tombstone deletes, so any point in the chain restores correctly. The database is dumped in full on every run. The archive-delta rewrite (ADR-051) replaced the previous per-file chunk scanner and is the shipping incremental model.
- Incremental chain visibility: incremental backups render as a single expandable row grouping the base backup and all its increments, with chain fields, a badge, and SSE phase labels that report progress in real time.
- Point-in-time restore version picker (chain restore, ADR-049): when restoring, pick the exact snapshot to roll back to. Files and database both restore to the chosen point in the chain, with the site staying online throughout.
- Selective-component backup: choose which components to include per backup (files, database, WP core), define exclusion patterns, lock a snapshot to prevent it being swept by retention GC, and receive a backup-completion email. Backup settings are now decoupled from the schedule so each schedule carries its own component selection.
- Mark-and-sweep retention GC (ADR-050): old backup generations are collected automatically based on configurable retention rules without manual cleanup.
- Standalone Search and Replace tool (serialization-safe): run a database-wide find-and-replace that handles PHP-serialized data correctly, so URLs and other structured values survive without corruption.
- Database Snapshots tool: take a quick local database snapshot before a risky change, then revert to it instantly if something goes wrong. Faster and lighter than a full backup for local safety nets.
- Unused Image Cleaner (Media): scans the WordPress media library for attachments that are not referenced anywhere and reports exactly where each in-use image appears (post content, block editor image IDs, SEO meta fields, options pages, and more). Unused images move to a reversible server-backed quarantine; permanent deletion requires an explicit confirmation step. Conservative by design: any ambiguous reference is treated as in-use, so a genuinely used image is never flagged. The optimizer's own bookkeeping metadata is excluded from the scan.
- Media Optimizer reliability: the scale-to-zero encoder now wakes when a job is enqueued, so jobs no longer sit waiting on a cold encoder. The encoder also shuts down gracefully, and cancelling an optimize job cancels its background encode job so no orphaned work remains.
- API spec coverage: the restore-run and schedule-run backup endpoints are now documented in the OpenAPI spec, with a routes-contract test to keep them in sync.
- Brand favicon (Fleet Hub mark) and theme-color meta tag in the web app.

### Fixed

- Self-host key and secret generator now produces correct values; `.env.example` is updated to match. The Docker Compose setup is more resilient to partial starts.
- Incremental reliability: base file-index bootstrap, chain-merge file-index correctness, auto-rebase on corrupt chain, 0-files data-loss prevention, and single-pass chunking performance are all addressed across a series of targeted fixes.
- PHP and JS CI jobs are green: PHPUnit mocks and ESLint both pass cleanly.

## [0.19.0] - 2026-06-04

### Added

- Database Cleaner Phase 3.1 (Corpus Foundation): adds the `plugin_signatures` global reference table to Postgres, a v1 seed covering the ~120 highest-orphan-risk plugins with their known option, transient, table, and cron-hook name patterns, and an `internal/dbclean` Go package skeleton with the `CorpusReader` interface, `CorpusPostgresReader` backed by a new sqlc query, `Signature` type, `ConfidenceLevel` enum (exact / prefix / heuristic / unknown), and the `Classification`, `OrphanedOption`, `OrphanedCronEvent` types. Nothing in this phase is destructive; the corpus is dormant read-only reference data. Includes `tools/corpus-gen/`, an offline tool (separate Go module; never part of the API build) that lists popular slugs from the wordpress.org API, downloads plugin ZIPs, scans PHP source, applies document-frequency suppression and a generic-literal blocklist, and emits a SQL seed migration. The tool enforces ZIP-SLIP and SSRF guards, a 2 req/s rate limit, and validates all emitted patterns as RE2-safe before writing. Migration M40. (Migrations 20260605000000, 20260605010000.)
- Database Cleaner Phase 3.1 security hardening: all anchored prefix patterns in the corpus seed must now have at least 4 characters before the first underscore (the minimum prefix body length). Short prefixes such as `^et_`, `^ep_`, `^lp_`, `^ls_`, `^kb_`, `^vc_`, `^nf_`, `^bp_`, `^gf_`, `^rg_`, `^fm_`, `^ac_`, `^um_`, and `^ct_` were removed or replaced with longer unambiguous co-prefixes (for example `^elasticpress_`, `^ultimate_member_`, `^learnpress_`, `^ninja_forms_`). The `corpus-gen` tool enforces the same floor via `minPrefixBodyLen = 4` and rejects short patterns at generation and emission time. `WPMGR_DB_MIGRATION_DSN` (owner DSN) is now documented as required: the seed migration inserts rows into `plugin_signatures` where `wpmgr_app` has INSERT revoked; the API server logs a startup warning when the env var is unset. The `plugin_signatures` REVOKE statement is now mirrored in `db/schema.sql` so tooling diffing against the live database sees the complete write guard. `.gitignore` updated to exclude the forbidden reference directories and the `corpus-gen` compiled binary.
- Database Cleaner, end to end. A full self-contained workflow now ships for scanning and cleaning a WordPress database: a read-only scan shows how many rows each category holds and how much space a clean would recover before anything is deleted; a per-table inventory lists every table with its row count, size, storage engine, and overhead; each table is labelled as WordPress core, an active plugin or theme, or an orphan left behind by a removed plugin; orphaned options and cron events are classified by matching against the corpus of known plugin signatures and marked with a confidence level (exact, prefix, heuristic, or unknown); a 90-day health trend records database size and overhead over time so you can see whether cleanup is keeping pace with growth; a fleet view surfaces every site's database health in one place so you can act on the worst offenders across a portfolio without opening each site individually; per-table maintenance actions cover optimize, repair, analyze, convert to InnoDB, empty, and delete, each gated by a typed confirmation; orphaned tables and orphaned option rows can be deleted in bulk with a guarded confirmation; cleanup tasks can run on a schedule the control plane drives, stream live per-category progress, and are batched so they never lock a busy database; a failed or silent run is detected and surfaced as failed rather than appearing stuck. Agent 0.15.3 to 0.15.9.
- Performance Suite, per site and across your whole portfolio. Turn on full-page caching and WPMgr serves anonymous pages as pre-gzipped HTML straight from disk, with logged-in, per-role, mobile, and per-query cache variants, bypass rules for cart and checkout pages, a configurable refresh interval, manual and automatic purge, and a preload warmer. The server fast-path installs automatically on Apache, with a copy-paste snippet for nginx and built-in handling for OpenLiteSpeed and WP Engine.
- Asset optimization that makes pages lighter without breaking them: CSS and JS minification, JS delay, font display-swap and self-hosting, lazy-load with width and height and srcset preserved, bloat removal, CDN URL rewriting with encrypted credentials, and on-demand or scheduled database cleanup. A failed optimization never breaks the page, it simply falls back to the original asset.
- Remove Unused CSS strips the rules a page does not use and inlines only what it needs, computed by WPMgr's own engine with no headless browser and no third-party service. Interactive states like hover and focus are always kept, a per-site safelist covers anything added by scripts, and results are cached and shared across pages with the same structure. On a cache miss or any failure the full CSS is served, so rendering is never blocked.
- Per-site controls plus portfolio bulk actions: save the performance config for one site, purge the cache across many sites at once, or apply a safe, balanced, or aggressive preset to a whole group in one run.

### Fixed

- Remove Unused CSS now keeps sliders, lightboxes, and other JavaScript-driven widgets working out of the box. These build their markup and add their state classes after the page loads, so the optimizer could not see them and stripped their styles, which left a slider stuck hidden. WPMgr now ships a built-in safelist of common runtime classes (sliders, carousels, lightboxes, and is-active or is-initialized style state classes) that is always kept, and the agent now actually sends your per-site safelist to the optimizer so anything you add there is honored too. Existing sites recompute their used CSS once after the update with this safety net applied. Agent 0.15.1.
- The cache "Last purge" gauge now records the time of a purge instead of always showing "Never". The control plane stamps it the moment you run a purge from the dashboard, and the agent also reports its own full-cache purges (for example an automatic purge after you edit content) so the gauge stays accurate even for clears the dashboard did not start. Agent 0.15.2.
- Optimize panel toggles no longer flicker or momentarily revert when you change one setting; each save now updates only what changed instead of refetching and re-rendering the whole panel.
- Fixed three settings that were silently rejected and rolled back when saved: the "Delay until interaction" JavaScript option, the "Every 30 minutes" cache refresh interval, and the CDN provider field (now a picker limited to the supported providers instead of free text).
- The database cleaner now actually works. Previously it reported "0 rows cleaned" no matter what, ignored which cleanup tasks you selected, and never ran on a schedule. It now removes the categories you choose (post revisions, auto-drafts, trashed posts, spam and trashed comments, expired transients, orphaned and duplicate metadata, oEmbed cache, table optimization, and more), streams live per-category progress as it runs, and supports a scheduled automatic clean that the control plane drives. Large cleanups are batched so they never lock a busy database, and the cleanup is careful not to remove rows it cannot confidently identify as safe. Agent 0.15.3.
- The database cleaner now scans before it cleans. A new read-only scan shows, per category, how many rows can be removed and how much space you would reclaim (including table-optimization overhead) before you delete anything, so you can pick exactly what to clean and see the total savings up front. Cleanups now also recover gracefully: if a run goes silent it is detected and reported as failed instead of appearing stuck, and each category reports progress as it goes. Agent 0.15.4.
- The database scan now includes a full per-table inventory: every table with its row count, size, storage engine, and overhead, and a "Belongs to" label that identifies whether a table is WordPress core, owned by an active plugin or theme, or an orphan left behind. The table list is paginated, searchable, sortable, and filterable (all tables, orphans, plugin tables, theme tables, WordPress core), so you can see exactly what is taking up space across the whole database. Agent 0.15.5.
- Table ownership is now far more accurate. Tables are matched to the plugin or theme that created them by inspecting installed source, so a plugin's tables are attributed correctly even when the table name does not match the plugin's folder name (for example WooCommerce's wc_ tables). Active plugins' tables are no longer mislabelled as orphans. You can also act on individual tables now: optimize or repair any table, and drop a leftover orphan table, from the table list, with a typed confirmation required before any table is dropped. Agent 0.15.6.
- You can now empty a table to reclaim space. Emptying a table (such as a large plugin log table) deletes all of its rows but keeps the table itself, which is the right way to clear space without removing the table. Emptying is available per table and in bulk, refuses WordPress core tables outright, and requires a typed confirmation. Bulk actions now run the action you choose instead of always optimizing. Agent 0.15.7.
- Deleting a whole table is now available for plugin and theme tables, not just orphans. "Empty" clears a table's rows while keeping the table; "Delete" removes the table entirely (the owning plugin recreates it on next run if it needs it). Both appear as distinct options per table and in bulk, both refuse WordPress core tables, and both require a typed confirmation. Agent 0.15.8.
- Two more per-table maintenance actions: "Analyze" refreshes a table's row-count statistics so the inventory numbers are accurate, and "Convert to InnoDB" upgrades an older MyISAM table to the modern InnoDB engine without losing data (offered only for tables that are not already InnoDB). Both are safe, non-destructive operations. Agent 0.15.9.

## [0.17.0] - 2026-06-03

Agent: 0.14.0-perf-live.

### Added

- Server-status verify card: the Cache tab now shows the real install state of
  the page cache on the host (web server detected, drop-in present, WP_CACHE
  constant set, managed .htaccess block in place) along with live gauges
  (cached pages, cache size, last purge, last preload). Previously the dashboard
  showed "not set" or zeros even when caching was fully operational.
- Optimization auto-applies on enable: turning on the page cache for a site now
  immediately pushes the full optimization config (CSS/JS minify, lazy-load, font
  display-swap, proper image sizing) to the site by default. Each toggle can still
  be turned off individually.
- Live preload progress: cache preload now streams progress and a completion event
  to the dashboard so the spinner resolves to a result. A client-side stale
  timeout fires if the stream goes quiet, so the UI never hangs indefinitely.
- Remove Unused CSS "Compute now" action: operators can trigger RUCSS computation
  for specific URLs on demand from the dashboard. The job streams a live
  queued to computing to reduced-N% progress sequence. Visitor-driven passive
  background computation continues as before.
- Page-source marker: pages optimized and cached by WPMgr now carry an HTML
  comment footprint with a timestamp ("Optimized and cached by WPMgr"), so
  operators can confirm cache and optimization are active by viewing page source.

### Fixed

- WP_CACHE remediation: when the agent cannot write `define('WP_CACHE', true);`
  to wp-config.php (file not writable), the dashboard surfaces the exact line to
  add manually instead of failing silently.
- nginx and OpenResty sites now correctly reflect that the PHP drop-in serves
  cache hits without .htaccess; the install-state card no longer marks
  `htaccess_managed` as an error condition on those servers.

## [0.16.9] - 2026-06-03

### Added
- Operator account recovery: a one-shot, env-driven seeder (`WPMGR_RECOVER_ACCOUNTS`) recreates a deleted user and re-attaches it as owner of an existing organisation it had lost access to, then logs a one-time set-password link. Lets an instance operator restore an account whose organisation and sites are still intact after an accidental user deletion, without touching the database by hand.

## [0.16.8] - 2026-06-03

### Fixed
- The superadmin orphaned-organisation cleanup added in 0.16.7 silently failed to remove empty organisations: deleting a user left their now-empty organisation behind and the organisation count unchanged. This was a database privilege interaction with the append-only audit log; empty orphaned organisations are now removed reliably when their sole owner is deleted, and organisations that still own sites are still kept and flagged.

## [0.16.7] - 2026-06-03

### Changed
- Superadmin user delete now tidies up the organisations that user solely owned: an organisation left with no members and no sites is removed automatically, and the user list shows an accurate organisation count per user.

### Fixed
- Deleting a superadmin-managed user no longer leaves behind an empty, unreachable organisation. When such an organisation still has sites, it is kept and the operator is warned to reassign or remove it rather than losing track of those sites.

## [0.16.5] - 2026-06-03

### Fixed
- Open self-serve sign up failed for everyone after the first account because every new workspace was created with the same internal identifier. Each sign up now gets a unique one, so registrations no longer collide.

## [0.16.0] - 2026-06-03

### Added
- Superadmin area for instance operators: a cross-tenant user list with search, the ability to delete or disable a user, resend a verification email, and an instance stats overview. Visible only to accounts listed in the superadmin allowlist; it cannot be granted through the app.

## [0.15.5] - 2026-06-02

### Added
- Site sharing now emails the person you share with: a new user gets a branded invite link to set their own password, and an existing user gets a notification that a site was shared with them and is ready in their account.

## [0.15.4] - 2026-06-02

### Fixed
- Creating your first organisation from the welcome screen returned a 403; org create, list, and switch no longer require an existing organisation, and creating one now drops you straight into it.

## [0.15.3] - 2026-06-02

### Added
- Invite teammates to an organisation by email: they receive a branded link and set their own password, so admins no longer choose a password on their behalf.
- A welcome screen that invites you to create an organisation when your account does not belong to one yet.

### Changed
- Trying to sign up with an email that already has an account now sends a short "you already have an account" email pointing to sign in or password reset, instead of doing nothing.

### Fixed
- Saving SMTP settings could fail with a server error; the settings now save reliably.

## [0.15.0] - 2026-06-02

### Added
- UI-configured SMTP: admins set SMTP credentials in Settings, the password is stored encrypted, and a test-send button confirms delivery before saving.
- Self-serve password reset and a strengthened change-password flow; changing a password immediately revokes all other active sessions.
- Open self-serve sign up with email verification: new users register with their email address and gain access only after clicking a verification link.

[0.15.0]: https://github.com/mosamlife/wpmgr/releases/tag/v0.15.0
