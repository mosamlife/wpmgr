import {
  INDIAN_CARD_GUIDANCE_ITEMS,
  INDIAN_SCALE_GUIDANCE_LINE,
} from "./billing-copy";

// S0.4 (stripe-design-v9.md 3.1, 5.14) — shown above the pay button for
// likely-Indian users, on both checkout surfaces.

export function IndianCardGuidance({
  showScaleLine,
}: {
  /** Whether Scale is the plan being purchased (or is one of the offered plans on this page) — appends the ₹15,000 auto-debit line. */
  showScaleLine: boolean;
}) {
  return (
    <div className="space-y-2 rounded-lg border border-border bg-muted/20 px-4 py-3 text-sm text-muted-foreground">
      <ul className="list-disc space-y-1 pl-4">
        {INDIAN_CARD_GUIDANCE_ITEMS.map((item) => (
          <li key={item}>{item}</li>
        ))}
      </ul>
      {showScaleLine ? <p>{INDIAN_SCALE_GUIDANCE_LINE}</p> : null}
    </div>
  );
}
