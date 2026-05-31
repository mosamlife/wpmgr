# Sites API — enrollment & connection lifecycle

Endpoints for the site-first enrollment flow, the connection-lifecycle SSE
stream, operator lifecycle actions, and the agent heartbeat/disconnect calls
(M21 / Phase 5.7). Source of truth: `packages/openapi/openapi.yaml`.

Design: [ADR-038](../adr/ADR-038-sse-channel-scoping.md),
[ADR-039](../adr/ADR-039-heartbeat-cadence-timeouts.md),
[ADR-040](../adr/ADR-040-agent-last-will-disconnect.md),
[ADR-041](../adr/ADR-041-reenrollment-identity-connection-state.md).
User guide: [features/site-lifecycle.md](../features/site-lifecycle.md).

## Auth & RBAC

| Group | Auth | Scope |
|-------|------|-------|
| Dashboard reads (`GET /sites/events`) | session / API key | `site:read` |
| Dashboard create (`POST /sites`) | session / API key | `site:write` |
| Destructive lifecycle (`revoke`, `archive`, `restore`, `enrollment-codes`) | session / API key | `site:write` **+ org scope** (`RequireOrgScope`) + `RequireSiteAccess` |
| Agent (`heartbeat`, `disconnect`) | Ed25519 signed-request | identity bound from the verified agent key |

> **Org scope on destructive actions.** Revoke / archive / restore / re-enroll
> rotate the agent identity or soft-delete the site, so a *site-scoped*
> collaborator (an outside operator shared exactly one site) may operate it but
> **may not** sever it from under the owner — these routes require org scope on
> top of `site:write`. (Phase 6 security review, finding #5.)

> **Agent routes are not session-authenticated.** `POST /agent/v1/heartbeat`
> and `/disconnect` run behind the agent Authenticator: the request is Ed25519
> signed-request verified and the site/tenant resolved from the proven key
> *before* the handler runs. Possession of a `site_id` alone cannot disconnect
> a site. See [agent.md](../agent.md#security-model).

---

## Dashboard endpoints

### POST /api/v1/sites — create a site (site-first enrollment)

Creates a site in `pending_enrollment` **and** mints a single-use, site-bound
enrollment code in one call. The response carries the new `site_id` plus the
once-shown code and its expiry, so the dashboard can open the install modal and
subscribe to `GET /sites/events` for this `site_id`.

> **Breaking change vs. pre-M21.** The 201 body is now `SiteEnrollmentCode`
> (`{site_id, enrollment_code, expires_at}`) instead of a bare `Site`. When the
> connection-lifecycle service is disabled (dev builds with no SSE bus), the
> control plane falls back to the legacy create that returns a bare `Site`.

**Request**

```http
POST /api/v1/sites
Content-Type: application/json

{ "url": "https://blog.example.com", "name": "Example blog", "tags": ["client-a"] }
```

**Response** `201 Created`

```json
{
  "site_id": "6f1c2b7e-9d4a-4f3e-8c21-0a2b3c4d5e6f",
  "enrollment_code": "JBSWY3DPEHPK3PXP4F2A7QZ9",
  "expires_at": "2026-05-31T18:15:00Z"
}
```

The `enrollment_code` is shown once and never retrievable again. It expires 15
minutes after minting (`pairingCodeTTL`).

Errors: `409` (URL already exists for this tenant), `422` (validation, e.g.
`site_url_scheme`).

### GET /api/v1/sites/events — connection-lifecycle SSE stream

A single **tenant-scoped** Server-Sent Events stream of lifecycle events. The
client filters by `site_id` in the browser. Each frame carries an `id:` (ULID),
an `event:` (the event type), and a JSON `data:` body; keepalive comments every
15s.

> **Per-site filtering.** The stream is tenant-keyed but the server filters both
> the replay and the live fan-out to the principal's allowed sites — a
> site-scoped collaborator sees only events for sites in their allowlist.
> (Phase 6 finding A.) A principal may hold at most 8 concurrent streams
> (finding D).

**Request**

```http
GET /api/v1/sites/events?since=01J0K8ABCDXYZ
Accept: text/event-stream
```

`?since=<event_id>` (or the `Last-Event-ID` header) replays events strictly
after that ULID cursor from the durable journal (~5 min retention). On reconnect
the browser sends `Last-Event-ID` automatically.

**Response** `200 OK` (`text/event-stream`)

```
id: 01J0K8ABCDXYZ
event: site.state_changed
data: {"id":"01J0K8ABCDXYZ","type":"site.state_changed","tenant_id":"…","site_id":"6f1c2b7e-…","ts":"2026-05-31T18:01:02Z","data":{"from":"pending_enrollment","to":"connected","enrolled":true,"site":{"id":"6f1c2b7e-…","connection_state":"connected","connection_generation":1,"health_status":"healthy","status":"active","url":"https://blog.example.com","name":"Example blog","enrolled":true,"last_seen_at":"2026-05-31T18:01:02Z"}}}

:

```

**Event types**

| `event:` | Emitted when |
|----------|--------------|
| `site.created` | A site row is created (pending_enrollment). |
| `site.state_changed` | Any transition (enroll/connect, recovery, re-enroll). Carries `data.from` / `data.to`. |
| `site.revoked` | Operator revoked the site. |
| `site.disconnected` | Last-will or heartbeat-timeout disconnect; carries `data.reason`. |
| `site.archived` | Site archived. |
| `site.restored` | Site restored from archive. |

Every event embeds a compact `site` summary (current `connection_state`,
`connection_generation`, `health_status`, `last_seen_at`, …) so the dashboard
row updates in place without a refetch. The summary never includes the agent
key.

### POST /api/v1/sites/{siteId}/enrollment-codes — begin re-enrollment

Moves an existing `disconnected` / `revoked` / `archived` site back to
`pending_enrollment` (bumping `connection_generation`) and mints a fresh
site-bound code under the **same** `site_id`, preserving backup/scan/uptime
history.

**Response** `201 Created`

```json
{
  "site_id": "6f1c2b7e-9d4a-4f3e-8c21-0a2b3c4d5e6f",
  "enrollment_code": "MFRGGZDFMZTWQ2LKNNWG23TP",
  "expires_at": "2026-05-31T18:30:00Z"
}
```

Errors: `409` (current state does not permit re-enrollment).

### POST /api/v1/sites/{siteId}/revoke — revoke a site's connection

Operator action. Transitions the site to `revoked` and queues a `revoke`
instruction returned (with a signed token) on the agent's next heartbeat; the
agent then wipes its keys + self-deactivates.

**Request** (optional reason)

```http
POST /api/v1/sites/6f1c2b7e-…/revoke
Content-Type: application/json

{ "reason": "offboarding client-a" }
```

**Response** `200 OK` — the updated `Site`.

Errors: `409` (illegal transition from the current state).

### POST /api/v1/sites/{siteId}/archive — archive (terminal soft-delete)

Operator action. Transitions the site to `archived`; hidden from the default
list, history preserved. Accepts an optional `{reason}` body.

**Response** `204 No Content`. Errors: `409` (illegal transition).

### POST /api/v1/sites/{siteId}/restore — restore (un-archive)

Operator action. Un-archives the site back to `disconnected`. Only valid from
`archived`.

**Response** `200 OK` — the updated `Site`. Errors: `409` (site is not
archived).

### GET /api/v1/sites — list (state filter)

Archived sites are hidden by default. Pass `?state=<connection_state>` to filter
to one state (e.g. `?state=archived` for the archived chip), or
`?include_archived=true` as an alias for `state=archived`. The `state` enum is
`pending_enrollment | connected | degraded | disconnected | revoked | archived`.

---

## Agent endpoints (Ed25519 signed-request)

### POST /agent/v1/heartbeat — liveness heartbeat (60s)

The 60s agent beat (ADR-039). Refreshes `last_seen_at`, recovers a
`degraded`/`disconnected` site to `connected`, and returns any pending
instructions. The body may carry light metadata (status, versions,
pending-update count); it is accepted best-effort and currently not persisted.

**Request** (signed; headers omitted for brevity)

```http
POST /agent/v1/heartbeat
Content-Type: application/json
X-WPMgr-Agent-Key: <base64 ed25519 pubkey>
X-WPMgr-Signature: <base64 sig>
X-WPMgr-Timestamp: 1748714400
X-WPMgr-Nonce: <jti>

{ "site_id": "6f1c2b7e-…", "ts": 1748714400, "status": "ok",
  "wp_version": "6.8.1", "installed_updates_count": 3 }
```

**Response** `200 OK` — normal beat:

```json
{ "ok": true }
```

**Response** `200 OK` — a **revoked** site (the agent must verify
`revoke_token` before acting; see [ADR-040 addendum](../adr/ADR-040-agent-last-will-disconnect.md#addendum-2026-05-31---signed-revoke-instruction-phase-6-security-review)):

```json
{ "ok": true, "instructions": ["revoke"], "revoke_token": "<compact-ed25519-jwt>" }
```

`revoke_token` is a short-lived Ed25519 JWT (`cmd="revoke"`, `aud=<site_id>`)
minted by the existing agentcmd signer. It is present only when the control
plane has a signing key configured; otherwise the `revoke` instruction is
returned unsigned and the agent ignores it (fail-closed).

> Pre-M21 control planes (no lifecycle sink) return `204 No Content` with no
> body. Errors: `401` (agent authentication failed).

### POST /agent/v1/disconnect — signed last-will

The agent's signed last-will (ADR-040), fired by the WordPress
deactivation/uninstall hooks (best-effort, 3s timeout). Transitions the site
`connected`/`degraded` → `disconnected` with the supplied reason. Does **not**
archive.

**Request** (signed)

```http
POST /agent/v1/disconnect
Content-Type: application/json
X-WPMgr-Agent-Key: <base64 ed25519 pubkey>
X-WPMgr-Signature: <base64 sig>

{ "reason": "deactivated" }
```

`reason` is one of `deactivated | uninstalled | user_initiated` (defaults to
`user_initiated`; truncated to 64 chars).

**Response** `200 OK`

```json
{ "ok": true }
```

Errors: `401` (agent authentication failed), `503` (`lifecycle_disabled` on a
control plane without the lifecycle sink wired).
