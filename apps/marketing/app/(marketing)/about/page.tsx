// About page: open-source mission, AGPL + MIT licensing, privacy-first.
// Trust-building, brand voice. ContentPage archetype.
import type { Metadata } from "next";
import Link from "next/link";
import { Icon } from "@/components/ui/icon";
import { Container, Section, Card } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { CTABand } from "@/components/sections/cta-band";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "About WPMgr | Open-Source WordPress Fleet Management",
  description:
    "WPMgr is an open-source, self-hostable WordPress fleet manager built on the conviction that your site data and your tooling should belong to you. AGPL control plane, MIT agent, Ed25519-signed messages.",
  canonical: "/about",
});

const PRINCIPLES = [
  {
    icon: "FileSearch",
    title: "Readable by design",
    body: "Every line of the control plane is open under the AGPL. The WordPress agent is MIT-licensed. You can read every function, every migration, and every message format before you trust the software with your sites.",
  },
  {
    icon: "ServerCog",
    title: "Your infrastructure, your data",
    body: "WPMgr runs on servers you choose. Fleet data, backups, and diagnostic logs never leave your infrastructure unless you explicitly configure a remote destination. The hosted cloud version, when it launches, will have the same open codebase.",
  },
  {
    icon: "KeyRound",
    title: "Cryptographically verifiable",
    body: "Every command the control plane sends to a WordPress site is signed with an Ed25519 key. The agent verifies the signature before executing any instruction. You can verify this in the source code and in the agent's verification log.",
  },
  {
    icon: "RotateCcw",
    title: "Reversible by default",
    body: "Features that modify a site, such as image format conversion, caching rules, or security hardening, are designed to be reversed with a single click. Originals are archived, not deleted. No feature creates a dependency you cannot remove.",
  },
  {
    icon: "Scale",
    title: "AGPL + MIT: no ambiguity",
    body: "The control plane is AGPL-3.0 so you can read, run, fork, and contribute back. The WordPress agent is MIT so you can use it in any project without license friction. Both choices are deliberate and permanent.",
  },
  {
    icon: "ShieldCheck",
    title: "Privacy off by default",
    body: "Diagnostics, performance telemetry, and Real User Monitoring are all opt-in or scoped to what the WordPress site already collects. WPMgr does not phone home, does not collect usage analytics, and does not require an account to self-host.",
  },
];

const LICENSING = [
  {
    label: "Control plane",
    value: "AGPL-3.0",
    desc: "Self-host, fork, and modify. If you distribute a modified version as a network service, you publish the source.",
    icon: "Scale",
  },
  {
    label: "WordPress agent",
    value: "MIT",
    desc: "Fully permissive. Use it in any project, proprietary or open, without restriction.",
    icon: "FileBadge",
  },
  {
    label: "Agent signing",
    value: "Ed25519",
    desc: "Every release is signed. Every runtime command is signed. Verification is in the code.",
    icon: "KeyRound",
  },
];

export default function AboutPage() {
  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "About", href: "/about/" },
  ]);

  return (
    <>
      <nav aria-label="Breadcrumb" className="py-4">
        <Container>
          <ol className="flex flex-wrap items-center gap-1.5 text-sm text-[var(--muted-foreground)]">
            <li>
              <Link href="/" className="transition-colors hover:text-foreground">
                Home
              </Link>
            </li>
            <li aria-hidden className="select-none">/</li>
            <li className="text-foreground font-medium" aria-current="page">
              About
            </li>
          </ol>
        </Container>
      </nav>

      {/* Hero: not JS-animated */}
      <section
        className="relative overflow-hidden py-20 sm:py-24 lg:py-28"
        aria-label="About hero"
      >
        <div aria-hidden className="dot-field pointer-events-none absolute inset-0" />
        <Container className="relative">
          <div className="mx-auto max-w-3xl">
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl leading-[1.1]">
              Built on the conviction that your tooling should belong to you
            </h1>
            <p className="mt-6 text-lg leading-relaxed text-[var(--muted-foreground)]">
              WPMgr is an open-source WordPress fleet manager. We built it because managing WordPress sites across a portfolio should not require trusting a proprietary SaaS with your clients' data, your backup archives, or your site credentials.
            </p>
            <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
              The full control plane is open under the AGPL. The WordPress agent is MIT-licensed. Every message the agent sends is Ed25519-signed. You can read every line of code before you run any of it.
            </p>
            <div className="mt-8 flex flex-wrap items-center gap-3">
              <Link
                href={SITE_CONFIG.github}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-2 rounded-[var(--radius)] bg-primary px-6 py-3 text-base font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                <Icon name="Github" size={18} />
                Read the code on GitHub
              </Link>
              <Link
                href="/features/"
                className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-6 py-3 text-base font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                See what it does
                <Icon name="ArrowRight" size={18} />
              </Link>
            </div>
          </div>
        </Container>
      </section>

      {/* Principles */}
      <Section tone="muted" id="principles">
        <Container>
          <Reveal>
            <div className="mx-auto max-w-2xl">
              <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                The principles we build to
              </h2>
              <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                These are not aspirations. Each one is enforced in the codebase, in the license choice, and in the product decisions we make.
              </p>
            </div>
          </Reveal>
          <Stagger className="mt-10 grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
            {PRINCIPLES.map((p) => (
              <StaggerItem key={p.title}>
                <Card className="flex h-full flex-col gap-3">
                  <span className="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                    <Icon name={p.icon} size={20} />
                  </span>
                  <h3 className="text-base font-semibold text-foreground">{p.title}</h3>
                  <p className="text-sm leading-relaxed text-[var(--muted-foreground)]">{p.body}</p>
                </Card>
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* Licensing */}
      <Section id="licensing">
        <Container>
          <Reveal>
            <div className="mx-auto max-w-2xl">
              <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                Licensing: no ambiguity
              </h2>
              <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                We chose licenses that make the terms unambiguous and that make forks and contributions straightforward. These choices are permanent.
              </p>
            </div>
          </Reveal>
          <Stagger className="mt-10 grid gap-5 sm:grid-cols-3">
            {LICENSING.map((l) => (
              <StaggerItem key={l.label}>
                <Card className="flex flex-col gap-3">
                  <div className="flex items-center gap-3">
                    <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                      <Icon name={l.icon} size={18} />
                    </span>
                    <div>
                      <p className="text-xs font-medium text-[var(--muted-foreground)] uppercase tracking-wide">{l.label}</p>
                      <p className="text-base font-semibold text-foreground font-mono">{l.value}</p>
                    </div>
                  </div>
                  <p className="text-sm leading-relaxed text-[var(--muted-foreground)]">{l.desc}</p>
                </Card>
              </StaggerItem>
            ))}
          </Stagger>
          <Reveal delay={0.1}>
            <div className="mt-8 flex flex-wrap gap-3">
              <Link
                href={`${SITE_CONFIG.github}/blob/main/LICENSE`}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-2 text-sm font-medium text-[var(--primary)] hover:underline"
              >
                Read the AGPL license
                <Icon name="ExternalLink" size={13} />
              </Link>
              <span className="text-[var(--border)]" aria-hidden>|</span>
              <Link
                href={`${SITE_CONFIG.github}/blob/main/agent/LICENSE`}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-2 text-sm font-medium text-[var(--primary)] hover:underline"
              >
                Read the MIT agent license
                <Icon name="ExternalLink" size={13} />
              </Link>
            </div>
          </Reveal>
        </Container>
      </Section>

      {/* Open source and contributing */}
      <Section tone="muted" id="open-source">
        <Container>
          <Reveal>
            <div className="grid gap-10 lg:grid-cols-2 lg:items-center">
              <div>
                <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                  Open source means open to contribution
                </h2>
                <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                  Good-first-issue labels are maintained on the repository. The architecture decision records are committed alongside the code so you can follow the reasoning behind every major choice without asking. The contributing guide explains how to set up a local dev environment, how to write tests, and how to open a pull request.
                </p>
                <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                  Issues that affect self-hosted users are treated the same as issues that affect the cloud product. There is no premium support tier for open-source users.
                </p>
                <div className="mt-6 flex flex-wrap gap-3">
                  <Link
                    href={`${SITE_CONFIG.github}/blob/main/docs/contributing.md`}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="inline-flex items-center gap-2 rounded-[var(--radius)] bg-primary px-5 py-2.5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    Contributing guide
                    <Icon name="ArrowRight" size={16} />
                  </Link>
                  <Link
                    href={`${SITE_CONFIG.github}/issues?q=is%3Aopen+label%3A%22good+first+issue%22`}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    Good first issues
                    <Icon name="ExternalLink" size={14} />
                  </Link>
                </div>
              </div>
              <div className="space-y-4">
                {[
                  { icon: "GitPullRequest", label: "Pull requests open", desc: "Fork the repository and send a PR. Reviews are constructive." },
                  { icon: "BookOpen", label: "Architecture decision records", desc: "Every major design choice has an ADR so you know why things are built the way they are." },
                  { icon: "Tag", label: "Semantic versioning", desc: "Every release is tagged and has curated release notes. The changelog is committed to the repository." },
                ].map((item) => (
                  <div key={item.label} className="flex gap-4 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm">
                    <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                      <Icon name={item.icon} size={18} />
                    </span>
                    <div>
                      <p className="text-sm font-semibold text-foreground">{item.label}</p>
                      <p className="mt-1 text-sm leading-relaxed text-[var(--muted-foreground)]">{item.desc}</p>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          </Reveal>
        </Container>
      </Section>

      {/* Changelog / what shipped */}
      <Section id="changelog">
        <Container>
          <Reveal>
            <div className="flex flex-col gap-6 sm:flex-row sm:items-center sm:justify-between">
              <div className="max-w-xl">
                <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                  Shipped regularly, documented clearly
                </h2>
                <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                  WPMgr releases follow a predictable cadence. Every change is in the changelog, every version is tagged on GitHub, and production deployments happen through the same public repository.
                </p>
              </div>
              <div className="shrink-0 flex flex-col gap-3">
                <Link
                  href="/changelog/"
                  className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-3 text-sm font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  Read the changelog
                  <Icon name="ArrowRight" size={14} />
                </Link>
                <Link
                  href={`${SITE_CONFIG.github}/releases`}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex items-center gap-2 text-sm font-medium text-[var(--primary)] hover:underline"
                >
                  GitHub releases
                  <Icon name="ExternalLink" size={13} />
                </Link>
              </div>
            </div>
          </Reveal>
        </Container>
      </Section>

      <CTABand
        heading="Run it yourself. Read every line."
        subhead="Self-host the complete control plane on your own infrastructure. No per-site fee, no data sent to a third party, and no features behind a paywall."
        ctas={[
          { label: "Get started for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
          { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
        ]}
      />

      <JsonLd data={breadcrumbLd} />
    </>
  );
}
