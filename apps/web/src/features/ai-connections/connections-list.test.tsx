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
  siteScopeMode: "all",
  revokedAt: null,
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
    // Derived, not frozen: the over-fire pass caught this pair of regexes
    // reddening when the absent wording was reworded, which is correct work.
    const floor = "2025-03-26";
    expect(
      await screen.findByText(protocolHeaderLabel({ kind: "absent" }, floor)),
    ).toBeInTheDocument();
    // Both halves still have to be SAID, which is a property of the label and
    // is asserted on the label rather than on the rendered sentence: the client
    // sent nothing, and we treat that as the floor.
    const label = protocolHeaderLabel({ kind: "absent" }, floor);
    expect(label).toContain(floor);
    expect(label.length).toBeGreaterThan(floor.length);
    // The number it would have been coerced into must not appear on its own.
    expect(screen.queryByText("2025-11-25")).not.toBeInTheDocument();
  });

  it("says a client has not connected yet, rather than that it sent no header", async () => {
    // FOUR STATES, NOT THREE. This one was missing from the model until the
    // endpoint existed. "Has never dialled in" and "dialled in and sent no
    // header" are different facts about different things, and the server went
    // out of its way not to flatten them into a nullable string.
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, protocolHeader: { kind: "never_connected" } }],
    });
    // DERIVED, NOT HARDCODED. The over-fire pass caught an earlier version
    // matching /has not connected yet/i: rewording that sentence is correct
    // work and it reddened. Fourth instance of this trap in this slice. What
    // must not regress is that this state renders as ITSELF and not as one of
    // the other three, so the expected string comes from the same function the
    // component uses.
    const floor = "2025-03-26";
    const expected = protocolHeaderLabel({ kind: "never_connected" }, floor);
    expect(await screen.findByText(expected)).toBeInTheDocument();
    // And not as either neighbouring state.
    expect(
      screen.queryByText(protocolHeaderLabel({ kind: "absent" }, floor)),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("2025-11-25")).not.toBeInTheDocument();
  });

  it("gives all five protocol kinds five different sentences", () => {
    const floor = "2025-03-26";
    const labels = [
      protocolHeaderLabel({ kind: "never_connected" }, floor),
      protocolHeaderLabel({ kind: "absent" }, floor),
      protocolHeaderLabel({ kind: "recognised", version: "2025-11-25" }, floor),
      protocolHeaderLabel({ kind: "unrecognised", version: "2025-11-25" }, floor),
      // Not a wire state: what we say when the response contradicts itself.
      protocolHeaderLabel({ kind: "unreadable" }, floor),
    ];
    // Same version string in the middle two on purpose: if the label ever
    // stopped marking the unrecognised one, they would collide and this fails.
    expect(new Set(labels).size).toBe(5);
    for (const l of labels) expect(l.trim().length).toBeGreaterThan(0);
  });

  it("phrases the unreadable label as our failure, not the client's behaviour", () => {
    // The distinction the kind exists for. Asserted on the pronoun rather than
    // the whole sentence, so rewording stays free.
    const label = protocolHeaderLabel({ kind: "unreadable" }, "2025-03-26");
    expect(label.toLowerCase()).toMatch(/\bwe\b|\bcould not\b|\bunreadable\b/);
    // And it must not read as a claim about what the client sent.
    expect(label.toLowerCase()).not.toContain("sent no");
  });

  it("renders an unreadable report as unreadable, not as 'sent no header'", async () => {
    // Derived expectations, first time of asking. The property is that this
    // kind renders as itself and not as a neighbour; the wording is free.
    const floor = "2025-03-26";
    renderList({
      status: "ready",
      connections: [{ ...CONNECTED, protocolHeader: { kind: "unreadable" } }],
    });
    expect(
      await screen.findByText(protocolHeaderLabel({ kind: "unreadable" }, floor)),
    ).toBeInTheDocument();
    // The specific wrong answer: a malformed response rendered as a confident
    // fact about the client.
    expect(
      screen.queryByText(protocolHeaderLabel({ kind: "absent" }, floor)),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(protocolHeaderLabel({ kind: "never_connected" }, floor)),
    ).not.toBeInTheDocument();
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
    // ASSERTED ON THE BRANCH, NOT THE SENTENCE. The over-fire pass caught this
    // matching /reported no client name/i: rewording the sentinel is correct
    // work and it reddened. The testid renders only in the null-name branch, so
    // it pins the decision without freezing the copy.
    renderList({
      status: "ready",
      connections: [
        { ...CONNECTED, reportedClientName: null, reportedClientVersion: null },
      ],
    });
    const cell = await screen.findByTestId("reported-client");
    // And it must not have been backfilled from the operator's chosen client.
    expect(cell.textContent).not.toContain("claude-code");
    expect((cell.textContent ?? "").trim().length).toBeGreaterThan(0);
  });

  it("keeps the version when the client reported one but no name", async () => {
    // The type permits name===null with a version present. Dropping the version
    // there throws away a fact the client actually sent.
    //
    // ASSERTED ON STRUCTURE, NOT ON THE SENTENCE. An earlier version matched
    // /reported no client name/i, and the over-fire pass caught it: rewording
    // that sentinel copy is correct work and it reddened. Same trap the sidebar
    // guard fell into. What must not regress is that the version survives.
    renderList({
      status: "ready",
      connections: [
        { ...CONNECTED, reportedClientName: null, reportedClientVersion: "0.9.1" },
      ],
    });
    const version = await screen.findByTestId("reported-version");
    expect(version.textContent).toContain("0.9.1");
  });

  it("does not invent a version when the client sent neither", async () => {
    // The over-fire half: a genuinely empty pair must not grow a stray comma or
    // a fabricated number.
    renderList({
      status: "ready",
      connections: [
        { ...CONNECTED, reportedClientName: null, reportedClientVersion: null },
      ],
    });
    expect(await screen.findByTestId("reported-client")).toBeInTheDocument();
    expect(screen.queryByTestId("reported-version")).not.toBeInTheDocument();
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
