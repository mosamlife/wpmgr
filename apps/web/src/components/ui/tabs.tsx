import * as React from "react";

import { cn } from "@/lib/utils";

// Tabs — a minimal, dependency-free, accessible tab set (WAI-ARIA tabs pattern).
//
// Built to live INSIDE the focus-trapped Dialog primitive, so it manages its own
// roving tabindex + arrow-key navigation without pulling in a radix dependency
// (the repo's frozen lockfile can't take one). Automatic activation: arrow keys
// move focus AND select, matching the shadcn/radix default.
//
//   <Tabs value={tab} onValueChange={setTab}>
//     <TabsList aria-label="Sharing">
//       <TabsTrigger value="invite">Invite</TabsTrigger>
//       <TabsTrigger value="people">Collaborators</TabsTrigger>
//     </TabsList>
//     <TabsContent value="invite">…</TabsContent>
//     <TabsContent value="people">…</TabsContent>
//   </Tabs>

interface TabsContextValue {
  value: string;
  setValue: (v: string) => void;
  baseId: string;
}

const TabsContext = React.createContext<TabsContextValue | null>(null);

function useTabs(component: string): TabsContextValue {
  const ctx = React.useContext(TabsContext);
  if (!ctx) {
    throw new Error(`${component} must be used within <Tabs>`);
  }
  return ctx;
}

export interface TabsProps {
  value: string;
  onValueChange: (value: string) => void;
  children: React.ReactNode;
  className?: string;
}

export function Tabs({ value, onValueChange, children, className }: TabsProps) {
  const baseId = React.useId();
  const ctx = React.useMemo<TabsContextValue>(
    () => ({ value, setValue: onValueChange, baseId }),
    [value, onValueChange, baseId],
  );
  return (
    <TabsContext.Provider value={ctx}>
      <div className={className}>{children}</div>
    </TabsContext.Provider>
  );
}

export interface TabsListProps {
  children: React.ReactNode;
  className?: string;
  "aria-label"?: string;
}

export function TabsList({ children, className, ...aria }: TabsListProps) {
  // Arrow-key roving over the enabled triggers, with Home/End. Reads the target
  // value from each trigger's data-value so we don't need per-trigger refs.
  function onKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    const keys = ["ArrowRight", "ArrowLeft", "Home", "End"];
    if (!keys.includes(e.key)) return;
    const tabs = Array.from(
      e.currentTarget.querySelectorAll<HTMLButtonElement>(
        '[role="tab"]:not([disabled])',
      ),
    );
    if (tabs.length === 0) return;
    const current = tabs.findIndex(
      (t) => t === document.activeElement,
    );
    let next = current;
    switch (e.key) {
      case "ArrowRight":
        next = current < 0 ? 0 : (current + 1) % tabs.length;
        break;
      case "ArrowLeft":
        next = current <= 0 ? tabs.length - 1 : current - 1;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = tabs.length - 1;
        break;
    }
    e.preventDefault();
    tabs[next]?.focus();
    tabs[next]?.click();
  }

  return (
    <div
      role="tablist"
      onKeyDown={onKeyDown}
      className={cn(
        "flex items-stretch gap-1 border-b border-[var(--color-border)]",
        className,
      )}
      {...aria}
    >
      {children}
    </div>
  );
}

export interface TabsTriggerProps {
  value: string;
  children: React.ReactNode;
  disabled?: boolean;
  className?: string;
}

export function TabsTrigger({
  value,
  children,
  disabled,
  className,
}: TabsTriggerProps) {
  const { value: active, setValue, baseId } = useTabs("TabsTrigger");
  const selected = active === value;
  return (
    <button
      type="button"
      role="tab"
      id={`${baseId}-tab-${value}`}
      aria-selected={selected}
      aria-controls={`${baseId}-panel-${value}`}
      tabIndex={selected ? 0 : -1}
      disabled={disabled}
      data-value={value}
      onClick={() => setValue(value)}
      className={cn(
        "relative -mb-px inline-flex items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50",
        selected
          ? "border-[var(--color-primary)] text-[var(--color-foreground)]"
          : "border-transparent text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]",
        className,
      )}
    >
      {children}
    </button>
  );
}

export interface TabsContentProps {
  value: string;
  children: React.ReactNode;
  className?: string;
}

export function TabsContent({ value, children, className }: TabsContentProps) {
  const { value: active, baseId } = useTabs("TabsContent");
  if (active !== value) return null;
  return (
    <div
      role="tabpanel"
      id={`${baseId}-panel-${value}`}
      aria-labelledby={`${baseId}-tab-${value}`}
      tabIndex={0}
      className={cn("focus-visible:outline-none", className)}
    >
      {children}
    </div>
  );
}
