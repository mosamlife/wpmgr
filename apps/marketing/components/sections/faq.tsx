import { Icon } from "@/components/ui/icon";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { cn } from "@/lib/utils";
import { Reveal } from "@/components/motion/reveal";
import type { FaqItem } from "@/lib/content/types";

// Native <details>/<summary>: the answer is always in the served HTML (a
// crawler that never executes JS still reads it), collapsed by default, and
// expand/collapse plus keyboard operation and the expanded a11y state come
// from the browser for free. No client JS needed for this component.
function FaqItemRow({ q, a }: FaqItem) {
  return (
    <details className="group border-b border-[var(--border)] last:border-0">
      <summary
        className={cn(
          "flex w-full list-none items-center justify-between gap-4 py-4 text-left",
          "cursor-pointer text-base font-medium text-foreground [&::-webkit-details-marker]:hidden",
          "transition-colors duration-[var(--duration-fast)] hover:text-[var(--primary)]",
          "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm",
        )}
      >
        <span>{q}</span>
        <Icon
          name="ChevronDown"
          size={18}
          className={cn(
            "shrink-0 text-[var(--muted-foreground)] transition-transform duration-[var(--duration-base)]",
            "group-open:rotate-180",
          )}
        />
      </summary>
      <p className="pb-5 text-sm leading-relaxed text-[var(--muted-foreground)]">{a}</p>
    </details>
  );
}

export function FAQ({
  eyebrow,
  heading,
  subhead,
  items,
}: {
  eyebrow?: string;
  heading: string;
  subhead?: string;
  items: FaqItem[];
}) {
  return (
    <Section id="faq">
      <Container>
        <Reveal>
          <SectionHeading eyebrow={eyebrow} title={heading} lead={subhead} />
        </Reveal>
        <Reveal delay={0.08}>
          <div className="mx-auto mt-12 max-w-2xl rounded-xl border border-[var(--border)] bg-card px-6 shadow-sm">
            {items.map((item) => (
              <FaqItemRow key={item.q} q={item.q} a={item.a} />
            ))}
          </div>
        </Reveal>
      </Container>
    </Section>
  );
}
