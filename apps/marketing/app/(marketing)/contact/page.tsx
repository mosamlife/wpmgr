import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd, buildContactPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { ContactForm } from "./contact-form";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "Contact WPMgr",
  description:
    "Reach the WPMgr team for sales enquiries, support, security reports, or to ask about contributing to the open-source project.",
  canonical: "/contact",
});

const TOPICS = [
  {
    icon: "MessageSquare",
    title: "Sales",
    description: "Questions about hosted plans, volume pricing, or a demo.",
  },
  {
    icon: "LifeBuoy",
    title: "Support",
    description: "Issues with a self-hosted instance or the hosted dashboard.",
    note: "For faster help, check the docs first.",
    noteHref: SITE_CONFIG.docs,
  },
  {
    icon: "ShieldAlert",
    title: "Security",
    description: "Responsible disclosure of a vulnerability in WPMgr.",
    note: "See our security policy for scope and process.",
    noteHref: "/legal/security-policy",
  },
  {
    icon: "GitPullRequest",
    title: "Contributing",
    description: "Ideas, pull requests, or partnership proposals.",
    note: "Contributing guide on GitHub.",
    noteHref: `${SITE_CONFIG.github}/blob/main/docs/contributing.md`,
  },
];

export default function ContactPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Contact", href: "/contact" },
  ]);

  const contactLd = buildContactPageLd();

  return (
    <>
      <JsonLd data={breadcrumb} />
      <JsonLd data={contactLd} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <nav
            aria-label="Breadcrumb"
            className="mb-5 flex items-center gap-2 text-sm text-[var(--muted-foreground)]"
          >
            <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
            <span aria-hidden>/</span>
            <span className="text-foreground">Contact</span>
          </nav>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Contact
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              Get in touch
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Sales, support, security reports, and contributing. Tell us what you need and we will
              route it to the right person.
            </p>
          </div>
        </Container>
      </section>

      {/* Topic cards + form */}
      <Section>
        <Container>
          <div className="grid gap-12 lg:grid-cols-2 lg:gap-16">
            {/* Left: topic guide */}
            <div>
              <h2 className="text-xl font-semibold text-foreground">What can we help with?</h2>
              <ul className="mt-6 space-y-5" role="list">
                {TOPICS.map((topic) => (
                  <li key={topic.title} className="flex gap-4">
                    <div className="mt-0.5 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                      <Icon name={topic.icon as Parameters<typeof Icon>[0]["name"]} size={18} />
                    </div>
                    <div>
                      <p className="font-semibold text-foreground">{topic.title}</p>
                      <p className="mt-0.5 text-sm leading-relaxed text-[var(--muted-foreground)]">
                        {topic.description}
                      </p>
                      {topic.note && topic.noteHref && (
                        <p className="mt-1 text-sm">
                          {topic.noteHref.startsWith("http") ? (
                            <a
                              href={topic.noteHref}
                              target="_blank"
                              rel="noreferrer noopener"
                              className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                            >
                              {topic.note}
                            </a>
                          ) : (
                            <Link
                              href={topic.noteHref}
                              className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                            >
                              {topic.note}
                            </Link>
                          )}
                        </p>
                      )}
                    </div>
                  </li>
                ))}
              </ul>

              {/* GitHub quick link */}
              <div className="mt-8 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-5">
                <p className="text-sm font-medium text-foreground">Prefer GitHub?</p>
                <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                  File a bug, propose a feature, or submit a pull request.
                </p>
                <a
                  href={SITE_CONFIG.github}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="mt-3 inline-flex items-center gap-2 text-sm font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  <Icon name="Github" size={15} />
                  Open an issue on GitHub
                </a>
              </div>
            </div>

            {/* Right: form */}
            <div>
              <ContactForm />
            </div>
          </div>
        </Container>
      </Section>
    </>
  );
}
