import { Check, X } from "lucide-react";

import { cn } from "@/lib/utils";

// THE CAN / CANNOT CONTRACT (design step 1).
//
// This is the block an operator reads while deciding whether to trust an AI
// client with their fleet, and until now it was one clause -- "It cannot change
// anything." -- standing in for four distinct limits. The mechanism was built;
// the sentence explaining it was not, so the thing that makes this safe was
// invisible on the one screen where it decides something.
//
// EVERY LINE HERE IS TRUE OF THE SHIPPED SYSTEM. That is the only rule this
// file has, and it is the one that is easy to break on a later fidelity pass.
// The design deck also draws a "propose changes" capability and a "Produce a
// change set for you to review" line. Neither exists:
// apps/api/internal/mcp/policy.go's vocabulary is eight capability names and
// every one of them ends in `.read`, and the m131 CHECK admits only those
// eight, so no grant can be minted holding a propose capability. Copying those
// two strings off the deck would put a capability claim on this screen that the
// server would refuse -- which is worse than saying nothing, because an
// operator reading it would calibrate their trust against a feature that does
// not exist. connection-contract.test.tsx fails if either reappears.
//
// The NEGATIVE half is not softened for the same reason, pointing the other
// way: "it cannot approve or apply anything by itself" is true today and is the
// entire point of the screen.

/** Heading over the positive half. Asserted verbatim by the tests. */
export const CONTRACT_CAN_HEADING = "What a connection can do";

/** Heading over the negative half. Asserted verbatim by the tests. */
export const CONTRACT_CANNOT_HEADING = "What it can never do";

/**
 * The lead sentence.
 *
 * The second sentence is the load-bearing half: it is what tells an operator
 * that an unstated permission is not a granted one. The deck's version of the
 * first sentence claims the connection can "propose changes to the sites you
 * name"; that clause is cut, because it is not true (see the file comment).
 */
export const CONTRACT_LEAD =
  "A connection lets one AI client read your fleet, limited to the sites you name. " +
  "Nothing about it is implicit.";

export const CONTRACT_CAN: readonly string[] = [
  "Read the sites you put in its scope",
  "Report what it found, with its sources",
];

export const CONTRACT_CANNOT: readonly string[] = [
  "Approve its own change",
  "Reach a site outside its scope",
  "Run PHP, WP-CLI, a shell, or open a file path of its choosing",
  // The deck writes this as one clause joined by an em dash. Split into two
  // sentences: this repository ships no em or en dashes in copy, and the words
  // are what matter.
  "Be granted a “skip approval” setting. There isn’t one.",
];

/**
 * Copy that must never appear on this screen, exported so the guard test and
 * the reason for the guard live in the same place as the copy it guards.
 *
 * A future pass that "restores fidelity to the deck" is exactly the thing that
 * would re-add these, which is why the list is here rather than in the test.
 */
export const CONTRACT_FORBIDDEN: readonly string[] = [
  "Produce a change set for you to review",
  "propose",
];

export function ConnectionContract({ className }: { className?: string }) {
  return (
    <section
      aria-label="What a connection can and cannot do"
      data-testid="connection-contract"
      className={cn("space-y-3", className)}
    >
      <p className="text-sm text-[var(--color-muted-foreground)]">{CONTRACT_LEAD}</p>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="rounded-lg border border-[var(--color-border)] p-4">
          <h3 className="text-sm font-semibold text-[var(--color-foreground)]">
            {CONTRACT_CAN_HEADING}
          </h3>
          <ul className="mt-2 space-y-1.5">
            {CONTRACT_CAN.map((line) => (
              <li key={line} className="flex items-start gap-2 text-sm">
                <Check
                  aria-hidden="true"
                  strokeWidth={2}
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-success)]"
                />
                <span className="text-[var(--color-foreground)]">{line}</span>
              </li>
            ))}
          </ul>
        </div>

        {/* THE NEGATIVE HALF IS GIVEN EQUAL WEIGHT, not a quieter footnote.
            Four limits, each stated on its own, because "it cannot change
            anything" collapses four different guarantees into one sentence an
            operator has to take on faith. */}
        <div className="rounded-lg border border-[var(--color-border)] p-4">
          <h3 className="text-sm font-semibold text-[var(--color-foreground)]">
            {CONTRACT_CANNOT_HEADING}
          </h3>
          <ul className="mt-2 space-y-1.5">
            {CONTRACT_CANNOT.map((line) => (
              <li key={line} className="flex items-start gap-2 text-sm">
                <X
                  aria-hidden="true"
                  strokeWidth={2}
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
                />
                <span className="text-[var(--color-foreground)]">{line}</span>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </section>
  );
}
