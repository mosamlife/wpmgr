import type { Metadata } from "next";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SELF_HOST } from "@/lib/content/self-host";
import { signupHref } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: SELF_HOST.metaTitle,
  description: SELF_HOST.metaDescription,
  canonical: "/self-host",
});

export default function SelfHostPage() {
  return (
    <>
      <Section>
        <Container>
          <h1 className="max-w-[22ch] text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            {SELF_HOST.hero.heading}
          </h1>
          <p className="mt-5 max-w-[64ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            {SELF_HOST.hero.subhead}
          </p>

          <div className="mt-8 max-w-3xl overflow-x-auto rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-4">
            <code className="whitespace-pre font-mono text-xs text-foreground sm:text-sm">
              {SELF_HOST.hero.command}
            </code>
          </div>
          <p className="mt-3 max-w-[70ch] text-sm leading-relaxed text-[var(--muted-foreground)]">
            {SELF_HOST.hero.commandNote}
          </p>
        </Container>
      </Section>

      {/* The trade, stated in both directions. */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container>
          <div className="max-w-3xl">
            <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
              {SELF_HOST.tradeoff.heading}
            </h2>
            <p className="mt-4 text-lg leading-relaxed text-foreground">
              {SELF_HOST.tradeoff.body}
            </p>
            <a
              href={signupHref("self-host")}
              target="_blank"
              rel="noreferrer noopener"
              className="mt-7 inline-flex h-11 items-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-6 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              Let us run it instead
              <Icon name="ArrowRight" size={16} aria-hidden />
            </a>
          </div>
        </Container>
      </Section>

      <Section>
        <Container>
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {SELF_HOST.runs.heading}
          </h2>
          <ul className="mt-8 grid gap-4 sm:grid-cols-2">
            {SELF_HOST.runs.items.map((it) => (
              <li
                key={it.title}
                className="flex gap-4 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm"
              >
                <span className="mt-0.5 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                  <Icon name={it.icon} size={18} aria-hidden />
                </span>
                <div>
                  <p className="font-semibold text-foreground">{it.title}</p>
                  <p className="mt-1 text-sm leading-relaxed text-[var(--muted-foreground)]">
                    {it.desc}
                  </p>
                </div>
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container className="max-w-3xl">
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {SELF_HOST.reality.heading}
          </h2>
          <ul className="mt-6 flex flex-col gap-3">
            {SELF_HOST.reality.items.map((line) => (
              <li key={line} className="flex gap-3 leading-relaxed text-foreground">
                <Icon
                  name="Check"
                  size={16}
                  className="mt-1 shrink-0 text-[var(--primary)]"
                  aria-hidden
                />
                {line}
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      <Section>
        <Container className="max-w-3xl">
          <h2 className="text-2xl font-semibold text-foreground">{SELF_HOST.cta.heading}</h2>
          <p className="mt-3 leading-relaxed text-[var(--muted-foreground)]">
            {SELF_HOST.cta.body}
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <a
              href={SELF_HOST.cta.primary.href}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex h-11 items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-6 text-sm font-medium text-foreground transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              <Icon name="Github" size={16} aria-hidden />
              {SELF_HOST.cta.primary.label}
            </a>
            <a
              href={SELF_HOST.cta.secondary.href}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex h-11 items-center rounded-[var(--radius)] border border-[var(--border)] bg-card px-6 text-sm font-medium text-foreground transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              {SELF_HOST.cta.secondary.label}
            </a>
          </div>
        </Container>
      </Section>

      <JsonLd
        data={buildBreadcrumbLd([
          { name: "Home", href: "/" },
          { name: "Self-host", href: "/self-host" },
        ])}
      />
    </>
  );
}
