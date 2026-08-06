import { Icon } from "@/components/ui/icon";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";

type ProvenanceFact = { label: string; value: string; mono?: boolean };
type ProvenanceCheck = { icon: string; text: string };
type ProvenancePanel = {
  icon: string;
  source: string;
  title: string;
  body: string;
  link: { label: string; href: string };
};

/**
 * Provenance: the WordPress.org listing and the public repository, side by
 * side as two different proof shapes (a record vs an invitation to verify).
 * Left panel is the dominant, denser record; right panel is deliberately
 * shorter (lg:items-start, no h-full) -- that height delta is intentional.
 */
export function Provenance({
  eyebrow,
  heading,
  subhead,
  directory,
  facts,
  repository,
  checks,
}: {
  eyebrow: string;
  heading: string;
  subhead: string;
  directory: ProvenancePanel;
  facts: ProvenanceFact[];
  repository: ProvenancePanel;
  checks: ProvenanceCheck[];
}) {
  return (
    // tone="muted" is load-bearing: FeatureGrid above is base and ends in ~25 white
    // cards, and --card sits 1% off --background, so on a base section these panels
    // would have no surface of their own and read as two more feature cards. The
    // border-b keeps the seam into the muted section below.
    <Section id="provenance" tone="muted" className="border-y border-[var(--border)]">
      <Container>
        <Reveal>
          <SectionHeading eyebrow={eyebrow} title={heading} lead={subhead} />
        </Reveal>

        <Stagger className="mt-14 grid gap-6 lg:grid-cols-12 lg:items-start">
          {/* LEFT: the directory record */}
          <StaggerItem className="lg:col-span-7">
            <div className="rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8">
              <div className="flex items-center gap-3">
                <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                  <Icon name={directory.icon} size={18} />
                </span>
                <span className="text-xs font-semibold uppercase tracking-[0.12em] text-[var(--muted-foreground)]">
                  {directory.source}
                </span>
              </div>

              <h3 className="mt-5 text-xl font-semibold leading-snug text-foreground">
                {directory.title}
              </h3>
              <p className="mt-3 leading-relaxed text-[var(--muted-foreground)]">
                {directory.body}
              </p>

              <dl className="mt-6 grid gap-y-3 border-t border-[var(--border)] pt-6 sm:grid-cols-[max-content_1fr] sm:gap-x-8 sm:gap-y-2.5">
                {facts.map((f) => (
                  <div key={f.label} className="sm:contents">
                    <dt className="text-sm text-[var(--muted-foreground)]">{f.label}</dt>
                    {/* Mono is for identifiers (slug, version). A four-word product
                        name set in mono reads as a code literal. */}
                    <dd
                      className={
                        f.mono
                          ? "font-mono text-sm text-foreground"
                          : "text-sm font-medium text-foreground"
                      }
                      style={f.mono ? { fontVariantNumeric: "tabular-nums" } : undefined}
                    >
                      {f.value}
                    </dd>
                  </div>
                ))}
              </dl>

              <a
                href={directory.link.href}
                target="_blank"
                rel="noreferrer noopener"
                className="mt-6 inline-flex items-center gap-1.5 rounded-sm text-sm font-medium text-[var(--primary-pressed)] underline-offset-4 transition-colors duration-[var(--duration-fast)] hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                {directory.link.label}
                <Icon name="ArrowRight" size={14} />
              </a>
            </div>
          </StaggerItem>

          {/* RIGHT: the invitation to verify */}
          <StaggerItem className="lg:col-span-5">
            <div className="rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8">
              <div className="flex items-center gap-3">
                {/* Same chip geometry as the left panel so the two headers read as a
                    pair. Neutral fill rather than the teal tint, because GitHub's
                    brand policy wants the mark left monochrome. */}
                <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md border border-[var(--border)] bg-[var(--muted)] text-foreground">
                  <Icon name={repository.icon} size={18} />
                </span>
                <span className="text-xs font-semibold uppercase tracking-[0.12em] text-[var(--muted-foreground)]">
                  {repository.source}
                </span>
              </div>

              <h3 className="mt-5 text-xl font-semibold leading-snug text-foreground">
                {repository.title}
              </h3>
              <p className="mt-3 leading-relaxed text-[var(--muted-foreground)]">
                {repository.body}
              </p>

              <ul className="mt-6 flex flex-col gap-4 border-t border-[var(--border)] pt-6">
                {checks.map((c) => (
                  <li key={c.text} className="flex gap-3">
                    <span className="mt-0.5 shrink-0 text-[var(--primary)]">
                      <Icon name={c.icon} size={16} />
                    </span>
                    <span className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                      {c.text}
                    </span>
                  </li>
                ))}
              </ul>

              <a
                href={repository.link.href}
                target="_blank"
                rel="noreferrer noopener"
                className="mt-6 inline-flex items-center gap-1.5 rounded-sm text-sm font-medium text-[var(--primary-pressed)] underline-offset-4 transition-colors duration-[var(--duration-fast)] hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                {repository.link.label}
                <Icon name="ArrowRight" size={14} />
              </a>
            </div>
          </StaggerItem>
        </Stagger>
      </Container>
    </Section>
  );
}
