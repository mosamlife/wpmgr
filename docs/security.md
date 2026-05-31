# Security

This is the full threat model. For **reporting a vulnerability**, follow the
private disclosure process in [SECURITY.md](../SECURITY.md) at the repo root — do
not open a public issue.

## Threat model

### Multi-tenant isolation

The control plane is multi-tenant: one tenant must never read another's data.
Enforced with **Postgres Row-Level Security (RLS)** plus application-layer
`tenant_id` scoping — defense in depth so an application-layer bug alone can't
leak across tenants.

### Untrusted agents

The agent (`apps/agent`) runs on **potentially-compromised WordPress hosts**. The
control plane therefore treats every agent as untrusted:

- All agent-supplied data is schema-validated before use.
- Agent identity is bound to an Ed25519 public key registered at enrollment;
  requests are signature-verified.
- Outbound calls from the control plane to agents go through an
  **SSRF-hardened HTTP transport** that pins the resolved IP and rejects
  private/link-local/loopback ranges at dial time (ADR-009), defeating
  DNS-rebinding via user-controlled site URLs.

### Client-side backup encryption

Backup blobs are encrypted with **age**. The control plane must never hold
decryption keys without the user's explicit consent — by default, encryption is
client-side and the server stores only ciphertext.

### Agent transport

All agent ↔ control-plane requests are **Ed25519-signed** in both directions and
verified against keys exchanged at enrollment. The agent sends **no third-party
telemetry**. See [agent.md](./agent.md).

### Media Optimizer

The Media Optimizer (M23) decodes and re-encodes **untrusted images** uploaded to
WordPress sites. Threat surface and mitigations:

- **Untrusted-image handling.** The source format is detected from **magic
  bytes** by `lilliput`, never the agent-claimed mime. Only `image/jpeg` and
  `image/png` decode; anything else is rejected (`ErrUnsupportedSource`) and
  recorded `excluded`. The decoder is guarded by **50 MB** and **100 MP** limits
  (`ErrDimensionsTooBig`) and a **60s per-encode timeout** (`ErrEncoderTimeout`).
- **Blast-radius isolation.** All decode/encode runs in the **optional
  `media-encoder` container** (CGO + native codec libs), never in the static main
  API. The `media_encode` River queue is bounded (small `MaxWorkers`) so a burst
  of large images can't OOM the instance, and the encoder process is the only
  place native codec CVE surface exists — a self-hoster who doesn't run the
  `media` profile has none of it.
- **No media bytes on the control plane.** Bytes move agent ↔ object storage over
  **presigned URLs** (the backup transport, ADR-033); the CP never streams or
  persists source or optimized image bytes, and never calls a live `GetObject`
  (it 403s on GCS). The CP holds metadata + audit rows only; thumbnails load from
  the site's own public URLs. Presigned URLs are bearer credentials and are never
  logged.
- **Tenant isolation + the delete-originals gate.** All three media tables are
  `FORCE ROW LEVEL SECURITY` (tenant-isolation + `app.agent` worker policies).
  `sync`/`optimize`/`restore` gate on `site:write` (operator+); the
  **irreversible** delete-originals gates on `media:delete_originals` (admin+)
  plus a type-the-hostname UI confirmation, and the consenting actor is recorded
  in the hash-chained audit log. Every agent callback re-asserts the job to the
  agent's proven tenant+site (from the verified key, never a header) before any
  mutation; every by-id route nests under `/sites/{siteId}/...` so per-site access
  is always enforced.
- **Serialized-safe DB rewrite.** The on-site URL rewrite (de)serialize-round-
  trips serialized PHP (preserving `s:NN:` length prefixes), JSON-decodes
  page-builder meta, honours a core/optimizer-owned meta skip-list, and uses a
  trailing boundary lookahead — so a rewrite can't silently corrupt content or
  the restore anchor.
- **Idempotent `.htaccess`.** The Accept-fallback block is written between
  `# BEGIN/END WPMgr Media` markers and replaces (never appends), so repeated
  installs leave exactly one block; the `-f` existence guard means a missing twin
  never 404s.

> **Phase 6 security review: PASS-WITH-NITS.** The two material findings — a
> use-after-free in the encoder's native buffer handling and a presigned-key
> validation gap — were fixed and re-verified. The remaining nits were advisory.

## Cryptography

Locked algorithms — **changing any requires an ADR**:

| Algorithm | Use |
|-----------|-----|
| **Ed25519** | Agent request signing (both directions) |
| **AES-256-GCM** | At-rest secret encryption |
| **blake3** | Content addressing / integrity |
| **age** | Backup encryption (client-side) |

## Disclosure

Report privately via the process in [SECURITY.md](../SECURITY.md):

- GitHub private vulnerability reporting, or email the maintainers.
- Acknowledgement targeted within 72 hours; remediation timeline after triage.
- Coordinated disclosure appreciated.

Security-sensitive PRs (auth, crypto, agent protocol, RBAC, tenant isolation)
must be flagged for review — see [contributing.md](./contributing.md).
