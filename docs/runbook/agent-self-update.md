# Runbook — Agent self-update (ADR-042)

How to ship a new WPMgr agent version through the CP-driven self-update channel,
how rollback works, and how to verify + audit. See `docs/adr/ADR-042-cp-agent-self-update.md`
for the design + security model.

## Mental model

- Agent release zips + a `latest.json` pointer live in object storage under
  `agent-releases/`. The **release pipeline writes** them; the **CP only reads**.
- The agent asks the CP (`GET /agent/v1/update/manifest`, signed request) for a
  **signed** manifest, which the CP builds from `latest.json` with a short-lived
  presigned download URL. The agent verifies the signature + a chain of checks
  (aud, downgrade guard, host allowlist, sha256) before WordPress installs.
- Result: the WP dashboard shows **"Update available"** and the operator updates
  with one click — no manual zip upload.

## Releasing a new version

1. **Bump the version** in `apps/agent/wpmgr-agent.php` (both the header `Version:`
   and `define('WPMGR_AGENT_VERSION', ...)`).
   - ⚠️ **Always bump the NUMERIC core** (`0.10.5` → `0.10.6`). The agent's
     downgrade guard compares the *normalised* bare-semver core, so two releases
     that share a numeric core but differ only in the `-suffix`
     (`0.10.6-a` vs `0.10.6-b`) are NOT seen as an update. The suffix is
     descriptive only.

2. **Publish** to object storage:
   ```sh
   make agent-release            # builds the stable-slug zip, sha256, latest.json, uploads
   # or preview without uploading:
   make agent-release-dry-run
   ```
   `scripts/release-agent.sh` uploads the **versioned package first**, then
   `latest.json` **last**, so the pointer never references a package that is not
   yet in place. Override the target with `WPMGR_RELEASE_BUCKET` /
   `WPMGR_RELEASE_PREFIX` (defaults: `wpmgr-chunks-prod` / `agent-releases`).

3. **Deploy the API** the first time only (the `GET /agent/v1/update/manifest`
   handler). Subsequent agent releases are decoupled — they are a `make
   agent-release` step with no API redeploy.

Within ~12h (the agent's manifest cache TTL), or immediately if the operator
clicks **Check for updates** (agent admin) / **Check again** (WP → Plugins →
Updates), every enrolled site sees the new version and can one-click update.

## Rollback = fix-forward

The agent's downgrade guard refuses to install a version whose numeric core is
`<=` the installed one, **even from a validly signed manifest**. So you cannot
"roll back" by re-pointing `latest.json` at an older version — agents will ignore
it. To undo a bad release, **ship N+2 with the fix** (a higher numeric core) and
`make agent-release` again. The old versioned package stays in object storage for
forensics; only `latest.json` moves.

## Self-hosted deployments (non-GCS object storage)

The agent's package-host allowlist defaults to `storage.googleapis.com`. A
self-hosted deployment whose object storage lives elsewhere (MinIO / SeaweedFS /
managed S3) must tell the agent the expected host, either:

- define a constant in `wp-config.php`:
  `define('WPMGR_AGENT_PACKAGE_HOST', 's3.example.com');` (comma-separated for
  multiple), or
- a `wpmgr_agent_package_hosts` filter returning an array of allowed hosts.

Keep this in sync with the CP's `WPMGR_S3_ENDPOINT` host. The download must be
HTTPS.

## Verify a release end-to-end

On a staging site running version N:
1. `make agent-release` for N+1.
2. WP → Plugins shows **WPMgr Agent — update available**; **View details** renders.
3. Click **Update Now** → installs into `wp-content/plugins/wpmgr-agent/` (no
   versioned-duplicate folder; plugin stays active).
4. Confirm `WPMGR_AGENT_VERSION == N+1` after.
5. Confirm crons survive (the heartbeat/reporting events are still scheduled; the
   `plugins_loaded` cron self-heal re-arms them regardless).
6. Negative path: temporarily publish a `latest.json` whose `package_sha256` is
   corrupted → the update must abort visibly with **"WPMgr update integrity
   check failed."** and leave the installed version untouched.

## Security / audit notes

- The manifest is signed with the CP's Ed25519 key (the same key as `revoke`/
  autologin tokens). **Follow-up (SHOULD-A):** isolate this signing key (KMS /
  offline) so a CP web-tier compromise cannot mint manifests. Until then, a CP
  compromise can sign arbitrary manifests — same trust level as the existing
  command channel.
- `make agent-release` is the new trust boundary. The script validates the zip's
  top-level slug (`wpmgr-agent/`) and recomputes the sha256 from the zip before
  upload. Treat write access to `gs://<bucket>/agent-releases/` as release-signing
  authority.
- Never log the presigned `package_url` (it is a bearer credential); both the CP
  handler and the agent are written to avoid it.
- Each accepted manifest is single-use (`jti` replay table) and monotonic by
  `iat`; a leaked manifest is `aud`-pinned to one site and cannot be downgraded.
