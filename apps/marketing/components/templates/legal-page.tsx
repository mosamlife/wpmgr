// LegalPage: shared long-form template for Terms of Service, Privacy Policy,
// and Refund Policy. Server Component. Prose is capped near 70ch for
// readability; headings and structure match the existing security-policy
// page so the three legal documents feel like one family.
import type { ReactNode } from "react";
import Link from "next/link";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { COMPANY, LEGAL_CONTACT_HREF } from "@/lib/content/legal";

export type LegalSection = {
  heading: string;
  body: ReactNode;
};

export function LegalPage({
  eyebrow,
  title,
  intro,
  effectiveDateLabel,
  breadcrumbLabel,
  sections,
}: {
  eyebrow: string;
  title: string;
  intro: ReactNode;
  effectiveDateLabel: string;
  breadcrumbLabel: string;
  sections: LegalSection[];
}) {
  return (
    <>
      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <nav
            aria-label="Breadcrumb"
            className="mb-5 flex flex-wrap items-center gap-2 text-sm text-[var(--muted-foreground)]"
          >
            <Link href="/" className="hover:text-foreground transition-colors">
              Home
            </Link>
            <span aria-hidden>/</span>
            <Link href="/legal/" className="hover:text-foreground transition-colors">
              Legal
            </Link>
            <span aria-hidden>/</span>
            <span className="text-foreground">{breadcrumbLabel}</span>
          </nav>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              {eyebrow}
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              {title}
            </h1>
            <p className="mt-4 text-sm text-[var(--muted-foreground)]">{effectiveDateLabel}</p>
            <div className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              {intro}
            </div>
          </div>
        </Container>
      </section>

      {/* Prose body, capped near 70ch for readability */}
      <Section>
        <Container>
          <div className="mx-auto max-w-[70ch] space-y-10">
            {sections.map((s) => (
              <div key={s.heading}>
                <h2 className="text-2xl font-semibold text-foreground">{s.heading}</h2>
                <div className="mt-4 space-y-4 leading-7 text-[var(--muted-foreground)]">
                  {s.body}
                </div>
              </div>
            ))}
          </div>

          {/* Contact callout */}
          <div className="mx-auto mt-12 max-w-[70ch] rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6">
            <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <p className="font-medium text-foreground">Questions about this policy?</p>
                <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                  Reach us at{" "}
                  <a
                    href={LEGAL_CONTACT_HREF}
                    className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    {COMPANY.supportEmail}
                  </a>
                  .
                </p>
              </div>
              <Link
                href="/contact/"
                className="inline-flex items-center justify-center gap-2 rounded-[var(--radius)] bg-primary px-5 py-2.5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] whitespace-nowrap"
              >
                <Icon name="Mail" size={15} />
                Contact us
              </Link>
            </div>
          </div>
        </Container>
      </Section>
    </>
  );
}
