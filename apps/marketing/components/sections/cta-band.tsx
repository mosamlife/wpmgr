import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Container, Section } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { newTabRel } from "@/lib/site";
import type { Cta } from "@/lib/content/types";

export function CTABand({
  heading,
  subhead,
  body,
  ctas,
}: {
  heading: string;
  subhead?: string;
  body?: string;
  ctas: Cta[];
}) {
  return (
    <Section tone="muted" id="cta">
      <Container>
        <Reveal>
          <div className="mx-auto max-w-2xl text-center flex flex-col gap-4">
            <h2 className="text-3xl font-semibold text-foreground sm:text-4xl">{heading}</h2>
            {subhead && (
              <p className="text-lg leading-relaxed text-[var(--muted-foreground)]">{subhead}</p>
            )}
            {body && (
              <p className="text-sm text-[var(--muted-foreground)]">{body}</p>
            )}
            <div className="mt-4 flex flex-wrap items-center justify-center gap-3">
              {ctas.map((cta) => {
                const trailing = cta.icon === "ArrowRight";
                return (
                  <Button
                    key={cta.label}
                    href={cta.href}
                    variant={cta.variant ?? "primary"}
                    size="lg"
                    {...(cta.href.startsWith("http") ? { target: "_blank", rel: newTabRel(cta.href) } : {})}
                  >
                    {cta.icon && !trailing ? <Icon name={cta.icon} size={18} /> : null}
                    {cta.label}
                    {cta.icon && trailing ? <Icon name={cta.icon} size={18} /> : null}
                  </Button>
                );
              })}
            </div>
          </div>
        </Reveal>
      </Container>
    </Section>
  );
}
