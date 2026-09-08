# Security Policy

## Reporting a vulnerability

Please report security issues privately. Do **not** open a public issue.

- Use GitHub's [private vulnerability reporting](https://github.com/mosamlife/wpmgr/security/advisories/new), or
- Email the maintainers (see repo metadata).

We aim to acknowledge within 72 hours and provide a remediation timeline after
triage. Coordinated disclosure is appreciated.

## Threat model (summary)

- **Multi-tenancy:** the control plane is multi-tenant; one tenant must never
  read another's data. Enforced via Postgres Row-Level Security plus
  application-layer `tenant_id` scoping.
- **Untrusted agents:** the agent runs on potentially-compromised WordPress
  sites. All site-supplied data is treated as untrusted and schema-validated.
- **Backup storage:** backups are **not** encrypted client-side in shipped
  builds, so a backup is protected by whatever protects the destination it was
  sent to: that destination's own access controls and encryption at rest. The
  default destination is a WPMgr-managed bucket, so unless a site is pointed
  elsewhere the control plane holds its chunks; a customer-owned S3-compatible
  bucket or a local folder on the WordPress host keeps them on storage the
  operator controls. Uploads to a managed or S3-compatible bucket go over
  HTTPS; a local destination writes to disk on the site's own server and does
  no network transfer. Client-side encryption is the intended model and the
  `age` implementation and per-site key management ship with the agent, but the
  encrypt step is not enabled. The constraint that the control plane must never
  hold a backup decryption key stands.

## Cryptography

Locked algorithms (changes require an ADR): **Ed25519** (agent request
signing), **AES-256-GCM** (at-rest secret encryption), **BLAKE2b-256** (content
addressing / integrity; the code identifier and the wire field name both read
`blake3`), **age** (backup encryption; implemented, not enabled in shipped
builds).

The full threat model lives in [docs/security.md](./docs/security.md).
