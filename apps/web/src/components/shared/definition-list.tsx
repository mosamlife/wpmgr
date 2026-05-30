import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

import { CopyableMono } from "./copyable-mono";

// DefinitionList + KvRow — the single canonical key/value grid (ADR-037 Batch
// 0), replacing the per-surface copies (health CardKVList, activity Field,
// backup Detail). One responsive 2-column grid: label (muted) / value
// (foreground). Pass rows via the `rows` prop or compose <KvRow> children.
//
//   • mono     → font-mono on the value (paths/versions/ids).
//   • tabular  → tabular-nums on the value (column-bound numbers).
//   • copyable → render the value through CopyableMono (overrides mono).
//   • Absent values render an en-dash "–" (U+2013); DESIGN.md bans em-dashes in
//     prose, but a data-absent glyph is fine and reads as "no value".

const ABSENT = "–"; // en-dash

export interface KvRowProps {
  label: string;
  value?: ReactNode;
  mono?: boolean;
  tabular?: boolean;
  /** When set, render the value through CopyableMono with this raw string. */
  copyable?: string;
}

export interface DefinitionListProps {
  /** Declarative rows. Omit to compose <KvRow> children instead. */
  rows?: KvRowProps[];
  children?: ReactNode;
  className?: string;
}

/**
 * DefinitionList — a <dl> laid out as a responsive label/value grid. The label
 * column sizes to content (min 8rem) and the value column takes the rest.
 */
export function DefinitionList({
  rows,
  children,
  className,
}: DefinitionListProps) {
  return (
    <dl
      className={cn(
        "grid min-w-0 grid-cols-[minmax(6rem,auto)_1fr] gap-x-4 gap-y-2 text-sm",
        className,
      )}
    >
      {rows?.map((row) => <KvRow key={row.label} {...row} />)}
      {children}
    </dl>
  );
}

/**
 * KvRow — one label/value pair inside a DefinitionList. Uses `display: contents`
 * so its dt/dd participate directly in the parent grid's two columns.
 */
export function KvRow({ label, value, mono, tabular, copyable }: KvRowProps) {
  const isAbsent = copyable == null && (value == null || value === "");

  let content: ReactNode;
  if (copyable != null) {
    content = <CopyableMono value={copyable} label={`Copy ${label}`} />;
  } else if (isAbsent) {
    content = <span className="text-muted-foreground">{ABSENT}</span>;
  } else {
    content = value;
  }

  return (
    <div className="contents">
      <dt className="text-muted-foreground">{label}</dt>
      <dd
        className={cn(
          "min-w-0 break-words text-foreground",
          mono && "font-mono break-all",
          tabular && "tabular-nums",
        )}
      >
        {content}
      </dd>
    </div>
  );
}
