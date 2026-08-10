import { describe, it, expect } from "vitest";

import { endpointDivergedFromOrg } from "./email-provider-config";

// GH #380. The control plane only lends the organisation credential to a site
// whose config still matches the organisation's on the fields that decide where
// the credential goes and who it authenticates as. The page has to apply the
// same rule, or it promises a credential that will never be sent.
//
// This mirrors credentialAudienceFields + sameCredentialAudience in
// apps/api/internal/email/service.go. If that list changes, this one does too.

describe("endpointDivergedFromOrg", () => {
  const orgSmtp = {
    host: "smtp.org-relay.example",
    port: 587,
    username: "org@example.com",
    encryption: "tls",
    auth: true,
    auto_tls: false,
  };

  it("an untouched inherited config has not diverged", () => {
    expect(
      endpointDivergedFromOrg("smtp", { ...orgSmtp }, "smtp", orgSmtp),
    ).toBe(false);
  });

  it("pointing the site at another host has diverged", () => {
    expect(
      endpointDivergedFromOrg(
        "smtp",
        { ...orgSmtp, host: "collector.attacker.example" },
        "smtp",
        orgSmtp,
      ),
    ).toBe(true);
  });

  it("changing the login identity has diverged", () => {
    expect(
      endpointDivergedFromOrg(
        "smtp",
        { ...orgSmtp, username: "someone.else@example.com" },
        "smtp",
        orgSmtp,
      ),
    ).toBe(true);
  });

  it("dropping encryption has diverged, because it decides whether the password crosses the network in clear", () => {
    expect(
      endpointDivergedFromOrg(
        "smtp",
        { ...orgSmtp, encryption: "none" },
        "smtp",
        orgSmtp,
      ),
    ).toBe(true);
  });

  it("a number typed back into a text input does not count as a change", () => {
    // The port input writes a string; the org row holds a JSON number.
    expect(
      endpointDivergedFromOrg("smtp", { ...orgSmtp, port: "587" }, "smtp", orgSmtp),
    ).toBe(false);
  });

  it("a field that cannot redirect the credential does not count as a change", () => {
    expect(
      endpointDivergedFromOrg(
        "smtp",
        { ...orgSmtp, auto_tls: true },
        "smtp",
        orgSmtp,
      ),
    ).toBe(false);
  });

  it("switching provider has diverged", () => {
    expect(endpointDivergedFromOrg("sendgrid", {}, "smtp", orgSmtp)).toBe(true);
  });

  it("an API provider with no operator-supplied endpoint never diverges", () => {
    expect(
      endpointDivergedFromOrg("sendgrid", { anything: 1 }, "sendgrid", {}),
    ).toBe(false);
  });

  it("an unknown provider slug is never treated as a match", () => {
    expect(endpointDivergedFromOrg("mystery", {}, "mystery", {})).toBe(true);
  });
});
