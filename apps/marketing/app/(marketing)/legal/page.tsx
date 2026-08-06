import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section, Card } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "Legal: Terms, Privacy, Security Policy",
  description:
    "WPMgr legal information: responsible disclosure and security policy, terms of service, and privacy policy.",
  canonical: "/legal",
});

const LEGAL_ITEMS = [
  {
    icon: "ShieldCheck",
    title: "Security Policy",
    description:
      "Responsible disclosure scope and process, our security posture (Ed25519-signed agent, redacted diagnostics, client-side-encrypted backups), and how to report a vulnerability.",
    href: "/legal/security-policy/",
    external: false,
    cta: "Read the security policy",
  },
  {
    icon: "ScrollText",
    title: "Terms of Service",
    description:
      "Terms governing use of the WPMgr hosted service at manage.wpmgr.app, including subscription billing through your chosen provider. The self-hosted control plane and MIT-licensed agent are governed by their respective open-source licenses (AGPL-3.0 / MIT).",
    href: "/terms/",
    external: false,
    cta: "View terms",
  },
  {
    icon: "LockKeyhole",
    title: "Privacy Policy",
    description:
      "How WPMgr collects, uses, and stores data from hosted service users, including our sub-processors, Google Cloud Platform, Stripe, Razorpay, and Paddle. The agent is privacy-first and off-by-default.",
    href: "/privacy/",
    external: false,
    cta: "View privacy policy",
  },
  {
    icon: "Undo2",
    title: "Refund Policy",
    description:
      "Cancel a monthly subscription anytime. First-time subscribers are covered by a 14-day money-back guarantee, refunded through your original payment provider.",
    href: "/refunds/",
    external: false,
    cta: "View refund policy",
  },
  {
    icon: "Scale",
    title: "Open-Source License",
    description:
      "The WPMgr control plane and dashboard are licensed under AGPL-3.0. The WordPress agent is licensed under MIT. You can read every line of code you run.",
    href: `${SITE_CONFIG.github}/blob/main/LICENSE`,
    external: true,
    cta: "View license on GitHub",
  },
];

export default function LegalPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Legal", href: "/legal/" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <nav
            aria-label="Breadcrumb"
            className="mb-5 flex items-center gap-2 text-sm text-[var(--muted-foreground)]"
          >
            <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
            <span aria-hidden>/</span>
            <span className="text-foreground">Legal</span>
          </nav>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Legal
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              Transparency and trust
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              WPMgr is open-source and privacy-first. Our policies reflect that. Read the code,
              audit the agent, report a vulnerability responsibly, or review the terms and privacy
              policy for the hosted service.
            </p>
          </div>
        </Container>
      </section>

      {/* Cards */}
      <Section>
        <Container>
          <ul className="grid gap-6 sm:grid-cols-2" role="list">
            {LEGAL_ITEMS.map((item) => (
              <li key={item.title}>
                <Card className="flex h-full flex-col gap-5">
                  <div className="inline-flex h-11 w-11 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                    <Icon name={item.icon as Parameters<typeof Icon>[0]["name"]} size={22} />
                  </div>
                  <div className="flex-1">
                    <h2 className="text-lg font-semibold text-foreground">{item.title}</h2>
                    <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">
                      {item.description}
                    </p>
                  </div>
                  <div className="mt-auto">
                    {item.external ? (
                      <a
                        href={item.href}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)] hover:opacity-80 transition-opacity focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        {item.cta}
                        <Icon name="ExternalLink" size={14} />
                      </a>
                    ) : (
                      <Link
                        href={item.href}
                        className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)] hover:opacity-80 transition-opacity focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        {item.cta}
                        <Icon name="ArrowRight" size={14} />
                      </Link>
                    )}
                  </div>
                </Card>
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      {/* Open-source note */}
      <section className="border-t border-[var(--border)] py-12">
        <Container className="max-w-2xl text-center">
          <h2 className="text-xl font-semibold text-foreground">Open source by default</h2>
          <p className="mt-3 text-[var(--muted-foreground)]">
            The control plane (AGPL-3.0) and the WordPress agent (MIT) are fully open source. You
            can self-host the entire stack, read every line of code that runs on your servers, and
            contribute to the project.
          </p>
          <a
            href={SITE_CONFIG.github}
            target="_blank"
            rel="noreferrer noopener"
            className="mt-5 inline-flex items-center gap-2 text-sm font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
          >
            <Icon name="Github" size={15} />
            View the source on GitHub
          </a>
        </Container>
      </section>
    </>
  );
}
