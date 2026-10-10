import type { SiteAiMode } from "@wpmgr/api";

// Test fixtures for a site's AI mode, shaped like what the control plane
// writes for GET /api/v1/sites/{siteId}/ai/mode:
//   - the body is SiteAIModeDTO (apps/api/internal/aitrust/dto.go) and
//     SiteAiMode in packages/openapi-client/src/generated/types.gen.ts;
//   - `options` holds one entry per offered mode, ask then ai_drafts, with
//     `reason` null when the caller may choose it (aitrust/service.go,
//     modeOptions);
//   - `kinds` lists a kind only when a reviewed write tool belongs to it, and
//     each decision is `auto` or `ask` as the mode matrix gives it
//     (aitrust/service.go changeKinds, aipolicy/model.go Allows);
//   - `min_agent_version` is aitrust.MinAgentVersion, which is
//     agentcmd.MinAgentVersionForPageCreate (agentcmd/ability_run_contract.go).
// Turning AI editing on sets the mode to `ai_drafts` with source
// `enable_default` (the AiModeSource contract in the OpenAPI document).

export const MIN_AGENT_VERSION = "0.61.156";

/** A site with AI editing on, set to Auto for AI drafts when a person turned it on. */
export function siteAiMode(siteId: string, over: Partial<SiteAiMode> = {}): SiteAiMode {
  return {
    site_id: siteId,
    mode: "ai_drafts",
    source: "enable_default",
    version: 1,
    set_by_user_id: "u",
    set_by_name: "A",
    set_by_account_deleted: false,
    set_at: "2026-10-09T12:00:00Z",
    setter_valid: true,
    ai_paused: false,
    min_agent_version: MIN_AGENT_VERSION,
    options: [
      { mode: "ask", choosable: true, reason: null },
      { mode: "ai_drafts", choosable: true, reason: null },
    ],
    kinds: [
      {
        change_class: "ai_draft",
        name: "changes to the AI's own drafts",
        abilities: [{ name: "wpmgr/page-create", title: "Create a draft page" }],
        decisions: [
          { mode: "ask", outcome: "ask" },
          { mode: "ai_drafts", outcome: "auto" },
        ],
      },
    ],
    ...over,
  };
}
