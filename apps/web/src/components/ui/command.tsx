import * as React from "react";
import { Command as CommandPrimitive } from "cmdk";
import { Search } from "lucide-react";

import { cn } from "@/lib/utils";

// Minimal shadcn-style Command (cmdk) wrapper, styled to match the app's
// existing cmdk usage in features/command/command-palette.tsx. Unlike the
// palette (which wraps cmdk in its own Radix Dialog via `Command.Dialog`),
// these primitives render the bare `Command` root so callers can embed it
// inside a Popover (tag picker) or plain inline chrome (bulk tag-edit panel).
//
// cmdk already implements the APG combobox keyboard contract for us: DOM
// focus stays in <CommandInput> the whole time; ArrowUp/ArrowDown/Home/End
// move the active option via `aria-activedescendant`; Enter dispatches a
// select event at the active option without moving focus or closing anything
// (that's left entirely to the caller, e.g. a wrapping Popover).

const Command = React.forwardRef<
  React.ComponentRef<typeof CommandPrimitive>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive>
>(({ className, ...props }, ref) => (
  <CommandPrimitive
    ref={ref}
    className={cn(
      "flex w-full flex-col overflow-hidden text-[var(--color-foreground)]",
      className,
    )}
    {...props}
  />
));
Command.displayName = "Command";

const CommandInput = React.forwardRef<
  React.ComponentRef<typeof CommandPrimitive.Input>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Input>
>(({ className, ...props }, ref) => (
  <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-3">
    <Search
      aria-hidden="true"
      className="size-3.5 shrink-0 text-[var(--color-muted-foreground)]"
    />
    <CommandPrimitive.Input
      ref={ref}
      className={cn(
        "flex h-9 w-full bg-transparent py-2 text-sm outline-none",
        "placeholder:text-[var(--color-muted-foreground)]",
        "disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    />
  </div>
));
CommandInput.displayName = "CommandInput";

const CommandList = React.forwardRef<
  React.ComponentRef<typeof CommandPrimitive.List>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.List>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.List
    ref={ref}
    className={cn("max-h-64 overflow-y-auto overflow-x-hidden p-1", className)}
    {...props}
  />
));
CommandList.displayName = "CommandList";

const CommandGroup = React.forwardRef<
  React.ComponentRef<typeof CommandPrimitive.Group>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Group>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Group
    ref={ref}
    className={cn(
      "overflow-hidden p-1 text-[var(--color-foreground)]",
      "[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5",
      "[&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:font-medium",
      "[&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wide",
      "[&_[cmdk-group-heading]]:text-[var(--color-muted-foreground)]",
      className,
    )}
    {...props}
  />
));
CommandGroup.displayName = "CommandGroup";

const CommandItem = React.forwardRef<
  React.ComponentRef<typeof CommandPrimitive.Item>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Item>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Item
    ref={ref}
    className={cn(
      "relative flex cursor-pointer select-none items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none transition-colors",
      "aria-selected:bg-[var(--color-accent)] aria-selected:text-[var(--color-accent-foreground)]",
      "data-[disabled=true]:pointer-events-none data-[disabled=true]:opacity-50",
      className,
    )}
    {...props}
  />
));
CommandItem.displayName = "CommandItem";

export { Command, CommandInput, CommandList, CommandGroup, CommandItem };
