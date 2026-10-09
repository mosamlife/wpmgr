import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import {
  CAPABILITY_DESCRIPTIONS,
  capabilityLabel,
  nextAbilityTicks,
  type AbilityRow,
  type AbilityTicks,
} from "./capabilities";

// The site-tools box (scope mcp:site), shared by the wizard's capability step
// and the consent screen so the two cannot describe it differently. Two
// explicit ticks, in no preset: the read can return page text, and the request
// only ever creates a request that a person approves in WPMgr. The box keeps no
// ticks of its own; the host passes them in. The wizard opens both clear, and
// the consent screen opens both ticked when the app asked for site tools.
//
// THE REQUEST NEEDS THE READ. A connection holding "ask for changes" without
// "see what the site can do" cannot call the tool that carries a request, so
// the box never lets that pair stand: ticking the request ticks the read, and
// clearing the read clears the request (nextAbilityTicks). The box works out the
// next state of BOTH rows and reports it in one onChange, so a host applies one
// update rather than two that could overwrite each other.

export interface AbilityCapabilityBoxProps {
  readonly readChecked: boolean;
  readonly requestChecked: boolean;
  /** The next state of both rows, with the request-needs-read rule applied. */
  readonly onChange: (next: AbilityTicks) => void;
  readonly disabled?: boolean;
  /** False when the server did not offer that capability to this app. Default true. */
  readonly readOffered?: boolean;
  readonly requestOffered?: boolean;
}

export function AbilityCapabilityBox({
  readChecked,
  requestChecked,
  onChange,
  disabled,
  readOffered = true,
  requestOffered = true,
}: AbilityCapabilityBoxProps) {
  // WHAT IS REALLY IN FORCE, which is also what an approval will send. A request
  // is only usable alongside a read that is on offer, and only counts while that
  // read is ticked, so a tick the host still holds for a request that cannot
  // stand is shown clear rather than ticked.
  const readOn = readOffered && readChecked;
  const requestUsable = requestOffered && readOffered;
  const requestOn = requestUsable && readOn && requestChecked;
  const current: AbilityTicks = { read: readOn, request: requestOn };

  const rows: readonly {
    readonly cap: "mcp.ability.read" | "mcp.ability.request";
    readonly row: AbilityRow;
    readonly usable: boolean;
    readonly on: boolean;
  }[] = [
    { cap: "mcp.ability.read", row: "read", usable: readOffered, on: readOn },
    { cap: "mcp.ability.request", row: "request", usable: requestUsable, on: requestOn },
  ];

  return (
    <div
      data-testid="ability-capability-box"
      className="rounded-lg border border-[var(--color-border)] p-3"
    >
      <p className="mb-2 text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]">
        Site tools
      </p>
      <ul className="space-y-2">
        {rows.map(({ cap, row, usable, on }) => (
          <li key={cap}>
            <label
              className={cn(
                "flex items-start gap-2 rounded-md border border-[var(--color-border)] p-2 text-sm",
                (disabled === true || !usable) && "cursor-not-allowed opacity-70",
              )}
            >
              <Checkbox
                className="mt-0.5"
                data-testid={`ability-box-${cap}`}
                checked={on}
                disabled={disabled === true || !usable}
                onChange={(e) => onChange(nextAbilityTicks(current, row, e.target.checked))}
              />
              <span>
                <span className="block font-medium text-[var(--color-foreground)]">
                  {capabilityLabel(cap)}
                </span>
                <span className="block text-xs text-[var(--color-muted-foreground)]">
                  {CAPABILITY_DESCRIPTIONS[cap]}
                </span>
                {row === "request" ? (
                  <RequestNeedsRead requestOffered={requestOffered} readOffered={readOffered} />
                ) : !readOffered ? (
                  <NotOffered cap={cap} />
                ) : null}
              </span>
            </label>
          </li>
        ))}
      </ul>
    </div>
  );
}

function NotOffered({ cap }: { readonly cap: string }) {
  return (
    <span
      data-testid={`ability-not-offered-${cap}`}
      className="mt-1 block text-xs font-medium text-[var(--color-muted-foreground)]"
    >
      WPMgr did not offer this for this connection.
    </span>
  );
}

/**
 * What the request row says about its dependency: that WPMgr did not offer it
 * at all, that it cannot be used because the read it needs was not offered
 * either, or, when it can be used, how it moves with the read. The box only
 * appears when the app asked for site tools, so the notes say what WPMgr did,
 * never that the app did not ask.
 */
function RequestNeedsRead({
  requestOffered,
  readOffered,
}: {
  readonly requestOffered: boolean;
  readonly readOffered: boolean;
}) {
  if (!requestOffered) return <NotOffered cap="mcp.ability.request" />;
  const readLabel = capabilityLabel("mcp.ability.read");
  if (!readOffered) {
    return (
      <span
        data-testid="ability-request-blocked"
        className="mt-1 block text-xs font-medium text-[var(--color-muted-foreground)]"
      >
        Needs &ldquo;{readLabel}&rdquo;, which WPMgr did not offer for this connection.
      </span>
    );
  }
  return (
    <span
      data-testid="ability-request-needs-read"
      className="mt-1 block text-xs text-[var(--color-muted-foreground)]"
    >
      Needs &ldquo;{readLabel}&rdquo;. Ticking this ticks that too, and clearing that clears this.
    </span>
  );
}
