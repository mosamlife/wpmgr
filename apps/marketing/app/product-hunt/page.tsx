import type { Metadata } from "next";
import { buildMetadata } from "@/lib/seo";
import { Container, Section, SectionHeading, Card, Eyebrow } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "WPMgr on Product Hunt",
  description: "WPMgr is live on Product Hunt. Open-source, self-hostable WordPress fleet management.",
  canonical: "/product-hunt",
  noindex: true,
});

const CAPABILITIES = [
  {
    icon: "DatabaseBackup",
    title: "Incremental backups with point-in-time restore",
    desc: "Scheduled backups run before every update. If something breaks, one click restores to any snapshot without taking the site offline.",
  },
  {
    icon: "ImageDown",
    title: "Cloud Media Optimizer: AVIF and WebP",
    desc: "Re-encodes your full WordPress media library to AVIF and WebP in a dedicated cloud encoder. Originals stay archived on the site, fully reversible.",
  },
  {
    icon: "ShieldCheck",
    title: "Security suite: hardening, file integrity, and vulnerability scanning",
    desc: "Hardening rules, IP ban lists, file integrity monitoring, and CVE scanning via Wordfence Intelligence, with site-user 2FA enforcement per role.",
  },
  {
    icon: "Gauge",
    title: "Full-page caching and Core Web Vitals from real visitors",
    desc: "Full-page cache with WooCommerce-aware bypasses, unused CSS removal, Redis object cache, and p75 Core Web Vitals from your actual traffic.",
  },
  {
    icon: "RefreshCw",
    title: "Safe fleet updates with auto-snapshot and auto-revert",
    desc: "Bulk update plugins, themes, and core across the fleet. A snapshot is taken before each update and automatically reverted if the site breaks.",
  },
  {
    icon: "ScrollText",
    title: "White-label client reports and per-site email routing",
    desc: "Branded maintenance reports by email or PDF, on schedule or on demand. Route each client site through SES, SendGrid, Mailgun, Postmark, or SMTP.",
  },
] as const;

const OPEN_SOURCE_POINTS = [
  {
    icon: "GitFork",
    title: "AGPL-3.0 control plane",
    desc: "The entire control plane is open-source under AGPL-3.0. Read every line, fork it, contribute to it.",
  },
  {
    icon: "Scale",
    title: "MIT-licensed agent",
    desc: "The WordPress agent that runs on your sites is MIT-licensed. No vendor lock-in, no black boxes on your servers.",
  },
  {
    icon: "KeyRound",
    title: "Ed25519-signed agent releases",
    desc: "Every agent release is cryptographically signed. The control plane verifies the signature before trusting any agent payload.",
  },
  {
    icon: "BadgeCheck",
    title: "Reviewed and listed on WordPress.org",
    desc: "The agent passed the WordPress.org plugin review and is published in the official directory as Fleet Agent Site Manager, so you can install it from your own wp-admin.",
  },
] as const;

// Product Hunt landing. noindex per spec (PH thank-you variant).
export default function ProductHuntPage() {
  return (
    <>
      <SiteHeader />
      <main>
        {/* Hero */}
        <section className="border-b border-[var(--border)] py-24 sm:py-32">
          <Container>
            <div className="mx-auto max-w-3xl text-center">
              <div className="mb-5">
                <Eyebrow>Product Hunt launch</Eyebrow>
              </div>
              <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl lg:text-6xl">
                WordPress fleet management, open-source and self-hosted
              </h1>
              <p className="mt-6 text-lg leading-relaxed text-[var(--muted-foreground)] max-w-[60ch] mx-auto">
                WPMgr is a self-hostable control plane for WordPress site operators, developers, and agencies. Back up, restore, optimize images, monitor uptime, harden security, and push safe bulk updates across every site you manage, from one dashboard on infrastructure you own.
              </p>
              <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
                <a
                  href={SITE_CONFIG.signup}
                  className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-7 text-base font-medium text-[var(--primary-foreground)] shadow-sm hover:bg-[var(--primary-hover)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  Get started for free
                  <Icon name="ArrowRight" size={18} />
                </a>
                <a
                  href={SITE_CONFIG.github}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-7 text-base font-medium text-foreground hover:bg-[var(--accent)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  <Icon name="Github" size={18} />
                  Star on GitHub
                </a>
                <a
                  href="/features/"
                  className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-7 text-base font-medium text-foreground hover:bg-[var(--accent)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  See features
                </a>
              </div>
            </div>
          </Container>
        </section>

        {/* What shipped */}
        <Section>
          <Container>
            <Reveal>
              <SectionHeading
                eyebrow="What shipped"
                title="A full WordPress operations platform"
                lead="Six capability areas, all in one open-source control plane. No per-site fee, no artificial limits."
              />
            </Reveal>
            <Stagger className="mt-14 grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
              {CAPABILITIES.map((cap) => (
                <StaggerItem key={cap.title}>
                  <Card className="flex h-full flex-col gap-4">
                    <div className="inline-flex h-11 w-11 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                      <Icon name={cap.icon} size={22} />
                    </div>
                    <div className="flex-1">
                      <h2 className="text-base font-semibold leading-snug text-foreground">
                        {cap.title}
                      </h2>
                      <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">
                        {cap.desc}
                      </p>
                    </div>
                  </Card>
                </StaggerItem>
              ))}
            </Stagger>
          </Container>
        </Section>

        {/* Why it is different: open-source angle */}
        <Section tone="muted">
          <Container>
            <div className="grid gap-12 lg:grid-cols-2 lg:items-center">
              <Reveal>
                <div>
                  <Eyebrow>Why it is different</Eyebrow>
                  <h2 className="mt-4 text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
                    Read every line. Run it yourself. Contribute.
                  </h2>
                  <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
                    Most WordPress management tools are closed black boxes on shared infrastructure. WPMgr is the opposite: the control plane and agent are fully open-source, and you can run the entire stack on servers you own and audit. No phone-home telemetry, no image bytes on vendor servers, no runtime surprises.
                  </p>
                  <p className="mt-4 text-[var(--muted-foreground)]">
                    The agent that runs on your WordPress sites is MIT-licensed. Every agent release is Ed25519-signed. The control plane is AGPL-3.0. The code is on GitHub today.
                  </p>
                  <div className="mt-8 flex flex-wrap gap-3">
                    <a
                      href={SITE_CONFIG.github}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                    >
                      <Icon name="Github" size={16} />
                      View the source
                    </a>
                    <a
                      href={`${SITE_CONFIG.github}/blob/main/docs/contributing.md`}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                    >
                      <Icon name="GitPullRequest" size={16} />
                      Contribute
                    </a>
                  </div>
                </div>
              </Reveal>

              <Stagger className="flex flex-col gap-4">
                {OPEN_SOURCE_POINTS.map((point) => (
                  <StaggerItem key={point.title}>
                    <div className="flex gap-4 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm">
                      <div className="mt-0.5 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary)]/10 text-[var(--primary)]">
                        <Icon name={point.icon} size={18} />
                      </div>
                      <div>
                        <p className="text-sm font-semibold text-foreground">{point.title}</p>
                        <p className="mt-1 text-sm leading-relaxed text-[var(--muted-foreground)]">{point.desc}</p>
                      </div>
                    </div>
                  </StaggerItem>
                ))}
              </Stagger>
            </div>
          </Container>
        </Section>

        {/* Closing CTA */}
        <section className="py-20 sm:py-24">
          <Container>
            <Reveal>
              <div className="mx-auto max-w-2xl text-center">
                <h2 className="text-3xl font-semibold text-foreground sm:text-4xl">
                  Try it today. Free, no credit card.
                </h2>
                <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                  Connect your first site in under a minute. Self-host on your own infrastructure or use the hosted cloud version. No per-site fee.
                </p>
                <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
                  <a
                    href={SITE_CONFIG.signup}
                    className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-7 text-base font-medium text-[var(--primary-foreground)] shadow-sm hover:bg-[var(--primary-hover)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    Get started for free
                    <Icon name="ArrowRight" size={18} />
                  </a>
                  <a
                    href={SITE_CONFIG.github}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-7 text-base font-medium text-foreground hover:bg-[var(--accent)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    <Icon name="Github" size={18} />
                    Star on GitHub
                  </a>
                  <a
                    href="/features/"
                    className="inline-flex h-12 items-center justify-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-7 text-base font-medium text-foreground hover:bg-[var(--accent)] transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    See all features
                  </a>
                </div>
              </div>
            </Reveal>
          </Container>
        </section>
      </main>
      <SiteFooter />
    </>
  );
}
