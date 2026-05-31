# ADR-042 — CP-Driven WordPress Agent Self-Update

**Status:** Accepted · **Date:** 2026-05-31
**Supersedes/relates:** ADR-031 (agent command signing), ADR-039/040/041 (connection lifecycle).
**Plugin key:** `wpmgr-agent/wpmgr-agent.php` · **Slug:** `wpmgr-agent`.

The release zip now always ships a top-level `wpmgr-agent/` folder (the
`agent-zip` build was fixed alongside the cron self-heal in
`v0.10.5-cron-selfheal`), so a package install replaces the existing plugin
folder **in place** instead of creating a versioned duplicate.

---

## Context

Operators install agent updates by manually uploading a zip. WordPress did not
recognise those uploads as updates of the same plugin (the slug was derived from
the zip filename), so the only way to install a new build was to deactivate /
delete / re-upload — which fires `register_deactivation_hook` →
`Scheduler::clearEvents()`, wiping every wp-cron event. Because
`register_activation_hook` does **not** fire on an in-place update, the heartbeat
cron never came back and the control plane's 360 s sweeper marked the site
disconnected. The stable-slug fix + the `plugins_loaded` cron self-heal stop the
bleeding, but the underlying friction — *there is no real update channel* —
remains.

This ADR adds a proper update channel: the WordPress dashboard shows
**“Update available”** for the WPMgr agent and the operator updates with one
click, exactly like a wp.org plugin, with no manual upload.

Delivering a plugin zip is delivering **executable code to every managed site**.
A weakness here is fleet-wide RCE, so the channel is designed signature-first.

---

## 1. Decision

- **Hosting.** Agent release zips + a `latest.json` manifest live in the existing
  object-storage bucket (`wpmgr-chunks-prod`, GCS via the S3-compat endpoint
  `https://storage.googleapis.com`) under the `agent-releases/` prefix:
  - `agent-releases/latest.json` — the pointer the CP reads.
  - `agent-releases/<version>/wpmgr-agent.zip` — the immutable package for a
    given version.
  The **release pipeline writes** these (`make agent-release`); the **CP only
  reads** them. No database rows — manifest state is fully stateless from object
  storage (`blobstore.Store.Get`).

- **CP mints a signed manifest** on `GET /agent/v1/update/manifest`
  (agent-authenticated). It reads `latest.json`, mints a short-lived presigned
  GET URL for the package (`blobstore.Store.PresignGet`, TTL ≤ 300 s), and
  returns a manifest **signed with the existing Ed25519 signer**
  (`agentcmd.Signer` — the same key that mints `revoke`/autologin tokens).

- **Signed payload = detached raw Ed25519 signature over the canonical manifest
  JSON, NOT the 60 s JWT.** The agent caches the manifest for ~12 h and verifies
  it offline well past 60 s; the JWT path clamps `exp ≤ now+60 s`, which is too
  short. The CP signs the manifest body with `ed25519.Sign(priv, body)`
  (`Signer.SignManifest`); the agent verifies with
  `sodium_crypto_sign_verify_detached` against the stored control-plane public
  key (the same primitive `Connector::verify` already uses). The manifest
  carries its **own** claims and the agent re-enforces them in code (it cannot
  route through `verifyCommand`'s 60 s clamp).

  Manifest claims: `aud` (site id), `cmd` = `"update_manifest"`, `slug`,
  `version`, `min_version`, `package_url` (presigned), `package_sha256`,
  `package_size`, `iat`, `exp` (≤ now+300 s), `jti`.

- **Agent verifies before WordPress installs anything**, aborting on the first
  failure, in this order:
  1. detached Ed25519 signature over the manifest body;
  2. `aud == siteId()` **and** `cmd == "update_manifest"` **and**
     `slug == "wpmgr-agent"` (constant-time compare);
  3. temporal: `exp > now` (small skew), `jti` single-use (replay table),
     `iat` monotonic (never older than the last accepted manifest);
  4. **downgrade guard:** `semver(version) > on-disk header version`, where the
     on-disk version is read live via `get_plugin_data` — never CP-supplied;
  5. **host allowlist** on `package_url`: scheme `https`, host exact-matches a
     configured allowlist (default `storage.googleapis.com`; a self-hosted
     deployment overrides it via the `WPMGR_AGENT_PACKAGE_HOST` constant or the
     `wpmgr_agent_package_hosts` filter), constant-time, `redirection => 0`. An
     exact-host allowlist inherently rejects literal IPs (incl. the cloud
     metadata IP) and look-alike hosts (anti-SSRF);
  6. **size clamp** to `package_size` during the streamed download, plus an
     absolute ceiling;
  7. **streaming sha256** of the downloaded bytes equals the signed
     `package_sha256`.
  Only then does `WP_Upgrader` swap files.

- **Transport.** Package bytes flow agent ↔ object storage over the short-lived
  presigned HTTPS GET URL, never relayed through the CP. A leaked URL lets an
  attacker *download* the (public, non-secret) plugin zip but **not substitute**
  one — substitution requires matching the signed `package_sha256`.

- **WordPress integration (agent).** Hook `site_transient_update_plugins` to
  inject `$transient->response[...]` when an update exists (and `->no_update[...]`
  when current, so the auto-update toggle renders); `plugins_api` (priority 20,
  3 args) for the “View details” modal; `upgrader_pre_download` for the
  downgrade/host/size/sha256 gate; `upgrader_source_selection` to keep the
  unzipped folder named `wpmgr-agent/`. Cache the verified manifest in
  `set_site_transient('wpmgr_agent_update_manifest', …, 12h)`, flushed on
  `delete_site_transient_update_plugins` and the admin “Check for updates”
  action.

- **AuthZ.** The manifest endpoint sits behind the existing per-site agent auth
  on `/agent/v1`; identity comes from `agent.IdentityFromContext`. `aud` is
  pinned to the requesting site, so even a captured manifest is installable on
  exactly one site.

> **Trade-off (owner-confirmed):** agent releases are **decoupled from API
> deploys**. Shipping a new agent version is a `make agent-release` step that
> uploads the zip + `latest.json` to object storage, independent of any Go API
> deploy. Pro: ship agent fixes without redeploying the API. Con: the upload
> step is a new trust boundary — a bad `latest.json` is a live rollout, mitigated
> by the agent's signature check + downgrade guard + sha256, and by uploading
> `latest.json` **last** (only after the package it references is in place).

---

## 2. Security controls (MUST → enforcement point)

| # | Control | Enforced at |
|---|---------|-------------|
| MUST-1 | CP signs manifest; agent verifies signature before reading any field | CP `Signer.SignManifest` / agent `sodium_crypto_sign_verify_detached` |
| MUST-2 | Required claims present; freshness via `iat`+`exp`+`jti` (not signature alone) | CP sets claims, `exp ≤ now+300 s` / agent re-checks |
| MUST-3 | `jti` single-use (replay) | agent — existing replay table (`wp_wpmgr_agent_jti`) |
| MUST-4 | `package_sha256` in the signed manifest; streaming hash check before file swap | agent `upgrader_pre_download` — `hash_init`/`hash_update` + `hash_equals`, abort with `WP_Error` |
| MUST-5 | Package via short-lived presigned URL from our bucket | CP `PresignGet`, TTL ≤ 300 s |
| MUST-6 | Anti-SSRF host allowlist (https + exact match against a configured allowlist, default `storage.googleapis.com`, no redirects) | agent, before download |
| MUST-6b | CP presigns ONLY the deterministic key `agent-releases/<version>/wpmgr-agent.zip` (no arbitrary in-prefix object) | CP `readLatest` key-pin |
| MUST-7 | Downgrade guard: never install `version ≤ on-disk` even with a valid signature | agent — on-disk via `get_plugin_data` |
| MUST-8 | Endpoint agent-authenticated, per-site; `aud` pins install to one site | CP `/agent/v1` auth + `IdentityFromContext` |
| MUST-9 | `min_version` floor | agent |
| MUST-10 | Zip-slip/traversal-safe extraction; folder renamed to `wpmgr-agent/` | agent `upgrader_source_selection` + `unzip_file` |
| MUST-11 | Size/bomb clamp (`package_size` + absolute max) | agent |
| SHOULD-A | Offline/KMS signing key (currently the request-auth key) | follow-up (see §4) |
| SHOULD-B | Monotonic `iat` / per-`(site,slug)` version counter | agent — persisted site option |
| SHOULD-C | Prefer SHA-384; `kid` for key rotation | Phase 3 decision (ship SHA-256 first) |

---

## 3. Phased plan (STOP gate after each phase)

### Phase 0 — ADR + release tooling
- This ADR (`docs/adr/ADR-042-cp-agent-self-update.md`).
- `scripts/release-agent.sh` + `make agent-release`: build the stable-slug zip,
  compute `sha256` + byte size, write `latest.json`, upload the versioned zip
  then `latest.json` (last) to `gs://wpmgr-chunks-prod/agent-releases/` via
  `gcloud storage cp`. No code path consumes the manifest yet.
- **🛑 Gate 0:** owner confirms the decoupled-release trade-off (done);
  `latest.json` + a test zip are uploadable and readable.

### Phase 1 — CP signed-manifest endpoint
- `apps/api/internal/agent/update_handler.go`: `GET /agent/v1/update/manifest`
  reusing the CP-global `blobstore.Store` + an `agentcmd.Signer`.
- `apps/api/internal/agentcmd/jwt.go`: `const CmdUpdateManifest` + a
  `Signer.SignManifest([]byte) string` (raw detached Ed25519, base64url).
- `apps/api/internal/server/server.go`: add `UpdateAgentH` to `Deps`, register on
  the `/agent/v1` group (the existing `UpdateH` is the unrelated `/api/v1`
  `update.Handler` — keep the names distinct).
- `apps/api/cmd/wpmgr/main.go`: construct the handler reusing `defStore` + a
  signer built like `revokeMinter`; clamp the manifest presign TTL ≤ 300 s.
- Go unit tests (sign/verify, absent `latest.json` → no-update, `aud`/`exp`/`jti`).
- **🛑 Gate 1.**

### Phase 2 — Agent UpdateChecker + admin affordance
- `apps/agent/includes/support/class-update-checker.php` (the WP hooks + the full
  verification chain + 12 h transient cache).
- Wire it in `apps/agent/includes/class-plugin.php`; add a “Check for updates”
  action in `apps/agent/includes/class-admin.php`.
- Agent unit tests (signature/sha/downgrade/host-allowlist/replay).
- **🛑 Gate 2.**

### Phase 3 — Security review + runbook + ship
- Threat-walk T1–T11 against the built code (security-reviewer).
- `docs/runbook/agent-self-update.md` (release, fix-forward rollback, audit).
- Bump `WPMGR_AGENT_VERSION`, `make agent-release`, deploy the API; live e2e.
- **🛑 Gate 3.**

---

## 4. Open risks / follow-ups

- The signing key is the shared request-auth key, not an isolated/KMS key
  (SHOULD-A). Acceptable for v1; a CP web-tier RCE could mint manifests until the
  key is isolated. Tracked as a follow-up.
- Confirm WordPress routes a self-hosted `package` URL through
  `upgrader_pre_download` on the target WP versions (return a verified local path
  there to skip WP's own download).
- `no_update` population is mandatory for the auto-update toggle UI.
- Never log the full `package_url` (it is a bearer credential).
- `make agent-release` is the new trust boundary — the script validates the zip's
  top-folder name and recomputes the sha before upload, and uploads `latest.json`
  last.

## 5. Test plan

- **Agent unit:** valid manifest injects an update; bad signature rejected; sha
  mismatch → `WP_Error` + temp unlinked; downgrade aborts despite a valid
  signature; host allowlist rejects `http://`, look-alike hosts, and
  `169.254.169.254`; expired `exp`/replayed `jti`/non-increasing `iat` rejected;
  12 h cache avoids re-fetch; current version → `no_update`.
- **CP unit:** `SignManifest` verifies under the public key (tamper → fail);
  absent `latest.json` → no-update; `aud` = caller site; `exp ≤ now+300 s`; fresh
  `jti`; presigned URL targets the versioned key with the clamped TTL.
- **Live e2e:** release N+1; dashboard shows the update + details modal; one-click
  installs into `wpmgr-agent/` (no duplicate folder, plugin stays active);
  `WPMGR_AGENT_VERSION == N+1` after; crons survive; a corrupted `package_sha256`
  aborts the install visibly.
