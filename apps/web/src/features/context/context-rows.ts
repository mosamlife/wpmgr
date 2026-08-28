import type { KvRowProps } from "@/components/shared/definition-list";
import type { GuidanceSet, RestrictionSet } from "@wpmgr/api";

// Shared DefinitionList row builders for ADR-064's RestrictionSet/GuidanceSet
// — used by both the read-only effective-context preview (Stage A,
// effective-context-preview.tsx, one per layer) and the org/site editors'
// read-only fallback (Stage B, gov-context-editor.tsx, one per subject).
// Kept in one place so the two surfaces render identical field labels/order
// for the same underlying types rather than drifting apart.

export function restrictionRows(restrictions: RestrictionSet): KvRowProps[] {
  return [
    { label: "Forbidden tools", value: joinOrUndefined(restrictions.forbidden_tools) },
    { label: "Forbidden domains", value: joinOrUndefined(restrictions.forbidden_domains) },
    { label: "Forbidden topics", value: joinOrUndefined(restrictions.forbidden_topics) },
  ];
}

export function guidanceRows(guidance: GuidanceSet): KvRowProps[] {
  return [
    { label: "Brand voice", value: guidance.brand_voice || undefined },
    { label: "Audience", value: guidance.audience || undefined },
    { label: "Terminology", value: guidance.terminology || undefined },
    { label: "Style", value: guidance.style || undefined },
  ];
}

function joinOrUndefined(items?: string[]): string | undefined {
  return items && items.length > 0 ? items.join(", ") : undefined;
}
