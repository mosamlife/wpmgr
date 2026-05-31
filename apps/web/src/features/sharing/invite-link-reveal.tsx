import { useRef, useState } from "react";
import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { toast } from "@/components/toast";

// InviteLinkReveal — the one-time invite-link surface.
//
// The raw accept token is shown exactly once (only its hash is stored), so this
// panel is the single place a fresh link (from a brand-new invite) OR a rotated
// link (from Regenerate) is surfaced + copied. It clears when the parent stops
// passing a `link` (tab switch / dialog close), matching the prior in-dialog
// behavior — there is no "show it again" because the token is unrecoverable.

interface InviteLinkRevealProps {
  /** The one-time accept link to reveal. */
  link: string;
  /** Optional recipient email, woven into the heading/copy. */
  email?: string;
  /** Heading; defaults to a generic "Invite link created". */
  title?: string;
  /** When true, frames it as a *rotated* link (old one dead). */
  rotated?: boolean;
}

export function InviteLinkReveal({
  link,
  email,
  title,
  rotated = false,
}: InviteLinkRevealProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [copied, setCopied] = useState(false);

  function copy() {
    void navigator.clipboard.writeText(link).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
      toast.success("Invite link copied to clipboard");
    });
  }

  const heading =
    title ??
    (rotated
      ? `New invite link${email ? ` for ${email}` : ""}`
      : "Invite link created");

  return (
    <div
      role="alert"
      aria-live="polite"
      className="space-y-2 rounded-lg border border-[var(--color-primary)]/30 bg-[var(--color-primary)]/5 p-4"
    >
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        {heading}
      </p>
      <p className="text-sm text-[var(--color-muted-foreground)]">
        {rotated
          ? "The previous link no longer works. This is shown once — copy it now."
          : "This link is shown once. Copy and share it with the invitee."}
      </p>
      <div className="flex gap-2">
        <Input
          ref={inputRef}
          readOnly
          value={link}
          className="font-mono text-xs"
          onFocus={(e) => e.target.select()}
          aria-label="Invite link"
        />
        <Button
          type="button"
          variant="outline"
          onClick={copy}
          aria-label="Copy invite link"
          className="shrink-0 gap-1.5"
        >
          {copied ? (
            <>
              <Check aria-hidden="true" className="size-4" />
              Copied
            </>
          ) : (
            <>
              <Copy aria-hidden="true" className="size-4" />
              Copy link
            </>
          )}
        </Button>
      </div>
    </div>
  );
}
