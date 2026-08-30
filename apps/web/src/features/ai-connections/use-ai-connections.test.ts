import { describe, it, expect } from "vitest";

import { parseConnectionList } from "./use-ai-connections";
import type { AiConnection } from "./connection-model";

// THE WIRE SHAPE IS THE BACKEND'S, AND THIS FILE IS WHERE THE TWO ARE COMPARED.
//
// Every fixture below is the shape apps/api/internal/mcp/dto.go actually
// emits -- connectionListDTO wrapping connectionDTO, with protocolReportDTO's
// {state, version} pair and pointers rendered as explicit nulls. The frontend
// model was written BEFORE that endpoint existed and disagreed with it in three
// places: it had three protocol states where the server has four, it carried a
// "paused" status the server cannot emit, and it expected a bare array. Nothing
// had ever compared them until this file.

function row(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    name: "Fleet manager",
    status: "active",
    site_scope_mode: "all",
    scopes: ["mcp:read"],
    created_at: "2026-08-01T00:00:00Z",
    reported_client_name: "claude-code",
    reported_client_version: "2.1.0",
    protocol: { state: "recognised", version: "2025-11-25" },
    last_used_at: "2026-08-29T10:00:00Z",
    revoked_at: null,
    ...over,
  };
}

/** Parse a one-row payload the way the queryFn does, and return that row. */
function one(over: Record<string, unknown> = {}): AiConnection {
  const out = parseConnectionList({ connections: [row(over)] });
  const first = out[0];
  if (first === undefined) throw new Error("expected one connection");
  return first;
}

describe("the wire shape parses into the model", () => {
  it("reads the object wrapper, not a bare array", () => {
    // dto.go wraps deliberately: a bare `[]` and an error body are both valid
    // JSON, so a caller that ignores the status can decode a failure into a
    // zero-length list.
    const out = parseConnectionList({ connections: [row()] });
    expect(out).toHaveLength(1);
    expect(out[0]?.name).toBe("Fleet manager");
  });

  it("refuses a bare array", () => {
    expect(() => parseConnectionList([row()])).toThrow();
  });

  it("refuses the house error envelope rather than reading it as empty", () => {
    // The exact confusion the wrapper exists to prevent.
    expect(() =>
      parseConnectionList({ code: "forbidden", message: "nope" }),
    ).toThrow();
  });

  it("reads a genuinely empty organisation as an empty list", () => {
    // The over-fire half: a real empty org must still parse.
    expect(parseConnectionList({ connections: [] })).toEqual([]);
  });
});

describe("the four protocol states survive the wire", () => {
  it("maps never_connected without collapsing it into absent", () => {
    const c = one(({ protocol: { state: "never_connected", version: null } }));
    expect(c.protocolHeader.kind).toBe("never_connected");
  });

  it("maps absent without inventing a version", () => {
    const c = one(({ protocol: { state: "absent", version: null } }));
    expect(c.protocolHeader.kind).toBe("absent");
    expect("version" in c.protocolHeader).toBe(false);
  });

  it("maps recognised and unrecognised with their versions", () => {
    const rec = one(({ protocol: { state: "recognised", version: "2025-11-25" } }));
    expect(rec.protocolHeader).toEqual({ kind: "recognised", version: "2025-11-25" });
    const unr = one(({ protocol: { state: "unrecognised", version: "2024-11-05" } }),
    );
    expect(unr.protocolHeader).toEqual({ kind: "unrecognised", version: "2024-11-05" });
  });

  it("refuses an unknown fifth state rather than coercing it", () => {
    // A closed enum. Coercing an unknown state into `absent` would be this
    // codebase's signature defect, wearing the exact costume the server avoided
    // by not flattening the state into a nullable string.
    expect(() =>
      parseConnectionList({ connections: [row({ protocol: { state: "weird", version: null } })] }),
    ).toThrow();
  });

  it("reports a contradicted pair as unreadable, NEVER as absent", () => {
    // THE DEFECT THIS REPLACES. Both of these used to map to `absent`, which is
    // a confident claim about the CLIENT -- "it connected and sent no header".
    // A response that disagrees with itself supports no such claim. dto.go only
    // populates Version for these two states, so a null here means we could not
    // read the answer, which is a fact about us and not about anyone else.
    for (const state of ["recognised", "unrecognised"]) {
      const c = one({ protocol: { state, version: null } });
      expect(c.protocolHeader.kind, `${state} with a null version`).toBe("unreadable");
      // Named explicitly, so the wrong answer cannot come back by another route.
      expect(c.protocolHeader.kind).not.toBe("absent");
      expect(c.protocolHeader.kind).not.toBe("never_connected");
    }
  });

  it("does not put a version on an unreadable report", () => {
    const c = one({ protocol: { state: "recognised", version: null } });
    expect("version" in c.protocolHeader).toBe(false);
  });

  it("still maps a well-formed absent, so the guard does not over-fire", () => {
    // Routing every version-less state to `unreadable` would be the opposite
    // defect: a genuine absent must still read as absent.
    expect(one({ protocol: { state: "absent", version: null } }).protocolHeader.kind).toBe(
      "absent",
    );
    expect(
      one({ protocol: { state: "never_connected", version: null } }).protocolHeader.kind,
    ).toBe("never_connected");
  });
});

describe("null is never coerced into a value", () => {
  it("maps last_used_at null onto never, not a date", () => {
    const c = one(({ last_used_at: null }));
    expect(c.lastUsed).toEqual({ kind: "never" });
  });

  it("keeps a real last_used_at", () => {
    const c = one(({ last_used_at: "2026-08-29T10:00:00Z" }));
    expect(c.lastUsed).toEqual({ kind: "at", iso: "2026-08-29T10:00:00Z" });
  });

  it("keeps a null reported client name as null, never backfilled from name", () => {
    const c = one(({ reported_client_name: null, reported_client_version: null }));
    expect(c.reportedClientName).toBeNull();
    // Never the operator's chosen name: one is the client's claim, the other is
    // the operator's, and they can disagree.
    expect(c.reportedClientName).not.toBe("Fleet manager");
  });

  it("keeps a version reported without a name", () => {
    const c = one(({ reported_client_name: null, reported_client_version: "0.9" }));
    expect(c.reportedClientVersion).toBe("0.9");
  });

  it("maps revoked_at null onto null, not a date", () => {
    expect(one(({ revoked_at: null })).revokedAt).toBeNull();
  });
});

describe("status matches what the server can actually emit", () => {
  it("accepts the two values mcp_grants_status_check permits", () => {
    expect(one(({ status: "active" })).status).toBe("active");
    expect(
      one(({ status: "revoked", revoked_at: "2026-08-29T11:00:00Z" })).status,
    ).toBe("revoked");
  });

  it("refuses a status the server cannot return", () => {
    // "paused" was in the model before the endpoint existed. There is no
    // endpoint that produces it and no column that stores it.
    expect(() =>
      parseConnectionList({ connections: [row({ status: "paused" })] }),
    ).toThrow();
  });
});
