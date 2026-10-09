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
//
// THE VOCABULARY HAS NAMES THAT ARE NOT READS (tracka-cache-purge design v7,
// ADR-061 option B). `mcp.cache.purge` lets a connection ASK to clear a site's
// cache, and `mcp.ability.request` lets it ASK to make a change through a
// site's tools; neither does anything by itself. The design deck also draws a "propose changes" capability and a
// "Produce a change set for you to review" line, and neither of those exists:
// this screen says "ask", never "propose", because asking is bounded by a
// human approving each specific request and a change set is not. Copying
// either string off the deck would put a capability claim on this screen the
// server does not honour -- which is worse than saying nothing, because an
// operator reading it would calibrate their trust against a feature that does
// not exist. connection-contract.test.tsx fails if either reappears.
//
// The NEGATIVE half is not softened for the same reason, pointing the other
// way: "it cannot approve its own change" is true today, including for the
// one capability that can ask for one, and is the entire point of the screen.

/** Heading over the positive half. Asserted verbatim by the tests. */
export const CONTRACT_CAN_HEADING = "What a connection can do";

/** Heading over the negative half. Asserted verbatim by the tests. */
export const CONTRACT_CANNOT_HEADING = "What it can never do";

/**
 * The lead sentence (design v7 S2.3, connection-contract.tsx :42-44).
 *
 * The last sentence is the load-bearing one: it is what tells an operator
 * that an unstated permission is not a granted one. The deck's version of the
 * first sentence claims the connection can "propose changes to the sites you
 * name"; that clause is cut, because it is not true (see the file comment) --
 * the middle sentence below says "ask", never "propose", for the same reason.
 */
export const CONTRACT_LEAD =
  "A connection lets one AI client read your fleet, limited to the sites you name. " +
  "If you allow it, it can also ask you to clear their cache and to make changes through their tools. " +
  "Nothing about it is implicit.";

export const CONTRACT_CAN: readonly string[] = [
  "Read the sites you put in its scope",
  "Report what it found, with its sources",
  // "Ask", not "propose" -- CONTRACT_FORBIDDEN below still refuses "propose"
  // outright, and these two lines are why it can stay green: asking is bounded
  // by a person approving the specific request, which is the whole difference.
  // The two asks (the cache clear, and changes through a site's tools) are one
  // line, so they are described the same way by construction: what it may ask
  // for, if you allow it, and that nothing runs until a person approves each
  // request. Two parallel lines would repeat that closing sentence on one frame,
  // which the wizard's duplicate-sentence guard refuses.
  "Ask you to clear a site's cache, or to make changes through a site's tools, if you allow it. Nothing runs until a person approves each request.",
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
