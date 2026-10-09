import { describe, it, expect } from "vitest";

import { mintCapabilitiesRequest } from "./mint-capabilities";

// What the wizard's mint request carries. The orphan state below (the request
// ticked without the read it needs) cannot be reached through the wizard's own
// boxes, which keep the pair together, so this rule is tested on the builder
// directly. The wizard's boxes are covered in connect-wizard.test.tsx.

const REFUSAL =
  "No capability is selected, so this token would authenticate and be able to reach nothing. Pick at least one capability above, or leave Sites checked. An empty selection is refused rather than becoming the default.";

describe("mintCapabilitiesRequest", () => {
  it("sends the selection as it stands, in the order given", () => {
    expect(
      mintCapabilitiesRequest(["mcp.uptime.read", "mcp.sites.read", "mcp.ability.read"]),
    ).toEqual({
      ok: true,
      capabilities: ["mcp.uptime.read", "mcp.sites.read", "mcp.ability.read"],
    });
  });

  it("sends the request beside the read it needs", () => {
    expect(
      mintCapabilitiesRequest(["mcp.sites.read", "mcp.ability.read", "mcp.ability.request"]),
    ).toEqual({
      ok: true,
      capabilities: ["mcp.sites.read", "mcp.ability.read", "mcp.ability.request"],
    });
  });

  it("does not send the request without the read, and sends the rest", () => {
    expect(mintCapabilitiesRequest(["mcp.sites.read", "mcp.ability.request"])).toEqual({
      ok: true,
      capabilities: ["mcp.sites.read"],
    });
  });

  it("refuses a selection that held nothing but the request, as an empty selection", () => {
    expect(mintCapabilitiesRequest(["mcp.ability.request"])).toEqual({
      ok: false,
      refusal: REFUSAL,
    });
  });

  it("refuses an empty selection with the same words, and never sends an empty list", () => {
    expect(mintCapabilitiesRequest([])).toEqual({ ok: false, refusal: REFUSAL });
  });

  it("does not change the list it was given", () => {
    const before = Object.freeze(["mcp.sites.read", "mcp.ability.request"]);
    expect(() => mintCapabilitiesRequest(before)).not.toThrow();
    expect(before).toEqual(["mcp.sites.read", "mcp.ability.request"]);
  });
});
