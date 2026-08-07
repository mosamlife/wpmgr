import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import type { ComparisonPageData } from "@/lib/content/types";

/**
 * Where the fleet data lives: one lane per product, three nodes each.
 *
 * This section carries the argument nobody closes by shipping a feature next
 * quarter. A rival can add incremental backups in a release. Where the data
 * lands is an architectural fact about the product, and stating it as three
 * labelled nodes makes that visible faster than any paragraph.
 *
 * BUILT FROM DOM, NOT SVG, on purpose. The nodes are real text in a flex row
 * with connectors between them, so the content is selectable, translatable,
 * readable by a screen reader in order, and present with no JavaScript. An SVG
 * diagram would have needed a second coordinate set for mobile and would have
 * put the labels beyond reach of the page's own text tooling.
 *
 * The only motion is a hover lift on our own lane, which is decoration and is
 * a transform, so nothing here depends on a transition to become visible.
 */
export function DataLocality({ data }: { data: ComparisonPageData }) {
  const nameOf = (key: string) =>
    key === "wpmgr" ? "WPMgr" : (data.products.find((p) => p.key === key)?.name ?? key);

  return (
    <Section id="locality">
      <Container>
        <div className="max-w-2xl">
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {data.locality.heading}
          </h2>
          <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
            {data.locality.subhead}
          </p>
        </div>

        <ul className="mt-10 flex flex-col gap-4">
          {data.locality.lanes.map((lane) => {
            const ours = lane.productKey === "wpmgr";
            return (
              <li
                key={lane.productKey}
                className={
                  ours
                    ? "rounded-xl border border-[var(--primary)]/35 bg-[var(--primary-subtle)]/40 p-5 sm:p-6"
                    : "rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm sm:p-6"
                }
              >
                <p className="text-sm font-semibold text-foreground">{nameOf(lane.productKey)}</p>

                {/* Nodes. Wraps to a column at narrow widths rather than needing
                    a second diagram. */}
                <div className="mt-4 flex flex-col gap-2 sm:flex-row sm:items-center sm:gap-0">
                  {lane.path.map((node, i) => (
                    <div key={node} className="flex items-center gap-2 sm:gap-0">
                      <span
                        className={
                          ours
                            ? "inline-flex items-center rounded-lg border border-[var(--primary)]/30 bg-card px-3 py-2 text-sm font-medium text-foreground"
                            : "inline-flex items-center rounded-lg border border-[var(--border)] bg-[var(--muted)]/40 px-3 py-2 text-sm text-foreground"
                        }
                      >
                        {node}
                      </span>
                      {i < lane.path.length - 1 && (
                        <span
                          aria-hidden
                          className="mx-2 flex items-center text-[var(--muted-foreground)]"
                        >
                          <Icon name="ArrowRight" size={14} />
                        </span>
                      )}
                    </div>
                  ))}
                </div>

                <p className="mt-4 text-sm leading-relaxed text-[var(--muted-foreground)]">
                  {lane.note}
                  {lane.cites?.map((id) => (
                    <a
                      key={id}
                      href={`/compare/${data.slug}/sources#${id}`}
                      className="ml-1 align-super text-[10px] underline underline-offset-2 hover:text-foreground"
                      aria-label={`Source, reference ${id}`}
                    >
                      {id.split("-")[1]}
                    </a>
                  ))}
                </p>
              </li>
            );
          })}
        </ul>
      </Container>
    </Section>
  );
}
