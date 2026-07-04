import { Bot, KeyRound, User } from "lucide-react";
import type { AuditEntry } from "@wpmgr/api";

import { cn } from "@/lib/utils";

import { initials, resolveActor } from "./actor";

// ActorChip — the row's accountability anchor (point 3 of the audit-log
// redesign). Always renders a person/key/system identity, never a bare
// account UUID: a real name + avatar initials for a resolved user, a key
// badge for an api_key actor, a distinct "System" badge for automated
// events, and — only until actor_name/actor_email are populated, or for an
// actor whose account no longer exists — a small muted fallback chip that
// still leads with an icon rather than bare text.

export function ActorChip({
  entry,
  className,
}: {
  entry: AuditEntry;
  className?: string;
}) {
  const actor = resolveActor(entry);
  const title = [actor.name, actor.detail].filter(Boolean).join(" · ");

  if (actor.kind === "system") {
    return (
      <span
        className={cn("inline-flex min-w-0 items-center gap-1.5", className)}
      >
        <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-muted-foreground">
          <Bot aria-hidden="true" className="size-3" />
        </span>
        <span className="truncate text-sm italic text-muted-foreground">
          System
        </span>
      </span>
    );
  }

  if (actor.kind === "api_key") {
    return (
      <span
        className={cn("inline-flex min-w-0 items-center gap-1.5", className)}
        title={title || undefined}
      >
        <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-info-subtle text-info-subtle-fg">
          <KeyRound aria-hidden="true" className="size-3" />
        </span>
        <span className="truncate text-sm text-foreground">{actor.name}</span>
      </span>
    );
  }

  if (!actor.resolved) {
    return (
      <span
        className={cn("inline-flex min-w-0 items-center gap-1.5", className)}
        title={actor.detail ?? "Account details unavailable"}
      >
        <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-muted-foreground">
          <User aria-hidden="true" className="size-3" />
        </span>
        <span className="truncate font-mono text-xs text-muted-foreground">
          {actor.name}
        </span>
      </span>
    );
  }

  return (
    <span
      className={cn("inline-flex min-w-0 items-center gap-1.5", className)}
      title={title || undefined}
    >
      <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-primary/15 text-[10px] font-semibold text-primary">
        {initials(actor.name)}
      </span>
      <span className="truncate text-sm text-foreground">{actor.name}</span>
    </span>
  );
}
