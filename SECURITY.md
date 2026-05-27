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
- **Client-side backup encryption:** backup blobs are encrypted with `age`;
  the control plane must never hold decryption keys without explicit consent.

## Cryptography

Locked algorithms (changes require an ADR): **Ed25519** (agent request
signing), **AES-256-GCM** (at-rest secret encryption), **blake3** (content
addressing / integrity), **age** (backup encryption).

The full threat model lives in [docs/security.md](./docs/security.md).
