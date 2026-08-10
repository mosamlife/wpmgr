import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import type { ComparisonPageData, MatrixCell, MatrixTone } from "@/lib/content/types";

/**
 * The at-a-glance matrix.
 *
 * TONE IS SEMANTIC, NEVER PER PRODUCT. A tone describes what the CELL means,
 * so tinting our own column wholesale is not available. That matters because
 * the first group is deliberately parity, and a reader who spots the author's
 * column glowing green throughout stops believing the parity group, which is
 * the group doing the most work.
 *
 * MOBILE IS A CARD PER ROW, NOT A SCROLLING TABLE. A horizontally scrolling
 * table with a sticky label column shows roughly one product column at a time
 * on a 360px screen, so a reader can never see our cell and theirs together.
 * That is the entire job of a comparison. Below `lg` each row becomes a small
 * stacked card listing all three products, which fits.
 */

const TONE: Record<MatrixTone, { cell: string; icon: string | null; iconClass: string }> = {
  included: {
    cell: "text-foreground",
    icon: "Check",
    iconClass: "text-[var(--primary)]",
  },
  paid: {
    cell: "text-foreground",
    icon: "Receipt",
    iconClass: "text-[var(--muted-foreground)]",
  },
  partial: {
    cell: "text-foreground",
    icon: "AlertTriangle",
    iconClass: "text-[var(--muted-foreground)]",
  },
  absent: {
    cell: "text-[var(--muted-foreground)]",
    icon: "Minus",
    iconClass: "text-[var(--muted-foreground)]",
  },
  neutral: { cell: "text-foreground", icon: null, iconClass: "" },
};

function Cell({ cell, slug }: { cell: MatrixCell; slug: string }) {
  const tone = TONE[cell.tone];
  return (
    <span className="flex items-start gap-2">
      {tone.icon && (
        <Icon name={tone.icon} size={15} className={`mt-0.5 shrink-0 ${tone.iconClass}`} aria-hidden />
      )}
      <span className={tone.cell}>
        {cell.value}
        {cell.cites?.map((id) => (
          <a
            key={id}
            href={`/compare/${slug}/sources#${id}`}
            className="ml-1 align-super text-[10px] text-[var(--muted-foreground)] underline underline-offset-2 hover:text-foreground"
            aria-label={`Source for this figure, reference ${id}`}
          >
            {/* THE WHOLE ID, not the number after the prefix. Each product
                numbers its claims from one, so a bare "11" appears twice on the
                same page pointing at two different sources. The sources page
                prints the full id beside each claim, so this also matches what
                the reader lands on. */}
            {id}
          </a>
        ))}
      </span>
    </span>
  );
}

export function CompareMatrix({ data }: { data: ComparisonPageData }) {
  const cols = [
    { key: "wpmgr", name: "WPMgr" },
    ...data.products.map((p) => ({ key: p.key, name: p.name })),
  ];

  return (
    <Section tone="muted" id="matrix" className="border-y border-[var(--border)]">
      <Container>
        <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
          At a glance
        </h2>
        <p className="mt-3 max-w-[68ch] text-[var(--muted-foreground)]">
          Free text rather than ticks, because a tick cannot say &quot;paid add-on&quot;. Every
          figure links to the page it came from.
        </p>

        {/* Desktop: a real table. */}
        <div className="mt-10 hidden lg:block">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-[var(--border)]">
                <th scope="col" className="w-[22%] py-3 pr-4 text-left">
                  <span className="sr-only">Capability</span>
                </th>
                {cols.map((c) => (
                  <th key={c.key} scope="col" className="py-3 pr-4 text-left font-semibold text-foreground">
                    {c.name}
                  </th>
                ))}
              </tr>
            </thead>
            {data.matrix.map((group) => (
              <tbody key={group.label}>
                <tr>
                  <th
                    colSpan={cols.length + 1}
                    scope="colgroup"
                    className="pt-8 pb-2 text-left text-xs font-semibold uppercase tracking-[0.1em] text-[var(--muted-foreground)]"
                  >
                    {group.label}
                  </th>
                </tr>
                {group.rows.map((row) => (
                  <tr key={row.label} className="border-b border-[var(--border)]/60 align-top">
                    <th scope="row" className="py-4 pr-4 text-left font-medium text-foreground">
                      {row.label}
                    </th>
                    {cols.map((c) => (
                      <td key={c.key} className="py-4 pr-4 leading-relaxed">
                        {row.cells[c.key] ? (
                          <Cell cell={row.cells[c.key]!} slug={data.slug} />
                        ) : (
                          <span className="text-[var(--muted-foreground)]">Not stated</span>
                        )}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            ))}
          </table>
        </div>

        {/* Mobile: one card per row, all three products visible together. */}
        <div className="mt-8 flex flex-col gap-8 lg:hidden">
          {data.matrix.map((group) => (
            <div key={group.label}>
              <h3 className="text-xs font-semibold uppercase tracking-[0.1em] text-[var(--muted-foreground)]">
                {group.label}
              </h3>
              <div className="mt-3 flex flex-col gap-3">
                {group.rows.map((row) => (
                  <div
                    key={row.label}
                    className="rounded-xl border border-[var(--border)] bg-card p-4 shadow-sm"
                  >
                    <p className="text-sm font-semibold text-foreground">{row.label}</p>
                    <dl className="mt-3 flex flex-col gap-2.5">
                      {cols.map((c) => (
                        <div key={c.key} className="grid grid-cols-[5.5rem_1fr] gap-3">
                          <dt className="text-xs font-medium text-[var(--muted-foreground)] pt-0.5">
                            {c.name}
                          </dt>
                          <dd className="text-sm leading-relaxed">
                            {row.cells[c.key] ? (
                              <Cell cell={row.cells[c.key]!} slug={data.slug} />
                            ) : (
                              <span className="text-[var(--muted-foreground)]">Not stated</span>
                            )}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      </Container>
    </Section>
  );
}
