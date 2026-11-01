import { describe, it, expect } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { AuditEntry } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { AuditEntryRow } from "./audit-row";
import type { SiteMin } from "./types";

// GH #201 — fleet Audit log: backup/restore/update entries are site-scoped
// but carry a target_type other than "site" ("backup_snapshot",
// "update_task", "backup_schedule", ...). The old TargetSlot only resolved +
// rendered a site name when target_type === "site", so every one of these
// rows showed the raw wire target_type string ("backup_snapshot",
// "update_task") instead of which site the backup/restore/update actually
// happened on. This pins the fix: for a non-"site" target_type, the row now
// derives a candidate site id from `metadata.site_id` (or, for
// "backup_schedule" specifically, `target_id` — the schedule row's id IS the
// site id, see backup/handler.go's recordScheduleChange) and resolves it
// against the known sites list, falling back to the original raw display
// when nothing resolves.

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const OTHER_SITE_ID = "22222222-2222-2222-2222-222222222222";
const UNKNOWN_SITE_ID = "99999999-9999-9999-9999-999999999999";

const SITES: SiteMin[] = [
  { id: SITE_ID, name: "Acme Blog", url: "https://acme.example" },
  { id: OTHER_SITE_ID, name: "Other Site", url: "https://other.example" },
];

let seq = 0;
function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  seq += 1;
  return {
    id: overrides.id ?? `entry-${seq}`,
    tenant_id: "tenant-1",
    actor_type: "user",
    actor_id: "user-1",
    action: "backup.started",
    target_type: "backup_snapshot",
    target_id: `snap-${seq}`,
    prev_hash: "prev",
    hash: "hash",
    created_at: "2026-07-08T12:00:00Z",
    ...overrides,
  };
}

describe("AuditEntryRow target site resolution (GH #201)", () => {
  it("resolves a backup_snapshot entry's site name from metadata.site_id, not the raw target_type", () => {
    const e = entry({
      target_type: "backup_snapshot",
      target_id: "snap-1",
      metadata: { site_id: SITE_ID, full: true },
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    // The resolved site name is shown...
    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
    // ...and the raw target_type string is never rendered as the target
    // label. This is the non-vacuous half of the assertion: the pre-fix
    // TargetSlot renders exactly "backup_snapshot" here, so this line fails
    // against the old target_type-only code and passes only with the fix.
    expect(screen.queryByText("backup_snapshot")).not.toBeInTheDocument();
  });

  it("resolves a backup_schedule entry's site via target_id (the schedule id IS the site id; no metadata.site_id present)", () => {
    const e = entry({
      action: "backup.schedule.changed",
      target_type: "backup_schedule",
      target_id: OTHER_SITE_ID,
      metadata: { cadence: "daily", kind: "full", enabled: true },
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    expect(screen.getByText("Other Site")).toBeInTheDocument();
    expect(screen.queryByText("backup_schedule")).not.toBeInTheDocument();
  });

  it("falls back to the raw target_type when metadata.site_id matches no known site, without crashing", () => {
    const e = entry({
      target_type: "backup_snapshot",
      target_id: "snap-2",
      metadata: { site_id: UNKNOWN_SITE_ID },
    });

    expect(() =>
      renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />),
    ).not.toThrow();

    expect(screen.getByText("backup_snapshot")).toBeInTheDocument();
  });

  it("falls back to the raw target_type when the entry carries no site id at all (e.g. update.run.created)", () => {
    const e = entry({
      action: "update.run.created",
      target_type: "update_run",
      target_id: "run-1",
      metadata: { dry_run: false, task_count: 3 },
    });

    expect(() =>
      renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />),
    ).not.toThrow();

    expect(screen.getByText("update_run")).toBeInTheDocument();
  });

  it("still resolves a plain target_type: \"site\" entry unchanged", () => {
    const e = entry({
      action: "site.cache.purged",
      target_type: "site",
      target_id: SITE_ID,
      metadata: {},
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
  });
});

// BF-E W3 (section 5.4 of the design, the owner's acceptance copy): a page
// request WPMgr refuses before it becomes a card has no card to show it on.
// The refusal's audit row (action mcp.tool.denied, written by
// apps/api/internal/mcp/service.go recordToolDeniedWith with the refusal's own
// keys, apps/api/internal/mcp/page_structure.go withBuilderEditTarget) is where
// a person reads that it happened, as a grey sentence in plain words.
describe("AuditEntryRow for a page request WPMgr refused before showing it", () => {
  const GRANT_ID = "7d1f2c3a-5b6e-4f70-8a91-b2c3d4e5f607";

  function denied(meta: Record<string, unknown>, over: Partial<AuditEntry> = {}): AuditEntry {
    return entry({
      actor_type: "assistant",
      actor_id: GRANT_ID,
      action: "mcp.tool.denied",
      target_type: "mcp_tool",
      target_id: "site_ability_run",
      metadata: { grant_name: "Claude", held_capabilities: 3, scoped_sites: 1, ...meta },
      ...over,
    });
  }

  const renderRow = (e: AuditEntry) =>
    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />, { withRouter: true });

  it("says a part of the page was not there, as a grey sentence naming the connection", async () => {
    renderRow(
      denied({
        ability: "wpmgr/page-edit",
        post_id: 418,
        code: "node_not_found",
        refusal_reason: "ability_precheck_refused",
      }),
    );
    const row = await screen.findByTestId("ai-refusal-row");
    expect(row.getAttribute("data-kind")).toBe("refused");
    const sentence = row.querySelector("p");
    expect(sentence?.textContent).toBe(
      'The AI asked to change "#418" and WPMgr refused the request before showing it to you: it named a part of the page that is not there.',
    );
    expect(sentence).toHaveClass("text-muted-foreground");
    expect(row.textContent).toContain("Connection: Claude");
    // Not the red treatment of a blocked tool.
    expect(screen.queryByText("Blocked AI tool call")).not.toBeInTheDocument();
    expect(screen.queryByText("Denied")).not.toBeInTheDocument();
  });

  it("says which kind of page it was when the page is not a draft WPMgr created", async () => {
    renderRow(
      denied({
        ability: "wpmgr/page-edit",
        post_id: 418,
        code: "target_not_eligible",
        agent_code: "target_not_eligible",
        detail: "not_draft",
        refusal_reason: "ability_agent_refused",
      }),
    );
    const row = await screen.findByTestId("ai-refusal-row");
    expect(row.getAttribute("data-kind")).toBe("not_eligible");
    expect(row.querySelector("p")?.textContent).toBe(
      'The AI asked to change "#418", which is not a draft WPMgr created for it (it is no longer a draft, for example because it was published). WPMgr refused.',
    );
  });

  it("shows the connection's name as text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)"><b>Claude</b>';
    const { container } = renderRow(
      denied({ ability: "wpmgr/page-edit", post_id: 418, code: "page_too_large", grant_name: hostile }),
    );
    const row = await screen.findByTestId("ai-refusal-row");
    expect(row.textContent).toContain(hostile);
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect(row.querySelector("bdi")?.textContent).toBe(hostile);
  });

  it("leaves any other blocked tool call as a red Denied row with its usual label", async () => {
    renderRow(denied({ refusal_reason: "capability_not_held" }));
    expect(await screen.findByText("Blocked AI tool call")).toBeInTheDocument();
    expect(screen.getByText("Denied")).toBeInTheDocument();
    expect(screen.queryByTestId("ai-refusal-row")).not.toBeInTheDocument();
  });

  it("leaves a blocked page request with a code it has no words for as a usual Denied row", async () => {
    renderRow(denied({ ability: "wpmgr/page-edit", post_id: 418, code: "something_new" }));
    expect(await screen.findByText("Blocked AI tool call")).toBeInTheDocument();
    expect(screen.queryByTestId("ai-refusal-row")).not.toBeInTheDocument();
  });

  it("shows the sentence only on a refusal, whatever else carries the same keys", async () => {
    renderRow(
      denied(
        { ability: "wpmgr/page-edit", post_id: 418, code: "node_not_found" },
        { action: "mcp.tool.called" },
      ),
    );
    expect(await screen.findByText(/AI tool call/)).toBeInTheDocument();
    expect(screen.queryByTestId("ai-refusal-row")).not.toBeInTheDocument();
  });
});

// GH #968: the row the control plane writes when a site's own email provider is
// removed. Target type "site" and the metadata keys are the ones the handler
// records (deleteConfig and record in apps/api/internal/email/handler.go).
describe("AuditEntryRow for a removed email provider (GH #968)", () => {
  it("reads as a plain, marked write on the site it happened to, not as a raw key", async () => {
    const e = entry({
      action: "site.email.config.reset",
      target_type: "site",
      target_id: SITE_ID,
      metadata: { provider: "smtp", fallback: "org_default", org_default_synced: true },
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />, { withRouter: true });

    expect(await screen.findByText("Removed email provider")).toBeInTheDocument();
    expect(screen.getByText("Write")).toBeInTheDocument();
    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
    expect(screen.queryByText("site.email.config.reset")).not.toBeInTheDocument();
  });
});

// D1004: the rows the control plane writes for a site's backup key. Target type
// "site", actor "system" and the metadata keys are the ones recorded by
// recordRecipientChangeRejected, recordRecipientCleared and
// applyAgentAgeRecipient in apps/api/internal/site/service.go. The reason codes
// are already_set, concurrent_first_set, agent_key_not_current (same file) and
// re_enrollment (reasonReEnrollment).
describe("AuditEntryRow for the backup key events (D1004)", () => {
  const renderRow = (e: AuditEntry) =>
    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />, { withRouter: true });

  const keyEvent = (action: string, metadata: Record<string, unknown>) =>
    entry({ actor_type: "system", action, target_type: "site", target_id: SITE_ID, metadata });

  const refused = (reason: string) =>
    keyEvent("site.backup.recipient.change_rejected", {
      current_recipient: "age1recordedkey",
      proposed_recipient: "age1reportedkey",
      source: "agent_metadata",
      agent_version: "0.61.167",
      reason,
    });

  // A person opening the row clicks its summary line.
  const openRow = (title: string) => fireEvent.click(screen.getByText(title));

  it.each([
    ["already_set", "WPMgr already had a key for this site"],
    ["concurrent_first_set", "Another report recorded a key first"],
    ["agent_key_not_current", "The report came from an old connection"],
  ])("titles a refused change in plain words and explains %s once opened", async (reason, plain) => {
    renderRow(refused(reason));

    // Closed: the plain title on the site it happened to, and neither the raw
    // event key nor the explanation yet.
    expect(await screen.findByText("Backup key change refused")).toBeInTheDocument();
    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
    expect(screen.queryByText("site.backup.recipient.change_rejected")).not.toBeInTheDocument();
    expect(screen.queryByText(plain)).not.toBeInTheDocument();

    openRow("Backup key change refused");

    expect(await screen.findByText(plain)).toBeInTheDocument();
    expect(screen.getByText("Reason")).toBeInTheDocument();
    // The wire code never becomes visible text.
    expect(screen.queryByText(reason)).not.toBeInTheDocument();
  });

  it("says a reconnect cleared the key, and why, once opened", async () => {
    renderRow(
      keyEvent("site.backup.recipient.cleared", {
        previous_recipient: "age1recordedkey",
        reason: "re_enrollment",
      }),
    );

    expect(await screen.findByText("Backup key cleared by reconnect")).toBeInTheDocument();
    openRow("Backup key cleared by reconnect");

    expect(await screen.findByText("The site was reconnected")).toBeInTheDocument();
    expect(screen.getByText("Reason")).toBeInTheDocument();
  });

  it("titles the first recording of a key, which carries no reason to show", async () => {
    renderRow(
      keyEvent("site.backup.recipient.set", {
        recipient: "age1reportedkey",
        source: "agent_metadata",
        agent_version: "0.61.167",
      }),
    );

    expect(await screen.findByText("Backup key recorded")).toBeInTheDocument();
    openRow("Backup key recorded");

    expect(await screen.findByText("Event key")).toBeInTheDocument();
    expect(screen.queryByText("Reason")).not.toBeInTheDocument();
  });

  it("keeps a denial's reason in its Why callout, with no extra Reason line", async () => {
    renderRow(
      entry({
        action: "site.files.delete.denied",
        target_type: "site",
        target_id: SITE_ID,
        metadata: { reason: "insufficient_permission" },
      }),
    );

    expect(await screen.findByText("Blocked file deletion")).toBeInTheDocument();
    openRow("Blocked file deletion");

    expect(await screen.findByText("Why:")).toBeInTheDocument();
    expect(
      screen.getByText("The actor's role does not have permission for this action"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Reason")).not.toBeInTheDocument();
  });

  it("does not add a Reason line to any other row that carries a reason", async () => {
    // grant_inactive is a real not_sent reason (apps/api/internal/assistantrequest/model.go).
    renderRow(
      entry({
        action: "assistant.request.not_sent",
        target_type: "site",
        target_id: SITE_ID,
        metadata: { reason: "grant_inactive" },
      }),
    );

    expect(await screen.findByText("AI cache-clear request not sent")).toBeInTheDocument();
    openRow("AI cache-clear request not sent");

    expect(await screen.findByText("Event key")).toBeInTheDocument();
    expect(screen.queryByText("Reason")).not.toBeInTheDocument();
    expect(screen.queryByText("The connection was revoked or had expired")).not.toBeInTheDocument();
  });
});
