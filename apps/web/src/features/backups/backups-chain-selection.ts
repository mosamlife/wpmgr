import type { BackupSnapshot } from "@wpmgr/api";

import { isTerminal } from "./use-backups";

// Chain-aware selection helpers for the bulk-delete surface (issue #115).
//
// Dependents are locally derivable straight off the list DTO: any member of
// the same chain_id with a strictly higher `generation` depends on this one
// (the server refuses to delete a snapshot that still has a later increment —
// "chain_has_dependents"). These are pure functions so the checkbox
// tri-state logic and the auto-expand-on-check behaviour are unit-testable
// without mounting <BackupsSection>.

export type CheckState = "checked" | "indeterminate" | "unchecked";

/** All members of `members` with a strictly higher generation than `member`. */
export function chainDependents(
  members: readonly BackupSnapshot[],
  member: BackupSnapshot,
): BackupSnapshot[] {
  const gen = member.generation ?? 0;
  return members.filter((m) => (m.generation ?? 0) > gen);
}

/** Terminal-only dependents — the subset eligible to be auto-selected. */
export function terminalChainDependents(
  members: readonly BackupSnapshot[],
  member: BackupSnapshot,
): BackupSnapshot[] {
  return chainDependents(members, member).filter((m) => isTerminal(m.status));
}

/**
 * Per-row checkbox state for one chain member.
 *
 *   unchecked     — not selected.
 *   checked        — selected AND every dependent (terminal or not) is also
 *                    selected, i.e. deleting this member alone would succeed.
 *   indeterminate  — selected but at least one dependent is NOT selected
 *                    (either the operator unchecked it, or it's non-terminal
 *                    and can never be selected) — deleting this member alone
 *                    would be rejected server-side (chain_has_dependents).
 */
export function memberCheckState(
  selectedIds: ReadonlySet<string>,
  members: readonly BackupSnapshot[],
  member: BackupSnapshot,
): CheckState {
  if (!selectedIds.has(member.id)) return "unchecked";
  const deps = chainDependents(members, member);
  if (deps.length === 0) return "checked";
  const allDepsSelected = deps.every((d) => selectedIds.has(d.id));
  return allDepsSelected ? "checked" : "indeterminate";
}

/**
 * Tri-state for the chain PARENT's own "select chain" checkbox, computed over
 * only the terminal (selectable) members: unchecked when none are selected,
 * checked when all are, indeterminate otherwise. A chain with zero terminal
 * members (still fully in flight) is "unchecked" and its checkbox should be
 * disabled by the caller.
 */
export function chainCheckState(
  selectedIds: ReadonlySet<string>,
  members: readonly BackupSnapshot[],
): CheckState {
  const eligible = members.filter((m) => isTerminal(m.status));
  if (eligible.length === 0) return "unchecked";
  const selectedCount = eligible.filter((m) => selectedIds.has(m.id)).length;
  if (selectedCount === 0) return "unchecked";
  return selectedCount === eligible.length ? "checked" : "indeterminate";
}

/**
 * Count members in `batch` that are "auto-included dependents" — i.e. a
 * later generation in a chain where an earlier generation is also in the
 * batch. Used for the confirm dialog's "+M dependents auto-included" line.
 * Every chain in the batch contributes (members above its lowest selected
 * generation) regardless of whether the operator clicked those rows directly
 * or they were pulled in by the auto-expand-on-check behaviour — the count
 * communicates "these are here because of the chain", not literal click
 * provenance.
 */
export function countAutoIncludedDependents(
  batch: readonly BackupSnapshot[],
): number {
  const byChain = new Map<string, BackupSnapshot[]>();
  for (const s of batch) {
    if (!s.chain_id) continue;
    const bucket = byChain.get(s.chain_id);
    if (bucket) bucket.push(s);
    else byChain.set(s.chain_id, [s]);
  }

  let count = 0;
  for (const chainMembers of byChain.values()) {
    if (chainMembers.length < 2) continue;
    const minGen = Math.min(...chainMembers.map((m) => m.generation ?? 0));
    count += chainMembers.filter((m) => (m.generation ?? 0) > minGen).length;
  }
  return count;
}

/** Sum of `total_size` across a batch, excluding locked snapshots (never submitted). */
export function reclaimableBytes(batch: readonly BackupSnapshot[]): number {
  return batch.reduce(
    (sum, s) => (s.locked ? sum : sum + (s.total_size ?? 0)),
    0,
  );
}
