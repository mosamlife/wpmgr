import { Icon } from "@/components/ui/icon";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { Reveal } from "@/components/motion/reveal";
import type { Step, Cta } from "@/lib/content/types";
import { Button } from "@/components/ui/button";
import { newTabRel } from "@/lib/site";

function StepCard({ n, icon, title, desc }: Step) {
  return (
    <div className="flex flex-col gap-3 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm">
      <div className="flex items-center justify-between">
        <span
          className="font-mono text-2xl font-medium text-[var(--primary)]"
          style={{ fontVariantNumeric: "tabular-nums" }}
        >
          {n}
        </span>
        <Icon name={icon} size={20} className="text-[var(--muted-foreground)]" />
      </div>
      <h3 className="text-base font-semibold text-foreground">{title}</h3>
      <p className="text-sm leading-relaxed text-[var(--muted-foreground)]">{desc}</p>
    </div>
  );
}

export function Steps({
  id,
  eyebrow,
  heading,
  subhead,
  steps,
  cta,
  tone = "base",
}: {
  id?: string;
  eyebrow?: string;
  heading: string;
  subhead?: string;
  steps: Step[];
  cta?: Cta;
  tone?: "base" | "muted";
}) {
  const trailing = cta?.icon === "ArrowRight";
  return (
    <Section id={id} tone={tone}>
      <Container>
        <Reveal>
          <SectionHeading eyebrow={eyebrow} title={heading} lead={subhead} />
        </Reveal>
        <Stagger className="mt-12 grid gap-6 sm:grid-cols-2 lg:grid-cols-4">
          {steps.map((step) => (
            <StaggerItem key={step.n}>
              <StepCard {...step} />
            </StaggerItem>
          ))}
        </Stagger>
        {cta && (
          <Reveal className="mt-10 flex justify-center">
            <Button
              href={cta.href}
              variant={cta.variant ?? "primary"}
              size="lg"
              {...(cta.href.startsWith("http") ? { target: "_blank", rel: newTabRel(cta.href) } : {})}
            >
              {cta.icon && !trailing ? <Icon name={cta.icon} size={18} /> : null}
              {cta.label}
              {cta.icon && trailing ? <Icon name={cta.icon} size={18} /> : null}
            </Button>
          </Reveal>
        )}
      </Container>
    </Section>
  );
}
