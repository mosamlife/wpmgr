import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "Security Policy: Responsible Disclosure",
  description:
    "WPMgr responsible disclosure policy and security posture: Ed25519-signed agent, redacted diagnostics, client-side-encrypted backups, and how to report a bug.",
  canonical: "/legal/security-policy",
});

const SECURITY_POSTURE = [
  {
    icon: "KeyRound",
    title: "Ed25519-signed agent",
    description:
      "Every WPMgr agent release is signed with an Ed25519 key. The control plane verifies the signature on every auto-update before applying it, so a compromised distribution channel cannot silently push malicious code to your sites.",
  },
  {
    icon: "Eye",
    title: "Redacted diagnostics",
    description:
      "Agent diagnostics (sent to the control plane on enrollment and on schedule) never include passwords, secret keys, or user data. The redaction logic is open-source and auditable.",
  },
  {
    icon: "Lock",
    title: "Client-side-encrypted backups",
    description:
      "Backup data is encrypted on the agent before it leaves the site. The encryption key is derived from a per-site secret managed by the control plane and never stored in plaintext on the backup destination.",
  },
  {
    icon: "ShieldOff",
    title: "Minimal footprint",
    description:
      "The MIT-licensed agent has no persistent database connections, no always-on daemons, and scales to zero. It activates for a scheduled task or a signed control-plane command and then exits.",
  },
  {
    icon: "FileSearch",
    title: "Open-source and auditable",
    description:
      "Both the AGPL-3.0 control plane and the MIT agent are publicly available on GitHub. You can read, audit, fork, and run every line of code on your infrastructure.",
  },
  {
    icon: "UserCheck",
    title: "Tenant isolation",
    description:
      "Every tenant's data is isolated at the database level with row-level security policies. Queries are parameterized throughout. No cross-tenant data leakage by design.",
  },
];

export default function SecurityPolicyPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Legal", href: "/legal" },
    { name: "Security Policy", href: "/legal/security-policy" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <nav
            aria-label="Breadcrumb"
            className="mb-5 flex flex-wrap items-center gap-2 text-sm text-[var(--muted-foreground)]"
          >
            <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
            <span aria-hidden>/</span>
            <Link href="/legal" className="hover:text-foreground transition-colors">Legal</Link>
            <span aria-hidden>/</span>
            <span className="text-foreground">Security Policy</span>
          </nav>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Security Policy
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              Responsible Disclosure
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              We build WPMgr to be secure by design: open source, minimal footprint, signed
              releases, and privacy-first defaults. If you find a vulnerability, we want to hear
              from you.
            </p>
          </div>
        </Container>
      </section>

      <Section>
        <Container className="max-w-4xl">
          <div className="prose-custom space-y-10">

            {/* Responsible disclosure */}
            <div>
              <h2 className="text-2xl font-semibold text-foreground">How to report a vulnerability</h2>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                If you believe you have found a security vulnerability in WPMgr, please report it
                responsibly by emailing{" "}
                <a
                  href="mailto:security@wpmgr.app"
                  className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  security@wpmgr.app
                </a>
                . Do not open a public GitHub issue for security vulnerabilities.
              </p>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                Please include:
              </p>
              <ul className="mt-3 list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
                <li className="leading-7">A description of the vulnerability and its potential impact.</li>
                <li className="leading-7">Steps to reproduce, including any proof-of-concept code if applicable.</li>
                <li className="leading-7">The component affected (control plane, dashboard, agent, hosted service, or a specific API endpoint).</li>
                <li className="leading-7">Whether you have already disclosed this to any third party.</li>
              </ul>
            </div>

            {/* What to expect */}
            <div>
              <h2 className="text-2xl font-semibold text-foreground">What to expect</h2>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                We aim to acknowledge reports within two business days. For confirmed vulnerabilities:
              </p>
              <ul className="mt-3 list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
                <li className="leading-7">We will work with you to understand the scope and validate the issue.</li>
                <li className="leading-7">
                  We will aim to release a fix within 30 days for critical or high-severity issues.
                  Complex issues may take longer; we will communicate the expected timeline.
                </li>
                <li className="leading-7">
                  We will credit you in the release notes and changelog if you would like public
                  attribution.
                </li>
                <li className="leading-7">
                  We ask that you do not publicly disclose the vulnerability until a fix has been
                  released, or until we have agreed on a coordinated disclosure date.
                </li>
              </ul>
            </div>

            {/* Scope */}
            <div>
              <h2 className="text-2xl font-semibold text-foreground">Scope</h2>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                In scope for this policy:
              </p>
              <ul className="mt-3 list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
                <li className="leading-7">
                  The WPMgr control plane (Go backend, source at{" "}
                  <a
                    href={SITE_CONFIG.github}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    github.com/mosamlife/wpmgr
                  </a>
                  )
                </li>
                <li className="leading-7">The WPMgr dashboard (TypeScript/React frontend)</li>
                <li className="leading-7">The WordPress agent (PHP, MIT-licensed)</li>
                <li className="leading-7">The hosted service at manage.wpmgr.app</li>
                <li className="leading-7">
                  The API surface documented at{" "}
                  <a
                    href={SITE_CONFIG.docs}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    manage.wpmgr.app/docs/
                  </a>
                </li>
              </ul>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                Out of scope: social engineering, physical attacks, denial-of-service attacks,
                vulnerabilities in third-party services integrated with WPMgr (report those to the
                relevant vendor), and issues in WordPress sites managed by WPMgr that are not caused
                by WPMgr itself.
              </p>
            </div>

            {/* Security posture */}
            <div>
              <h2 className="text-2xl font-semibold text-foreground">Security posture</h2>
              <p className="mt-4 leading-7 text-[var(--muted-foreground)]">
                WPMgr is designed with security as a first-class concern. Key design decisions:
              </p>
            </div>
          </div>

          {/* Posture cards */}
          <ul className="mt-8 grid gap-5 sm:grid-cols-2 lg:grid-cols-3" role="list">
            {SECURITY_POSTURE.map((item) => (
              <li
                key={item.title}
                className="rounded-xl border border-[var(--border)] bg-card p-5"
              >
                <div className="mb-3 inline-flex h-9 w-9 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                  <Icon name={item.icon as Parameters<typeof Icon>[0]["name"]} size={18} />
                </div>
                <h3 className="font-semibold text-foreground">{item.title}</h3>
                <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">
                  {item.description}
                </p>
              </li>
            ))}
          </ul>

          {/* Dashboard 2FA posture */}
          <div className="mt-10 space-y-6">
            <h2 className="text-2xl font-semibold text-foreground">Dashboard account security</h2>
            <p className="leading-7 text-[var(--muted-foreground)]">
              The WPMgr dashboard is the single front door to every site you manage. Operator accounts
              are protected with:
            </p>
            <ul className="list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
              <li className="leading-7">
                Two-factor authentication: TOTP (authenticator app) and WebAuthn/FIDO2 (passkeys and
                hardware security keys).
              </li>
              <li className="leading-7">
                Recovery codes: one-time codes generated at 2FA setup, stored hashed and single-use.
              </li>
              <li className="leading-7">
                Trusted devices: remember-this-device token, revocable per device, cleared on
                password change.
              </li>
              <li className="leading-7">
                Rate-limited login: failed attempts are counted and locked out per account and per IP.
              </li>
              <li className="leading-7">
                Tamper-evident audit log: every session, login, and security event is written to a
                hash-chained audit log that cannot be silently modified.
              </li>
            </ul>

            <h2 className="text-2xl font-semibold text-foreground">Agent security</h2>
            <p className="leading-7 text-[var(--muted-foreground)]">
              The MIT-licensed WordPress agent that runs on each managed site is designed to be
              auditable, minimal, and safe to run on production servers:
            </p>
            <ul className="list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
              <li className="leading-7">
                All agent-to-control-plane communication is over HTTPS with certificate pinning on
                the control-plane side.
              </li>
              <li className="leading-7">
                Control-plane commands to the agent are signed. The agent verifies the signature before
                executing any command.
              </li>
              <li className="leading-7">
                Autologin tokens are single-use and expire. They are signed by the control plane and
                verified by the agent without a round trip.
              </li>
              <li className="leading-7">
                The agent does not write mu-plugin helpers unless you explicitly enable a feature that
                requires them. A freshly activated agent writes nothing outside its own plugin folder.
              </li>
              <li className="leading-7">
                Auto-updates verify the Ed25519 signature of the update package before applying it.
              </li>
            </ul>

            <h2 className="text-2xl font-semibold text-foreground">Data privacy</h2>
            <p className="leading-7 text-[var(--muted-foreground)]">
              WPMgr is privacy-first and off-by-default:
            </p>
            <ul className="list-disc pl-6 space-y-2 text-[var(--muted-foreground)]">
              <li className="leading-7">
                Diagnostic data sent to the control plane never includes passwords, secret keys,
                customer email addresses, order data, or other personal information. The redaction
                logic is in the open-source agent.
              </li>
              <li className="leading-7">
                Real User Monitoring (Core Web Vitals) collects only performance metrics, no
                personally identifiable information. No session replay, no visitor fingerprinting.
              </li>
              <li className="leading-7">
                Backup data is encrypted on the agent before leaving the site. The control plane
                stores a per-site encryption key reference; the backup destination stores only
                ciphertext.
              </li>
              <li className="leading-7">
                Email logs record metadata (from, to domain, subject, status) but not message bodies.
                Body storage is configurable and off by default.
              </li>
            </ul>
          </div>

          {/* CTA */}
          <div className="mt-12 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6">
            <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <p className="font-medium text-foreground">Found something?</p>
                <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                  Report it privately to{" "}
                  <a
                    href="mailto:security@wpmgr.app"
                    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    security@wpmgr.app
                  </a>
                  . We investigate all credible reports.
                </p>
              </div>
              <Link
                href="/contact"
                className="inline-flex items-center justify-center rounded-[var(--radius)] bg-primary px-5 py-2.5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] whitespace-nowrap"
              >
                Contact us
              </Link>
            </div>
          </div>
        </Container>
      </Section>
    </>
  );
}
