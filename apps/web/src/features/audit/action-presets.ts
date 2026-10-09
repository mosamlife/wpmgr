export interface ActionPreset {
  readonly label: string;
  /** A prefix of the audit action, "" for every action. */
  readonly value: string;
}

// Quick-filter presets for the action field. Prefixes match the real
// emitted keys (apps/api/internal/audit/audit.go + each domain's Record call
// sites) — the previous "cache."/"settings."/"security." chips silently
// missed site.cache.*, smtp.settings.*, and site_security_*/auth.2fa.*.
export const ACTION_PRESETS: readonly ActionPreset[] = [
  { label: "All events", value: "" },
  { label: "File manager", value: "site.files." },
  { label: "Backups", value: "backup." },
  { label: "Restores", value: "restore." },
  { label: "Updates", value: "update." },
  { label: "Security", value: "site_security" },
  { label: "2FA", value: "auth.2fa." },
  { label: "SMTP", value: "smtp.settings." },
  { label: "Cache", value: "site.cache." },
  // What an AI connection asked for and was refused, including the page
  // requests WPMgr turns away before they become a card.
  { label: "Blocked AI calls", value: "mcp.tool.denied" },
];
