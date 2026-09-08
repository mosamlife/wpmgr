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
  builds. Chunks travel over TLS and are stored as uploaded at the destination
  configured for the site, so the security of a backup is the security of that
  destination. Client-side encryption is the intended model and the `age`
  implementation and per-site key management ship with the agent, but the
  encrypt step is not enabled. The constraint that the control plane must never
  hold a backup decryption key stands.

## Cryptography

Locked algorithms (changes require an ADR): **Ed25519** (agent request
signing), **AES-256-GCM** (at-rest secret encryption), **BLAKE2b-256** (content
addressing / integrity; the code identifier and the wire field name both read
`blake3`), **age** (backup encryption; implemented, not enabled in shipped
builds).

The full threat model lives in [docs/security.md](./docs/security.md).
