import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { ConnectionsList } from "./connections-list";
import {
  connectionsState,
  protocolHeaderLabel,
  type AiConnection,
  type ConnectionsState,
} from "./connection-model";

// A FAILED LOAD IS NOT AN EMPTY LIST, AND AN ABSENT HEADER IS NOT A VERSION.
//
// Both are the same defect: an absence coerced into a plausible value. On this
// page the plausible values are "you have no AI connections" (a claim about the
// operator's account, made out of a fact about our request) and a version
// number the client never sent. The consent screen next door shipped this class
// three rounds running, so it is pinned here at the mapping AND at the render.

const CONNECTED: AiConnection = {
  id: "c1",
  name: "Fleet manager",
  reportedClientName: "claude-code",
  reportedClientVersion: "2.1.0",
  protocolHeader: { kind: "recognised", version: "2025-11-25" },
  lastUsed: { kind: "at", iso: "2026-08-29T10:00:00.000Z" },
  scopes: ["mcp:read"],
  status: "active",
  createdAt: "2026-08-01T00:00:00.000Z",
};

/** The copy that must never appear over a failure. */
const EMPTY_CLAIM = /no ai clients are connected/i;

function renderList(state: ConnectionsState) {
  return renderWithProviders(<ConnectionsList state={state} />, { withRouter: true });
}

describe("connectionsState — the mapping that decides which sentence is shown", () => {
  it("maps a rejected query to error, never to empty", () => {
    const state = connectionsState({
      isPending: false,
      error: new Error("the server said 500"),
      connections: undefined,
    });
    expect(state.status).toBe("error");
  });

  it("maps an error to error even when the cache still holds rows", () => {
    // A refetch failing after a successful load is still a failure, and the
    // rows on screen are stale. Preferring the stale rows silently is how a
    // revoked connection keeps appearing to be live.
    const state = connectionsState({
      isPending: false,
      error: new Error("network"),
      connections: [CONNECTED],
    });
    expect(state.status).toBe("error");
  });

  it("maps a resolved empty array to empty", () => {
    expect(
      connectionsState({ isPending: false, error: null, connections: [] }).status,
    ).toBe("empty");
  });

  it("maps a settled query with undefined data to error, not empty", () => {
    // The `connections ?? []` line, refused. Not pending, no error, and still
    // nothing means we did not read the list.
    expect(
      connectionsState({ isPending: false, error: null, connections: undefined }).status,
    ).toBe("error");
  });

  it("maps a pending query to loading", () => {
    expect(
      connectionsState({ isPending: true, error: null, connections: undefined }).status,
    ).toBe("loading");
  });

  it("maps rows to ready", () => {
    const state = connectionsState({
      isPending: false,
      error: null,
      connections: [CONNECTED],
    });
    expect(state.status).toBe("ready");
    if (state.status === "ready") expect(state.connections).toHaveLength(1);
  });
});

describe("ConnectionsList renders each state as a different thing", () => {
  it("renders the failure and NOT the empty claim", async () => {
    renderList({ status: "error", message: "the server said 500" });
    expect(await screen.findByText(/could not load your ai connections/i)).toBeInTheDocument();
    expect(screen.getByText(/the server said 500/i)).toBeInTheDocument();
    // The assertion that matters: the empty sentence is absent.
    expect(screen.queryByText(EMPTY_CLAIM)).not.toBeInTheDocument();
    expect(screen.queryByTestId("connections-empty")).not.toBeInTheDocument();
  });

  it("renders the empty state, and it does not read as a failure", async () => {
    renderList({ status: "empty" });
    // Proves the guard above does not over-fire: correct empty work still
    // renders the empty sentence.
    expect(await screen.findByText(EMPTY_CLAIM)).toBeInTheDocument();
    expect(screen.queryByText(/could not load/i)).not.toBeInTheDocument();
  });

  it("renders 'we cannot list these yet' as neither empty nor error", async () => {
    renderList({ status: "unavailable", reason: "no endpoint exists yet" });
    expect(await screen.findByText(/cannot list your connections yet/i)).toBeInTheDocument();
    expect(screen.getByText(/no endpoint exists yet/i)).toBeInTheDocument();
    // Three different facts, three different sentences.
    expect(screen.queryByText(EMPTY_CLAIM)).not.toBeInTheDocument();
    expect(screen.queryByText(/could not load your ai connections/i)).not.toBeInTheDocument();
  });

  it("renders a loading state that is not an empty table", async () => {
    renderList({ status: "loading" });
    expect(await screen.findByTestId("connections-loading")).toBeInTheDocument();
    expect(screen.queryByText(EMPTY_CLAIM)).not.toBeInTheDocument();
  });

  it("renders rows when there are rows", async () => {
    renderList({ status: "ready", connections: [CONNECTED] });
    expect(await screen.findByText("Fleet manager")).toBeInTheDocument();
    expect(screen.getByText(/claude-code 2\.1\.0/)).toBeInTheDocument();
    expect(screen.getByText("2025-11-25")).toBeInTheDocument();
  });
});

describe("a field the client did not send is rendered as absent", () => {
  it("says the protocol header was absent instead of printing a version", async () => {
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, protocolHeader: { kind: "absent" } }],
    });
    // Both halves: the client sent nothing, AND we treat that as the floor.
    expect(await screen.findByText(/no protocol header sent/i)).toBeInTheDocument();
    expect(screen.getByText(/treated as 2025-03-26/i)).toBeInTheDocument();
    // The number it would have been coerced into must not appear on its own.
    expect(screen.queryByText("2025-11-25")).not.toBeInTheDocument();
  });

  it("distinguishes an unrecognised version from a recognised one", () => {
    // Three cases, three strings. Design §6: treating any two of them the same
    // is a defect.
    const floor = "2025-03-26";
    expect(protocolHeaderLabel({ kind: "absent" }, floor)).toMatch(/no protocol header/i);
    expect(protocolHeaderLabel({ kind: "recognised", version: "2025-11-25" }, floor)).toBe(
      "2025-11-25",
    );
    expect(
      protocolHeaderLabel({ kind: "unrecognised", version: "2024-11-05" }, floor),
    ).toMatch(/not a revision we recognise/i);
    // And no two of them are the same string.
    const all = new Set([
      protocolHeaderLabel({ kind: "absent" }, floor),
      protocolHeaderLabel({ kind: "recognised", version: "2024-11-05" }, floor),
      protocolHeaderLabel({ kind: "unrecognised", version: "2024-11-05" }, floor),
    ]);
    expect(all.size).toBe(3);
  });

  it("says never used rather than showing a date", async () => {
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, lastUsed: { kind: "never" } }],
    });
    expect(await screen.findByText(/never used/i)).toBeInTheDocument();
  });

  it("says the client reported no name rather than backfilling one", async () => {
    renderList({
      status: "ready",
      connections: [
        { ...CONNECTED, reportedClientName: null, reportedClientVersion: null },
      ],
    });
    expect(await screen.findByText(/reported no client name/i)).toBeInTheDocument();
  });

  it("says a client sent no version rather than dropping the field silently", async () => {
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, reportedClientVersion: null }],
    });
    expect(await screen.findByText(/\(no version\)/i)).toBeInTheDocument();
  });

  it("says no scopes were granted rather than rendering an empty cell", async () => {
    renderList({ status: "ready", connections: [{ ...CONNECTED, scopes: [] }] });
    expect(await screen.findByText(/no scopes granted/i)).toBeInTheDocument();
  });

  it("does not render an unparseable timestamp as a date", async () => {
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, lastUsed: { kind: "at", iso: "not-a-date" } }],
    });
    expect(await screen.findByText(/unreadable timestamp/i)).toBeInTheDocument();
    expect(screen.queryByText(/invalid date/i)).not.toBeInTheDocument();
  });
});
