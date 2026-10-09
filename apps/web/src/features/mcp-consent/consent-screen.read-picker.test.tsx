import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { capabilityLabel } from "@/features/ai-connections/capabilities";

import { ConsentScreen, type ConsentScreenProps } from "./consent-screen";
import {
  parseConsentContext,
  SCOPE_CACHE,
  SCOPE_READ,
  SCOPE_SITE,
  type ConsentContext,
} from "./consent-context";
import { OAuthRequestError } from "./use-consent";

// The consent screen's read picker (GH #660): the same presets and read rows as
// the connection wizard's step 4, opening on the same default, and an approval
// that sends exactly what is ticked.
//
// Every case renders the real screen inside the real router and a real
// QueryClient (withRouter: true), and reads the outcome the way an operator
// does: from the checkboxes on screen and from the payload handed to onApprove.

// What the server lists for a request holding mcp:read: scopeCapabilities
// [ScopeRead] in apps/api/internal/mcp/policy.go. Written out here, not derived
// from the dashboard's own vocabulary, so a change to that vocabulary cannot
// quietly change what this file says the server sent.
const SERVER_READS = [
  "mcp.activity.read",
  "mcp.backups.read",
  "mcp.diagnostics.read",
  "mcp.performance.read",
  "mcp.security.read",
  "mcp.sites.read",
  "mcp.uptime.read",
] as const;

const CACHE_PURGE = { name: "mcp.cache.purge", effect: "request" } as const;
const ABILITY_READ = { name: "mcp.ability.read", effect: "read" } as const;
const ABILITY_REQUEST = { name: "mcp.ability.request", effect: "request" } as const;

/** The one-sentence reason Approve is off when nothing at all is ticked. */
const NOTHING_TICKED =
  "Nothing is ticked, so this connection could do nothing: tick a box above, or deny the request.";

const reads = (names: readonly string[]) => names.map((name) => ({ name, effect: "read" }));

function consentFor(
  conferrable: readonly { name: string; effect: string }[],
  scopes: readonly string[] = [SCOPE_READ],
): ConsentContext {
  return parseConsentContext({
    client_id: "c_picker",
    client_name_unverified: "Test Client",
    identity_verified: false,
    redirect_uri: "https://x.example/cb",
    redirect_host: "x.example",
    scopes,
    grant_lifetime_days: 90,
    conferrable_capabilities: conferrable,
  });
}

function props(consent: ConsentContext, over: Partial<ConsentScreenProps> = {}): ConsentScreenProps {
  return {
    consent,
    tags: [],
    fleet: { sites: [{ id: "s1", name: "Alpha", url: "https://alpha.example" }], complete: true },
    tagsBySiteId: { s1: [] },
    sitesLoading: false,
    isApproving: false,
    approveError: null,
    onApprove: vi.fn(),
    onDeny: vi.fn(),
    ...over,
  };
}

async function renderScreen(consent: ConsentContext, over: Partial<ConsentScreenProps> = {}) {
  const onApprove = vi.fn();
  renderWithProviders(<ConsentScreen {...props(consent, { onApprove, ...over })} />, {
    withRouter: true,
  });
  // The router's first paint is not synchronous: assert the first thing found.
  await screen.findByTestId("consent-approve");
  return onApprove;
}

const readPicker = () => screen.getByRole("group", { name: "It will be able to read" });

const rowBox = (picker: HTMLElement, cap: string) =>
  within(picker).getByRole<HTMLInputElement>("checkbox", {
    name: new RegExp(`^${capabilityLabel(cap)}\\b`),
  });

/** The read rows currently ticked, as wire names, in the order SERVER_READS lists them. */
const tickedReads = (picker: HTMLElement) =>
  SERVER_READS.filter((cap) => rowBox(picker, cap).checked);

const cacheBox = () =>
  within(screen.getByTestId("consent-cache-capability")).getByRole<HTMLInputElement>("checkbox");

function submit() {
  fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
}

function sentCapabilities(onApprove: ReturnType<typeof vi.fn>): string[] {
  expect(onApprove).toHaveBeenCalledTimes(1);
  const input = onApprove.mock.calls[0]![0] as { capabilities?: string[] };
  expect(input.capabilities).toBeDefined();
  return [...input.capabilities!].sort();
}

describe("ConsentScreen, the read picker opens on the wizard's defaults", () => {
  it("opens on Just the basics with only Sites ticked and every other read clear", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();

    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(within(picker).getByRole("button", { name: "Read everything", pressed: false })).toBeTruthy();
    expect(within(picker).queryByTestId("preset-custom")).toBeNull();

    // The default is the wizard's, which is Sites alone. A literal, on purpose:
    // the owner's ruling is the source of this value, not the code under test.
    expect(tickedReads(picker)).toEqual(["mcp.sites.read"]);
    for (const cap of SERVER_READS) expect(rowBox(picker, cap)).toBeEnabled();
  });

  it("describes what the preset does, in the wizard's words", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    expect(
      within(readPicker()).getByText("See which sites are in scope, and nothing else."),
    ).toBeTruthy();
  });

  it("shows one row per read the server confers, plus Content disabled with its reason", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();

    expect(within(picker).getAllByRole("checkbox")).toHaveLength(SERVER_READS.length + 1);
    const content = within(picker).getByRole("checkbox", { name: /^Content/ });
    expect(content).toBeDisabled();
    expect(content).not.toBeChecked();
    expect(
      within(picker).getByText(/not available yet.*no content tools.*for a connection to call/i),
    ).toBeTruthy();
    // And it is the reason for Content alone: every other row is on offer.
    expect(within(picker).queryByText(/not requested by this app/i)).toBeNull();
  });

  it("states that the rows above the line are read-only, as the wizard does", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    expect(
      within(readPicker()).getByText(
        /no capability on this screen can change wordpress content or configuration/i,
      ),
    ).toBeTruthy();
  });

  it("sends the default untouched: Sites alone", async () => {
    const onApprove = await renderScreen(consentFor(reads(SERVER_READS)));
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.sites.read"]);
  });
});

describe("ConsentScreen, the presets", () => {
  it("ticks every read the server offered for Read everything, and nothing else", async () => {
    await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    const picker = readPicker();
    fireEvent.click(within(picker).getByRole("button", { name: "Read everything" }));

    expect(within(picker).getByRole("button", { name: "Read everything", pressed: true })).toBeTruthy();
    expect(tickedReads(picker)).toEqual([...SERVER_READS]);
    // Not the unreachable read, and not the cache-clear request.
    expect(within(picker).getByRole("checkbox", { name: /^Content/ })).not.toBeChecked();
    expect(cacheBox()).not.toBeChecked();
  });

  it("sends every offered read for Read everything and never mcp.content.read", async () => {
    const onApprove = await renderScreen(consentFor(reads(SERVER_READS)));
    fireEvent.click(within(readPicker()).getByRole("button", { name: "Read everything" }));
    submit();

    const sent = sentCapabilities(onApprove);
    expect(sent).toEqual([...SERVER_READS].sort());
    expect(sent).not.toContain("mcp.content.read");
  });

  it("returns to Just the basics when that preset is pressed again", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(within(picker).getByRole("button", { name: "Read everything" }));
    fireEvent.click(within(picker).getByRole("button", { name: "Just the basics" }));
    expect(tickedReads(picker)).toEqual(["mcp.sites.read"]);
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
  });

  it("moves to Custom the moment a row diverges, and takes the claim back when the set matches again", async () => {
    // Derived from the ticks on every render, never latched. A control that
    // stayed on Custom after the set became Read everything again would be a
    // label lying in the other direction.
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(within(picker).getByRole("button", { name: "Read everything" }));

    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();
    expect(within(picker).queryAllByRole("button", { pressed: true })).toHaveLength(0);

    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(within(picker).queryByTestId("preset-custom")).toBeNull();
    expect(within(picker).getByRole("button", { name: "Read everything", pressed: true })).toBeTruthy();
  });

  it("calls a set that is neither preset Custom, and says so", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(rowBox(picker, "mcp.backups.read"));
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();
    expect(
      within(picker).getByText(/you have changed the rows below, so this is your own set/i),
    ).toBeTruthy();
  });

  it("derives the claim over the cache-clear tick too, and a preset clears it as it does in the wizard", async () => {
    await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    const picker = readPicker();
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();

    fireEvent.click(cacheBox());
    // Sites plus a write is not "Just the basics": its description says
    // "and nothing else".
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();

    fireEvent.click(within(picker).getByRole("button", { name: "Read everything" }));
    expect(cacheBox()).not.toBeChecked();
  });
});

describe("ConsentScreen, the approval sends exactly the ticked reads", () => {
  it("sends Backups and Security when the operator ticks those and clears Sites", async () => {
    const onApprove = await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(rowBox(picker, "mcp.sites.read"));
    fireEvent.click(rowBox(picker, "mcp.backups.read"));
    fireEvent.click(rowBox(picker, "mcp.security.read"));
    submit();

    // Exactly these two. The screen used to send every read the server offered
    // whatever was on it; sending Sites here would be a read nobody ticked.
    expect(sentCapabilities(onApprove)).toEqual(["mcp.backups.read", "mcp.security.read"]);
  });

  it("sends what the screen shows ticked, for any mix of rows", async () => {
    const onApprove = await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    for (const cap of ["mcp.uptime.read", "mcp.activity.read", "mcp.performance.read"]) {
      fireEvent.click(rowBox(picker, cap));
    }
    fireEvent.click(rowBox(picker, "mcp.activity.read"));

    const shown = [...tickedReads(picker)].sort();
    submit();
    expect(sentCapabilities(onApprove)).toEqual(shown);
    expect(shown).toEqual(["mcp.performance.read", "mcp.sites.read", "mcp.uptime.read"]);
  });

  it("adds the cache-clear request only when its box is ticked, beside the reads that are", async () => {
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    fireEvent.click(rowBox(readPicker(), "mcp.uptime.read"));
    fireEvent.click(cacheBox());
    submit();
    expect(sentCapabilities(onApprove)).toEqual([
      "mcp.cache.purge",
      "mcp.sites.read",
      "mcp.uptime.read",
    ]);
  });

  it("adds the site-tools requests only when their boxes are ticked, beside the reads that are", async () => {
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), ABILITY_READ, ABILITY_REQUEST], [SCOPE_READ, SCOPE_SITE]),
    );
    fireEvent.click(screen.getByTestId("ability-box-mcp.ability.read"));
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.ability.read", "mcp.sites.read"]);
  });

  it("lets the cache-clear box stand alone once every read is cleared", async () => {
    // Nothing is ticked among the reads, but the operator did tick something,
    // so there is a list to send and it has one name in it. The empty list is
    // never sent.
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    fireEvent.click(rowBox(readPicker(), "mcp.sites.read"));
    fireEvent.click(cacheBox());
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.cache.purge"]);
  });
});

describe("ConsentScreen, a read the server did not offer", () => {
  const OFFERED = ["mcp.sites.read", "mcp.uptime.read"];

  it("is shown clear and disabled with a reason, and is not a Content reason", async () => {
    await renderScreen(consentFor(reads(OFFERED)));
    const picker = readPicker();
    for (const cap of SERVER_READS.filter((c) => !OFFERED.includes(c))) {
      expect(rowBox(picker, cap)).toBeDisabled();
      expect(rowBox(picker, cap)).not.toBeChecked();
      expect(within(picker).getByTestId(`read-not-offered-${cap}`)).toHaveTextContent(
        "Not requested by this app",
      );
    }
    expect(rowBox(picker, "mcp.sites.read")).toBeEnabled();
    expect(rowBox(picker, "mcp.uptime.read")).toBeEnabled();
  });

  it("is never ticked by Read everything and never sent", async () => {
    const onApprove = await renderScreen(consentFor(reads(OFFERED)));
    const picker = readPicker();
    fireEvent.click(within(picker).getByRole("button", { name: "Read everything" }));
    expect(tickedReads(picker)).toEqual(["mcp.sites.read", "mcp.uptime.read"]);
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.sites.read", "mcp.uptime.read"]);
  });

  it("does not default to a read that is not on offer: with Sites missing nothing is ticked", async () => {
    const onApprove = await renderScreen(consentFor(reads(["mcp.uptime.read", "mcp.backups.read"])));
    const picker = readPicker();
    expect(tickedReads(picker)).toEqual([]);
    expect(within(picker).queryByRole("button", { name: "Just the basics" })).toBeNull();
    // Nothing ticked, and no preset claims otherwise.
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();
    expect(screen.getByTestId("consent-approve")).toBeDisabled();
    submit();
    expect(onApprove).not.toHaveBeenCalled();

    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.uptime.read"]);
  });
});

describe("ConsentScreen, nothing ticked", () => {
  it("blocks Approve and says why in one sentence", async () => {
    const onApprove = await renderScreen(consentFor(reads(SERVER_READS)));
    fireEvent.click(rowBox(readPicker(), "mcp.sites.read"));

    const approve = screen.getByTestId("consent-approve");
    expect(approve).toBeDisabled();
    const why = screen.getByTestId("consent-nothing-to-confer");
    expect(why.textContent).toBe(NOTHING_TICKED);
    expect(why).toHaveAttribute("role", "status");
    // One sentence: the only sentence-ending mark is the last character.
    expect((why.textContent ?? "").match(/[.!?]/g)).toHaveLength(1);
    // And the button carries the reason for a screen reader.
    expect(approve).toHaveAccessibleDescription(NOTHING_TICKED);

    // Submitting past the disabled button is refused too.
    submit();
    expect(onApprove).not.toHaveBeenCalled();
  });

  it("lifts the block, and drops the sentence, the moment anything is ticked", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(rowBox(picker, "mcp.sites.read"));
    expect(screen.getByTestId("consent-approve")).toBeDisabled();

    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
    expect(screen.queryByTestId("consent-nothing-to-confer")).toBeNull();
  });

  it("does not show the sentence while something is ticked", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    expect(screen.queryByTestId("consent-nothing-to-confer")).toBeNull();
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
  });

  it("still says it for a request that asked only for the cache-clear tick, with no read picker", async () => {
    await renderScreen(consentFor([CACHE_PURGE], [SCOPE_CACHE]));
    expect(screen.getByTestId("consent-nothing-to-confer").textContent).toBe(NOTHING_TICKED);
    expect(screen.getByTestId("consent-approve")).toBeDisabled();
    fireEvent.click(cacheBox());
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
  });
});

describe("ConsentScreen, when no read picker belongs on the screen", () => {
  it("shows none when the app did not ask to read", async () => {
    await renderScreen(consentFor([CACHE_PURGE], [SCOPE_CACHE]));
    expect(screen.queryByRole("group", { name: "It will be able to read" })).toBeNull();
    expect(screen.queryByTestId("read-capability-picker")).toBeNull();
    expect(screen.queryByTestId("capability-presets")).toBeNull();
  });

  it("keeps the static description and sends no list against a server that offers none", async () => {
    // Deploy-ordering case: an older server sends no conferrable_capabilities.
    // There is nothing to tick, so nothing is claimed to be ticked, and the
    // server's own default applies to an omitted list.
    const onApprove = await renderScreen(consentFor([], [SCOPE_READ]));
    expect(screen.queryByRole("group", { name: "It will be able to read" })).toBeNull();
    expect(screen.getByText(/Read your fleet's data/i)).toBeTruthy();
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
    submit();
    expect(onApprove).toHaveBeenCalledTimes(1);
    expect(onApprove.mock.calls[0]![0]).not.toHaveProperty("capabilities");
  });

  it("replaces the static read description with the picker, so the screen lists what will be granted", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    expect(screen.queryByText(/Read your fleet's data/i)).toBeNull();
    expect(readPicker()).toBeTruthy();
  });

  it("still flags a permission it does not recognise beside the picker", async () => {
    await renderScreen(consentFor(reads(SERVER_READS), [SCOPE_READ, "mcp:write"]));
    expect(readPicker()).toBeTruthy();
    expect(screen.getByTestId("consent-unrecognised-scope")).toBeTruthy();
    expect(screen.getByText(/an unrecognised permission/i)).toBeTruthy();
    expect(screen.getByTestId("consent-approve")).toBeDisabled();
  });
});

describe("ConsentScreen, while an approval is in flight", () => {
  it("disables the presets, every read row and both write boxes", async () => {
    await renderScreen(
      consentFor(
        [...reads(SERVER_READS), CACHE_PURGE, ABILITY_READ, ABILITY_REQUEST],
        [SCOPE_READ, SCOPE_CACHE, SCOPE_SITE],
      ),
      { isApproving: true },
    );
    const picker = readPicker();
    for (const button of within(picker).getAllByRole("button")) expect(button).toBeDisabled();
    for (const box of within(picker).getAllByRole("checkbox")) expect(box).toBeDisabled();
    expect(cacheBox()).toBeDisabled();
    expect(screen.getByTestId("ability-box-mcp.ability.read")).toBeDisabled();
    expect(screen.getByTestId("ability-box-mcp.ability.request")).toBeDisabled();
  });

  it("leaves every control live when nothing is in flight", async () => {
    await renderScreen(
      consentFor(
        [...reads(SERVER_READS), CACHE_PURGE, ABILITY_READ, ABILITY_REQUEST],
        [SCOPE_READ, SCOPE_CACHE, SCOPE_SITE],
      ),
    );
    const picker = readPicker();
    for (const button of within(picker).getAllByRole("button")) expect(button).toBeEnabled();
    expect(cacheBox()).toBeEnabled();
  });
});

describe("ConsentScreen, a refusal from the server", () => {
  it("is shown, with the server's own words, and the operator can change the ticks and try again", async () => {
    // The wire shape the consent endpoint answers a refused narrowing with:
    // HTTP 400, error "invalid_request", and the Go message as the description
    // (CapabilitySet.NarrowTo in policy.go; the oauthError default arm in dto.go).
    const refusal = new OAuthRequestError(
      "invalid_request",
      'capability "mcp.content.read" is not held by this organisation\'s default, so a connection cannot be granted it; a connection may only ever be narrower than the organisation default',
      400,
    );
    await renderScreen(consentFor(reads(SERVER_READS)), { approveError: refusal });

    expect(screen.getByText(/we could not approve this connection/i)).toBeTruthy();
    expect(screen.getByText(/is not held by this organisation's default/i)).toBeTruthy();
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
    expect(readPicker()).toBeTruthy();
  });
});
