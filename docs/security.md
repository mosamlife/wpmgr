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
