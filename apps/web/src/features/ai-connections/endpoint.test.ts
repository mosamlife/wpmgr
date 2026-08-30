import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { mcpEndpointUrl } from "./endpoint";
import { MCP_TRANSPORT_PATH } from "./client-table";

// FOUND BY THE MUTATION SWEEP. Replacing the origin lookup with a literal
// `https://app.wpmgr.app` was caught only by tsc (the `origin` binding went
// unused), and tsc catching it is an accident of that particular mutation --
// a literal written in place of the whole expression would have compiled
// cleanly and printed the wrong endpoint to every self-hosted install, as
// confidently as the right one. This file pins the property itself.

describe("mcpEndpointUrl is derived from this deployment's origin", () => {
  it("returns the running origin plus the transport path", () => {
    // jsdom serves a real origin, and it is not the managed host. If the
    // implementation ever hardcodes one, this equality breaks.
    expect(window.location.origin.length).toBeGreaterThan(0);
    expect(mcpEndpointUrl()).toBe(`${window.location.origin}${MCP_TRANSPORT_PATH}`);
  });

  it("does not emit the managed hostname when running somewhere else", () => {
    expect(window.location.origin).not.toContain("app.wpmgr.app");
    expect(mcpEndpointUrl()).not.toContain("app.wpmgr.app");
  });

  it("ignores the fallback while a window is present", () => {
    // The fallback is for a windowless environment only; letting it win here
    // would let a caller override the real origin by accident.
    expect(mcpEndpointUrl("https://somewhere.else")).toBe(
      `${window.location.origin}${MCP_TRANSPORT_PATH}`,
    );
  });

  it("carries no hostname literal in its executable source", () => {
    // The equality above only fails while jsdom's origin differs from whatever
    // literal someone writes. This closes that gap directly.
    //
    // COMMENTS ARE STRIPPED FIRST. The doc comment on mcpEndpointUrl quotes the
    // managed URL as the thing not to write, and a scan that cannot tell code
    // from prose reddens correct work -- which is how a guard gets switched off.
    const source = readFileSync(
      join(process.cwd(), "src", "features", "ai-connections", "endpoint.ts"),
      "utf8",
    );
    const code = source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
    // The scan has to still be looking at something.
    expect(code).toContain("window.location.origin");
    expect(code).not.toMatch(/https?:\/\/[a-z0-9.-]+/i);
  });
});
