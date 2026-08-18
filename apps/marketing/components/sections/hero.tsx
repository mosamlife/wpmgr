import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Badge, Container } from "@/components/ui/primitives";
import { cn } from "@/lib/utils";
import { newTabRel } from "@/lib/site";
import type { Cta } from "@/lib/content/types";

type HeroTrust = { icon: string; title: string; desc: string; href?: string };

type HeroProps = {
  badge?: string;
  // Heading is rendered directly (no JS animation on LCP element)
  heading: string;
  subhead: string;
  ctas: Cta[];
  trust?: HeroTrust[];
};

/** Home hero. The heading is server-rendered static text. NEVER wrap in a
 *  motion reveal or set initial opacity 0 -- this is the LCP element. */
export function Hero({ badge, heading, subhead, ctas, trust }: HeroProps) {
  return (
    <section
      className="relative overflow-hidden py-20 sm:py-28 lg:py-32"
      aria-label="Hero"
    >
      {/* Dot-field texture centered behind hero */}
      <div
        aria-hidden
        className="dot-field pointer-events-none absolute inset-0"
      />

      <Container className="relative">
        {/* Badge */}
        {badge && (
          <div className="mb-6 flex justify-center">
            <Badge className="font-mono text-xs">
              <span className="h-1.5 w-1.5 rounded-full bg-[var(--success)]" />
              {badge}
            </Badge>
          </div>
        )}

        {/* Heading: static, no motion. This is the LCP element. */}
        <div className="mx-auto max-w-4xl text-center">
          <h1
            className={cn(
              "text-4xl font-semibold tracking-tight text-foreground",
              "sm:text-5xl lg:text-[3.25rem]",
              "leading-[1.12] sm:leading-[1.1]",
            )}
          >
            {heading}
          </h1>
          <p className="mx-auto mt-6 max-w-2xl text-lg leading-relaxed text-[var(--muted-foreground)]">
            {subhead}
          </p>

          {/* CTAs */}
          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            {ctas.map((cta) => {
              const trailing = cta.icon === "ArrowRight";
              const external = cta.href.startsWith("http");
              return (
                <Button
                  key={cta.label}
                  href={cta.href}
                  variant={cta.variant ?? "primary"}
                  size="lg"
                  {...(external ? { target: "_blank", rel: newTabRel(cta.href) } : {})}
                >
                  {cta.icon && !trailing ? <Icon name={cta.icon} size={18} /> : null}
                  {cta.label}
                  {cta.icon && trailing ? <Icon name={cta.icon} size={18} /> : null}
                </Button>
              );
            })}
          </div>
        </div>

        {/* Trust chips */}
        {trust && trust.length > 0 && (
          <div className="mt-14 grid gap-4 sm:grid-cols-3 mx-auto max-w-3xl">
            {trust.map((t) => {
              const chipClass = "flex items-start gap-3 rounded-xl border border-[var(--border)] bg-card p-4";
              const inner = (
                <>
                  <span className="mt-0.5 inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                    <Icon name={t.icon} size={16} />
                  </span>
                  <div className="flex flex-col gap-0.5">
                    <span className="inline-flex items-center gap-1.5 text-sm font-semibold text-foreground">
                      {t.title}
                      {/* Persistent affordance. Hover does not exist on touch, and the
                          two neighbouring chips are inert, so without this the linked
                          chip is indistinguishable from them on a phone. */}
                      {t.href && (
                        <Icon
                          name="ArrowRight"
                          size={12}
                          className="text-[var(--muted-foreground)]"
                          aria-hidden
                        />
                      )}
                    </span>
                    <span className="text-xs text-[var(--muted-foreground)] leading-relaxed">{t.desc}</span>
                  </div>
                </>
              );
              return t.href ? (
                <a
                  key={t.title}
                  href={t.href}
                  className={cn(
                    chipClass,
                    "transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                  )}
                >
                  {inner}
                </a>
              ) : (
                <div key={t.title} className={chipClass}>
                  {inner}
                </div>
              );
            })}
          </div>
        )}
      </Container>
    </section>
  );
}
