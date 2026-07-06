/**
 * Tests for the audit metadata helpers, focused on the honest delivery-status
 * rendering for uptime.alert.sent (GitHub #144): the control plane now emits
 * email_status/webhook_status ("sent"/"skipped"/"failed") plus a reason code
 * instead of a bare "emailed"/"webhooked" boolean that always read "Yes"
 * regardless of whether delivery actually happened.
 *
 * Following the project convention (labels.test.ts, group-runs.test.ts):
 * pure-function tests only; no React renderer, no DOM.
 */
import { describe, it, expect } from "vitest";

import {
  formatDeliveryStatus,
  humanizeDeliveryReason,
  humanizeDeliveryStatus,
} from "./metadata";

describe("humanizeDeliveryStatus", () => {
  it("maps the three known status codes to sentence-case words", () => {
    expect(humanizeDeliveryStatus("sent")).toBe("Sent");
    expect(humanizeDeliveryStatus("skipped")).toBe("Skipped");
    expect(humanizeDeliveryStatus("failed")).toBe("Failed");
  });

  it("falls back to sentence case for an unknown status code", () => {
    expect(humanizeDeliveryStatus("throttled")).toBe("Throttled");
  });
});

describe("humanizeDeliveryReason", () => {
  it("maps the known reason codes to readable text", () => {
    expect(humanizeDeliveryReason("smtp_not_configured")).toBe("SMTP not configured");
    expect(humanizeDeliveryReason("no_recipients")).toBe("No recipients");
    expect(humanizeDeliveryReason("no_recipients_configured")).toBe("No recipients");
    expect(humanizeDeliveryReason("resolve_smtp")).toBe("SMTP lookup failed");
  });

  it("passes an unknown reason code through unchanged", () => {
    expect(humanizeDeliveryReason("some_future_reason")).toBe("some_future_reason");
  });
});

describe("formatDeliveryStatus", () => {
  it("renders a sent outcome with no reason suffix", () => {
    expect(formatDeliveryStatus("sent", null)).toBe("Sent");
    // Even a stray reason on a "sent" row is ignored — only non-"sent"
    // outcomes surface a reason.
    expect(formatDeliveryStatus("sent", "smtp_not_configured")).toBe("Sent");
  });

  it("renders a skipped outcome with its known reason", () => {
    expect(formatDeliveryStatus("skipped", "smtp_not_configured")).toBe(
      "Skipped (SMTP not configured)",
    );
  });

  it("renders a failed outcome with its known reason", () => {
    expect(formatDeliveryStatus("failed", "resolve_smtp")).toBe(
      "Failed (SMTP lookup failed)",
    );
  });

  it("renders bare status word when no reason is present", () => {
    expect(formatDeliveryStatus("skipped", null)).toBe("Skipped");
  });

  it("passes an unknown reason through unchanged inside the parens", () => {
    expect(formatDeliveryStatus("failed", "some_future_reason")).toBe(
      "Failed (some_future_reason)",
    );
  });
});
