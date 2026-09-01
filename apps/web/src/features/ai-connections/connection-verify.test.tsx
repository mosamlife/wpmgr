import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { ConnectionVerifyBody } from "./connection-verify";
import {
  ESCALATE_UNUSED_AFTER_MS,
  NOTHING_ARRIVED_AFTER_MS,
  shouldKeepPolling,
  verifyVerdict,
} from "./connection-status-model";
import { parseConnectionStatus, type ConnectionStatusWire } from "./use-connection-status";

// THE THREE STATES THIS SCREEN EXISTS TO KEEP APART, AND THE FOURTH IT MUST NOT
// INVENT.
//
// never connected / connected and healthy / connected and something is wrong
// are the three. The fourth is a CAUSE for the first, which this endpoint does
// not have: `refusal` is hard-coded null in connection_status.go:751, so a
// client that tried and was turned away is byte-identical to one that was never
// started. Every fixture below is the shape connectionStatusResponse
// (connection_status.go:738) actually emits.

const CREATED = "2026-08-15T09:00:00Z";

function wire(over: Record<string, unknown> = {}): ConnectionStatusWire {
  return parseConnectionStatus({
    id: "11111111-1111-1111-1111-111111111111",
    status: "active",
    created_at: CREATED,
    expires_at: "2027-08-15T09:00:00Z",
    handshake: {
      state: "awaiting_client",
      recorded_at: null,
      reported_client_name: null,
      reported_client_version: null,
      protocol: {
        state: "never_connected",
        version: null,
        assumed: null,
        floor: "2025-03-26",
        target: "2025-06-18",
        supported: ["2025-03-26", "2025-06-18"],
      },
      refusal: null,
    },
    first_call: {
      state: "awaiting_call",
      called_at: null,
      tool_name: null,
      audit_event_id: null,
      last_used_at: null,
      partial: null,
    },
    observed_at: CREATED,
    poll_after_ms: 2000,
    ...over,
  });
}

/** A handshake block for a client that connected and reported everything. */
function connectedHandshake(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    state: "connected",
    recorded_at: "2026-08-15T09:00:04Z",
    reported_client_name: "claude-code",
    reported_client_version: "2.4.1",
    protocol: {
      state: "recognised",
      version: "2025-06-18",
      assumed: null,
      floor: "2025-03-26",
      target: "2025-06-18",
      supported: ["2025-03-26", "2025-06-18"],
    },
    refusal: null,
    ...over,
  };
}

// RENDER THROUGH THE REAL ROUTER AND QueryClient, and await the first paint.
// render.tsx's module doc is explicit that RouterProvider resolves its initial
// match on a microtask, so the first assertion in every test below must be a
// findBy*. Awaiting the wrapper here is what makes the getBy* that follow safe.
async function renderBody(w: ConnectionStatusWire, now?: Date) {
  const r = renderWithProviders(<ConnectionVerifyBody wire={w} now={now} />, {
    withRouter: true,
  });
  await screen.findByTestId("verify-root");
  return r;
}

// ---------------------------------------------------------------------------
// The wire schema is the Go DTO's, and this is where the two are compared.
// ---------------------------------------------------------------------------

describe("the status wire shape parses", () => {
  it("accepts the never-connected response the server actually sends", () => {
    const w = wire();
    expect(w.handshake.state).toBe("awaiting_client");
    expect(w.first_call.state).toBe("awaiting_call");
    expect(w.poll_after_ms).toBe(2000);
  });

  it("refuses a handshake state this build does not know", () => {
    // A fifth member must fail the parse rather than being coerced into
    // awaiting_client, which would report a client that did something as one
    // that did nothing.
    expect(() =>
      wire({ handshake: { ...connectedHandshake(), state: "connected_and_shouting" } }),
    ).toThrow();
  });

  it("refuses the house error envelope rather than reading it as a status", () => {
    expect(() => parseConnectionStatus({ code: "forbidden", message: "nope" })).toThrow();
  });

  it("keeps `assumed` as its own field, never folded into `version`", () => {
    // The separation is the whole point: one is what the client sent, the other
    // is what we assumed on its behalf.
    const w = wire({
      handshake: connectedHandshake({
        state: "connected_protocol_assumed",
        protocol: {
          state: "absent",
          version: null,
          assumed: "2025-03-26",
          floor: "2025-03-26",
          target: "2025-06-18",
          supported: ["2025-03-26"],
        },
      }),
    });
    expect(w.handshake.protocol.version).toBeNull();
    expect(w.handshake.protocol.assumed).toBe("2025-03-26");
  });
});

// ---------------------------------------------------------------------------
// State 1 -- never connected. The shipped default, and NOT a failure.
// ---------------------------------------------------------------------------

describe("never connected", () => {
  it("reads as waiting, not as a failure, in the first five minutes", async () => {
    await renderBody(wire(), new Date(Date.parse(CREATED) + 30_000));
    expect(screen.getByTestId("handshake-waiting")).toBeInTheDocument();
    expect(screen.getByText(/Nothing is wrong yet/i)).toBeInTheDocument();
    // The failure copy must be absent, not merely quieter.
    expect(screen.queryByTestId("handshake-silent")).toBeNull();
  });

  it("crosses to 'nothing has reached us' at the five-minute mark", async () => {
    await renderBody(wire(), new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS));
    expect(screen.getByTestId("handshake-silent")).toBeInTheDocument();
    expect(
      screen.getByText(/The connection exists and the key is valid/i),
    ).toBeInTheDocument();
  });

  it("counts the days on a long-unused credential", async () => {
    const elevenDays = Date.parse(CREATED) + 11 * 24 * 60 * 60 * 1000;
    await renderBody(wire(), new Date(elevenDays));
    // EXACT VALUE, not a shape. "11 days ago" is wrong if the arithmetic is
    // wrong, and a regex for /days ago/ would pass on "NaN days ago".
    expect(
      screen.getByText(/created 11 days ago and no client has ever opened a session/i),
    ).toBeInTheDocument();
  });

  it("escalates the wording past thirty days without calling it broken", async () => {
    await renderBody(wire(), new Date(Date.parse(CREATED) + ESCALATE_UNUSED_AFTER_MS));
    expect(
      screen.getByText(/An unused credential is still a credential/i),
    ).toBeInTheDocument();
  });

  it("states that it cannot tell a refusal from a never-started client", async () => {
    // THE ANTI-INVENTION ASSERTION. The wireframe draws "3 attempts from
    // 198.51.100.24 presented a key we do not recognise"; the endpoint has no
    // such fact, so the screen must admit the gap instead of filling it.
    await renderBody(wire(), new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS));
    expect(screen.getByTestId("refusal-gap")).toHaveTextContent(
      /cannot tell from here whether a client tried and was turned away/i,
    );
  });

  it("never renders an attempt count or a source address", async () => {
    const { container } = await renderBody(
      wire(),
      new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS),
    );
    expect(container.textContent).not.toMatch(/attempts?\s+from/i);
    expect(container.textContent).not.toMatch(/\d+\.\d+\.\d+\.\d+/);
  });

  it("never says 'nothing has reached us' about a connection that has demonstrably been used", async () => {
    // GH #636: tools/call is served without a recorded initialize, so a live,
    // working connection can carry an unrecorded handshake. The naive render
    // prints "Nothing has reached us from this connection" directly above "It
    // read your fleet". Both cannot be true, and the false one is the absence.
    await renderBody(
      wire({
        first_call: {
          state: "succeeded",
          called_at: "2026-08-15T09:21:04Z",
          tool_name: "fleet_updates_pending",
          audit_event_id: null,
          last_used_at: "2026-08-15T09:21:04Z",
          partial: null,
        },
      }),
      new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS),
    );
    expect(screen.getByTestId("handshake-contradicted")).toBeInTheDocument();
    expect(screen.queryByTestId("handshake-silent")).toBeNull();
    // And the success beneath it still stands: the call really did happen.
    expect(screen.getByTestId("firstcall-succeeded")).toBeInTheDocument();
  });

  it("treats last_used_at alone as enough to disprove 'nothing has reached us'", async () => {
    // last_used_at is too weak to prove a READ (tools/list stamps it) and
    // plenty to disprove an absence -- something set it.
    await renderBody(
      wire({
        first_call: {
          state: "awaiting_call",
          called_at: null,
          tool_name: null,
          audit_event_id: null,
          last_used_at: "2026-08-15T09:00:05Z",
          partial: null,
        },
      }),
      new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS),
    );
    expect(screen.getByTestId("handshake-contradicted")).toBeInTheDocument();
    // ...and it still must NOT be upgraded into a successful first read.
    expect(screen.getByTestId("firstcall-none")).toBeInTheDocument();
  });

  it("still says nothing arrived when nothing actually has", async () => {
    // THE OVER-FIRE HALF. The contradiction branch must not swallow the real
    // never-used case, which is the state of most connections.
    await renderBody(wire(), new Date(Date.parse(CREATED) + NOTHING_ARRIVED_AFTER_MS));
    expect(screen.getByTestId("handshake-silent")).toBeInTheDocument();
    expect(screen.queryByTestId("handshake-contradicted")).toBeNull();
  });

  it("does not become fresh again when created_at is unreadable", async () => {
    // An unparseable date must not floor the age to zero and pin the screen in
    // "any second now" forever.
    await renderBody(wire({ created_at: "not-a-date" }), new Date(Date.parse(CREATED)));
    expect(screen.getByTestId("handshake-silent")).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// State 2 -- connected and healthy.
// ---------------------------------------------------------------------------

describe("connected and healthy", () => {
  const healthy = () =>
    wire({
      handshake: connectedHandshake(),
      first_call: {
        state: "succeeded",
        called_at: "2026-08-15T09:21:04Z",
        tool_name: "fleet_updates_pending",
        audit_event_id: "22222222-2222-2222-2222-222222222222",
        last_used_at: "2026-08-15T09:21:04Z",
        partial: null,
      },
    });

  it("names the tool the client actually called", async () => {
    await renderBody(healthy());
    expect(screen.getByTestId("firstcall-succeeded")).toBeInTheDocument();
    // EXACT NAME. "something rendered" would pass while showing another
    // connection's tool.
    expect(screen.getByTestId("verify-tool-name")).toHaveTextContent(
      "fleet_updates_pending",
    );
  });

  it("names the client's own reported version, not the operator's choice", async () => {
    await renderBody(healthy());
    expect(screen.getByTestId("verify-client-name")).toHaveTextContent("claude-code 2.4.1");
  });

  it("shows the negotiated revision the client sent", async () => {
    await renderBody(healthy());
    expect(screen.getByTestId("verify-protocol")).toHaveTextContent("2025-06-18");
  });

  it("does not claim the call covered every site", async () => {
    // S29-9's P frame wants "22 sites -- every site in its scope". The response
    // carries no coverage at all, so a success must say so.
    await renderBody(healthy());
    expect(screen.getByTestId("coverage-gap")).toHaveTextContent(
      /do not record how many sites that call reached/i,
    );
  });

  it("stops polling once the first call has succeeded", () => {
    const w = healthy();
    expect(verifyVerdict(w, new Date(w.observed_at))).toMatchObject({
      firstCall: { kind: "succeeded" },
    });
  });

  it("says 'no header sent, treated as the floor' without printing a header the client never sent", async () => {
    await renderBody(
      wire({
        handshake: connectedHandshake({
          state: "connected_protocol_assumed",
          protocol: {
            state: "absent",
            version: null,
            assumed: "2025-03-26",
            floor: "2025-03-26",
            target: "2025-06-18",
            supported: ["2025-03-26"],
          },
        }),
      }),
    );
    expect(screen.getByTestId("verify-protocol")).toHaveTextContent(
      "None sent, treated as 2025-03-26",
    );
  });
});

// ---------------------------------------------------------------------------
// State 3 -- it connected and something is wrong.
// ---------------------------------------------------------------------------

describe("connected, and something is wrong", () => {
  it("reports a revision this build no longer speaks, as connected", async () => {
    // The one reachable "tried and it is not right" state. It is NOT a refusal:
    // the session was accepted, and the revision stored is one we have since
    // stopped speaking.
    await renderBody(
      wire({
        handshake: connectedHandshake({
          state: "connected_protocol_unrecognised",
          protocol: {
            state: "unrecognised",
            version: "2024-11-05",
            assumed: null,
            floor: "2025-03-26",
            target: "2025-06-18",
            supported: ["2025-03-26", "2025-06-18"],
          },
        }),
      }),
    );
    expect(screen.getByTestId("handshake-connected")).toBeInTheDocument();
    expect(screen.getByTestId("verify-protocol")).toHaveTextContent(
      "2024-11-05, which this build does not currently speak",
    );
  });

  it("calls a contradictory protocol pair our failure, never the client's", async () => {
    await renderBody(
      wire({
        handshake: connectedHandshake({
          protocol: {
            state: "recognised",
            // recognised with no version: the response disagrees with itself.
            version: null,
            assumed: null,
            floor: "2025-03-26",
            target: "2025-06-18",
            supported: ["2025-03-26"],
          },
        }),
      }),
    );
    expect(screen.getByTestId("verify-protocol")).toHaveTextContent(
      "We could not read what it reported",
    );
  });

  it("renders the indeterminate scan as not knowing, never as 'no calls yet'", async () => {
    await renderBody(
      wire({
        handshake: connectedHandshake(),
        first_call: {
          state: "indeterminate",
          called_at: null,
          tool_name: null,
          audit_event_id: null,
          last_used_at: "2026-08-15T09:21:04Z",
          partial: null,
        },
      }),
    );
    expect(screen.getByTestId("firstcall-unknown")).toBeInTheDocument();
    expect(screen.queryByTestId("firstcall-none")).toBeNull();
    expect(
      screen.getByText(/limit of the check, not a fault in the connection/i),
    ).toBeInTheDocument();
  });

  it("does not read last_used_at as proof of a first call", async () => {
    // THE TRAP connection_status.go:258 NAMES. Every client issues tools/list
    // right after initialize, which stamps last_used_at without anyone asking.
    // A screen deriving success from it shows "it read your fleet" for a client
    // that read nothing.
    await renderBody(
      wire({
        handshake: connectedHandshake(),
        first_call: {
          state: "awaiting_call",
          called_at: null,
          tool_name: null,
          audit_event_id: null,
          last_used_at: "2026-08-15T09:00:05Z",
          partial: null,
        },
      }),
    );
    expect(screen.getByTestId("firstcall-none")).toBeInTheDocument();
    expect(screen.queryByTestId("firstcall-succeeded")).toBeNull();
  });

  it("refuses to render a success that carries no timestamp", async () => {
    await renderBody(
      wire({
        handshake: connectedHandshake(),
        first_call: {
          state: "succeeded",
          called_at: null,
          tool_name: "fleet_updates_pending",
          audit_event_id: null,
          last_used_at: null,
          partial: null,
        },
      }),
    );
    expect(screen.getByTestId("firstcall-unknown")).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Revoked -- terminal, and it must stop polling.
// ---------------------------------------------------------------------------

describe("revoked", () => {
  it("says a revoked connection HAD connected, when it had", async () => {
    await renderBody(wire({ status: "revoked", handshake: connectedHandshake() }));
    expect(screen.getByTestId("verify-revoked")).toHaveTextContent(
      /A client did connect with it before it was revoked/i,
    );
  });

  it("says a revoked connection never connected, when it never did", async () => {
    // The two are different facts about what happened before the revoke, and
    // an operator auditing a leak needs them apart.
    await renderBody(wire({ status: "revoked" }));
    expect(screen.getByTestId("verify-revoked")).toHaveTextContent(
      /Nothing ever connected with it/i,
    );
  });

  it("stops polling a revoked connection rather than waiting forever", () => {
    const w = wire({ status: "revoked" });
    expect(shouldKeepPolling(verifyVerdict(w, new Date(w.observed_at)))).toBe(false);
  });

  it("keeps polling one that has not connected yet", () => {
    const w = wire();
    expect(shouldKeepPolling(verifyVerdict(w, new Date(w.observed_at)))).toBe(true);
  });
});
