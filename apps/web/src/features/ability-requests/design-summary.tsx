import type { ReactNode } from "react";

import { cornersWords, fontWords, frameWords, spacingWords, swatchesOf, typeRamp } from "./design-model";
import type { PageDesign, PageFrame } from "./outline-model";

// The design the AI chose for a designed page, drawn from the exact input: the
// page's title, six colour swatches, the type ramp at the sizes the agent will
// write, the fonts, the corners, the spacing and the page frame the site
// resolved. Everything is a React text node. The only values that reach a style
// are the swatch colours (a closed "#" and six hex digits, checked again by
// swatchesOf) and the ramp's sizes and weights (numbers the card works out).
// A font name is the AI's word and is shown, never drawn: the font may not be
// on this screen.

export interface DesignSummaryProps {
  /** The page's title, the AI's word. */
  title: string;
  design: PageDesign;
  pageFrame: PageFrame;
}

export function DesignSummary({ title, design, pageFrame }: DesignSummaryProps) {
  const frame = frameWords(pageFrame);
  return (
    <div data-testid="design-summary" className="space-y-3 rounded-md bg-muted/30 p-3 text-sm text-foreground">
      <p className="break-words font-semibold">
        <bdi>{title}</bdi>
      </p>

      <Block label="Colours">
        <ul aria-label="Design colours" className="grid gap-2 sm:grid-cols-2">
          {swatchesOf(design.colors).map((s) => (
            <li key={s.role} className="flex items-center gap-2">
              <span
                data-testid="swatch-chip"
                aria-hidden="true"
                className="size-6 shrink-0 rounded border border-border"
                style={s.chip === null ? undefined : { backgroundColor: s.chip }}
              />
              <span className="min-w-0">
                <span data-testid="swatch-role" className="block font-medium">
                  {s.label}
                </span>
                <span className="block text-xs text-muted-foreground">
                  <code data-testid="swatch-hex" className="font-mono">
                    {s.hex}
                  </code>{" "}
                  {s.hint}
                </span>
              </span>
            </li>
          ))}
        </ul>
      </Block>

      <Block label="Type, in pixels">
        <ol aria-label="Type ramp" className="divide-y divide-border">
          {typeRamp(design.scale).map((step) => (
            <li key={step.style} className="flex items-baseline justify-between gap-3 py-1">
              <span
                data-testid={`ramp-sample-${step.style}`}
                className="min-w-0 break-words text-foreground"
                style={{ fontSize: `${step.size}px`, fontWeight: step.weight, lineHeight: 1.15 }}
              >
                <span data-testid="ramp-name">{step.label}</span>
              </span>
              <span data-testid="ramp-size" className="shrink-0 text-xs text-muted-foreground">
                {step.size} px
              </span>
            </li>
          ))}
        </ol>
      </Block>

      <dl data-testid="design-rows" className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1">
        <dt className="text-muted-foreground">Page frame</dt>
        <dd className="min-w-0 break-words">
          <p className="font-medium">{frame.name}</p>
          <p className="text-muted-foreground">{frame.line}</p>
        </dd>
        <dt className="text-muted-foreground">Heading font</dt>
        <dd className="min-w-0 break-words">
          <bdi>{fontWords(design.fonts.heading)}</bdi>
        </dd>
        <dt className="text-muted-foreground">Body font</dt>
        <dd className="min-w-0 break-words">
          <bdi>{fontWords(design.fonts.body)}</bdi>
        </dd>
        <dt className="text-muted-foreground">Corners</dt>
        <dd>{cornersWords(design.radius)}</dd>
        <dt className="text-muted-foreground">Spacing</dt>
        <dd>{spacingWords(design.density)}</dd>
      </dl>
    </div>
  );
}

/** One of our words over a block: small and muted. */
function Block({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      {children}
    </div>
  );
}
