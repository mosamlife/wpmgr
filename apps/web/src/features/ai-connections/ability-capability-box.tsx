import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import { CAPABILITY_DESCRIPTIONS, capabilityLabel } from "./capabilities";

// The site-tools box (scope mcp:site), shared by the wizard's capability step
// and the consent screen so the two cannot describe it differently. Two
// explicit ticks, both clear by default and in no preset: the read can return
// page text, and the request lets the connection make changes as far as each
// site's setting allows (a change the setting does not allow waits for a
// person in WPMgr).

const ROWS = ["mcp.ability.read", "mcp.ability.request"] as const;

export interface AbilityCapabilityBoxProps {
  readonly readChecked: boolean;
  readonly requestChecked: boolean;
  readonly onReadChange: (checked: boolean) => void;
  readonly onRequestChange: (checked: boolean) => void;
  readonly disabled?: boolean;
  /** False when the server did not offer that capability to this app. Default true. */
  readonly readOffered?: boolean;
  readonly requestOffered?: boolean;
}

export function AbilityCapabilityBox({
  readChecked,
  requestChecked,
  onReadChange,
  onRequestChange,
  disabled,
  readOffered = true,
  requestOffered = true,
}: AbilityCapabilityBoxProps) {
  const offered = {
    "mcp.ability.read": readOffered,
    "mcp.ability.request": requestOffered,
  };
  const state = {
    "mcp.ability.read": readChecked,
    "mcp.ability.request": requestChecked,
  };
  const change = {
    "mcp.ability.read": onReadChange,
    "mcp.ability.request": onRequestChange,
  };
  return (
    <div
      data-testid="ability-capability-box"
      className="rounded-lg border border-[var(--color-border)] p-3"
    >
      <p className="mb-2 text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]">
        Site tools
      </p>
      <ul className="space-y-2">
        {ROWS.map((cap) => {
          const off = !offered[cap];
          return (
            <li key={cap}>
              <label
                className={cn(
                  "flex items-start gap-2 rounded-md border border-[var(--color-border)] p-2 text-sm",
                  (disabled === true || off) && "cursor-not-allowed opacity-70",
                )}
              >
                <Checkbox
                  className="mt-0.5"
                  data-testid={`ability-box-${cap}`}
                  checked={state[cap] && !off}
                  disabled={disabled === true || off}
                  onChange={(e) => change[cap](e.target.checked)}
                />
                <span>
                  <span className="block font-medium text-[var(--color-foreground)]">
                    {capabilityLabel(cap)}
                  </span>
                  <span className="block text-xs text-[var(--color-muted-foreground)]">
                    {CAPABILITY_DESCRIPTIONS[cap]}
                  </span>
                  {off ? (
                    <span
                      data-testid={`ability-not-offered-${cap}`}
                      className="mt-1 block text-xs font-medium text-[var(--color-muted-foreground)]"
                    >
                      Not requested by this app
                    </span>
                  ) : null}
                </span>
              </label>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
