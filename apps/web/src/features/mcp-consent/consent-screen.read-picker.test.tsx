import { describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, screen, within } from "@testing-library/react";

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
    name: new RegExp(`^${capabilityLabel(cap)}`),
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
      within(readPicker()).getByText("See which sites are in scope, and no other read."),
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

  it("states that the rows above the line are read-only, as the wizard does, and says nothing wider", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    expect(
      within(readPicker()).getByText(
        "Every row above this line is read-only. None of them can change WordPress content or configuration, whichever ones you pick.",
      ),
    ).toBeTruthy();
    // The old wording claimed this of every capability on the screen, which is
    // false the moment an ask box is ticked below the rows.
    expect(readPicker().textContent ?? "").not.toMatch(/no capability on this screen/i);
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

  it("calls read rows that are neither shortcut Custom, and says so plainly", async () => {
    await renderScreen(consentFor(reads(SERVER_READS)));
    const picker = readPicker();
    fireEvent.click(rowBox(picker, "mcp.backups.read"));
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();
    // The line is about the read rows, and it does not say "your own set" or
    // what the person did.
    expect(within(picker).getByText("The read rows are not either shortcut.")).toBeTruthy();
    expect(picker.textContent ?? "").not.toMatch(/your own set/i);
  });
});

// OWNER RULING 2026-10-09: A PRESET CHANGES ONLY THE READ ROWS. The chip is
// judged from the read rows alone, and pressing a shortcut leaves the site-tools
// ticks and the cache-clear tick exactly as they were. What the approval sends is
// read from what onApprove was handed.
describe("ConsentScreen, a preset changes only the read rows", () => {
  const SITE_TOOLS_ASKED = [SCOPE_READ, SCOPE_SITE];
  const offeredWithSiteTools = () => [...reads(SERVER_READS), ABILITY_READ, ABILITY_REQUEST];
  const abilityRead = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.read");
  const abilityRequest = () =>
    screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.request");
  const press = (name: string) =>
    fireEvent.click(within(readPicker()).getByRole("button", { name }));

  it("opens on Just the basics, not Custom, when site tools open ticked, and says what the shortcut is", async () => {
    // The read rows are Sites alone, which is the first shortcut. The site-tools
    // ticks are in no shortcut and do not move the chip, so nobody is told they
    // made a custom set when they made no choice at all.
    await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    const picker = readPicker();
    expect(abilityRead().checked).toBe(true);
    expect(abilityRequest().checked).toBe(true);
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(within(picker).queryByTestId("preset-custom")).toBeNull();
    expect(
      within(picker).getByText("See which sites are in scope, and no other read."),
    ).toBeTruthy();
    expect(picker.textContent ?? "").not.toMatch(/your own set|you have changed|clears them/i);
  });

  it("keeps both site tools ticked when Read everything is pressed, and the approval carries them with every read", async () => {
    const onApprove = await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    press("Read everything");

    expect(within(readPicker()).getByRole("button", { name: "Read everything", pressed: true })).toBeTruthy();
    expect(tickedReads(readPicker())).toEqual([...SERVER_READS]);
    expect(abilityRead().checked).toBe(true);
    expect(abilityRequest().checked).toBe(true);
    submit();
    expect(sentCapabilities(onApprove)).toEqual(
      [...SERVER_READS, "mcp.ability.read", "mcp.ability.request"].sort(),
    );
  });

  it("keeps both site tools ticked when Just the basics is pressed after Read everything", async () => {
    const onApprove = await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    press("Read everything");
    press("Just the basics");

    expect(within(readPicker()).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(tickedReads(readPicker())).toEqual(["mcp.sites.read"]);
    expect(abilityRead().checked).toBe(true);
    expect(abilityRequest().checked).toBe(true);
    submit();
    expect(sentCapabilities(onApprove)).toEqual([
      "mcp.ability.read",
      "mcp.ability.request",
      "mcp.sites.read",
    ]);
  });

  it("leaves a site tool the person cleared cleared when a shortcut is pressed", async () => {
    // "Ask for changes" cleared, "see what the site can do" left ticked. A press
    // must not put the request back, and must not clear the read.
    const onApprove = await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    fireEvent.click(abilityRequest());
    expect(abilityRequest().checked).toBe(false);
    press("Read everything");

    expect(abilityRequest().checked).toBe(false);
    expect(abilityRead().checked).toBe(true);
    submit();
    expect(sentCapabilities(onApprove)).toEqual([...SERVER_READS, "mcp.ability.read"].sort());
  });

  it("does not tick a site tool the person never ticked: a press adds none", async () => {
    // Both cleared by the person; the presets must not bring either back.
    const onApprove = await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    fireEvent.click(abilityRead());
    expect(abilityRead().checked).toBe(false);
    expect(abilityRequest().checked).toBe(false);
    press("Read everything");
    press("Just the basics");

    expect(abilityRead().checked).toBe(false);
    expect(abilityRequest().checked).toBe(false);
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.sites.read"]);
  });

  it("judges the read rows only: ticking the cache clear does not move the chip, and a press leaves it ticked", async () => {
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    const picker = readPicker();
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();

    fireEvent.click(cacheBox());
    expect(cacheBox().checked).toBe(true);
    // Still Just the basics, because the read rows still are.
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(within(picker).queryByTestId("preset-custom")).toBeNull();

    press("Read everything");
    expect(cacheBox().checked).toBe(true);
    expect(within(picker).getByRole("button", { name: "Read everything", pressed: true })).toBeTruthy();
    submit();
    expect(sentCapabilities(onApprove)).toEqual([...SERVER_READS, "mcp.cache.purge"].sort());
  });

  it("does not tick the cache clear from a press", async () => {
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), CACHE_PURGE], [SCOPE_READ, SCOPE_CACHE]),
    );
    press("Read everything");
    expect(cacheBox().checked).toBe(false);
    submit();
    expect(sentCapabilities(onApprove)).toEqual([...SERVER_READS].sort());
  });

  it("moves the chip to Custom when a read row diverges, whatever else is ticked, and back when it matches again", async () => {
    await renderScreen(consentFor(offeredWithSiteTools(), SITE_TOOLS_ASKED));
    const picker = readPicker();
    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(within(picker).getByTestId("preset-custom")).toBeTruthy();
    expect(within(picker).getByText("The read rows are not either shortcut.")).toBeTruthy();

    fireEvent.click(rowBox(picker, "mcp.uptime.read"));
    expect(within(picker).queryByTestId("preset-custom")).toBeNull();
    expect(within(picker).getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
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

  it("adds the site-tools names only while their boxes are ticked, beside the reads that are", async () => {
    // Both site-tools boxes open ticked because the app asked for site tools.
    // Clearing "ask for changes" takes exactly that name out of the request.
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), ABILITY_READ, ABILITY_REQUEST], [SCOPE_READ, SCOPE_SITE]),
    );
    expect(screen.getByTestId("ability-box-mcp.ability.read")).toBeChecked();
    expect(screen.getByTestId("ability-box-mcp.ability.request")).toBeChecked();
    fireEvent.click(screen.getByTestId("ability-box-mcp.ability.request"));
    expect(screen.getByTestId("ability-box-mcp.ability.request")).not.toBeChecked();
    expect(screen.getByTestId("ability-box-mcp.ability.read")).toBeChecked();
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.ability.read", "mcp.sites.read"]);
  });

  it("takes both site-tools names out of the request when see what the site can do is cleared", async () => {
    // "Ask for changes" needs "see what the site can do", so clearing the read
    // clears the request with it; neither name is sent.
    const onApprove = await renderScreen(
      consentFor([...reads(SERVER_READS), ABILITY_READ, ABILITY_REQUEST], [SCOPE_READ, SCOPE_SITE]),
    );
    fireEvent.click(screen.getByTestId("ability-box-mcp.ability.read"));
    expect(screen.getByTestId("ability-box-mcp.ability.read")).not.toBeChecked();
    expect(screen.getByTestId("ability-box-mcp.ability.request")).not.toBeChecked();
    submit();
    expect(sentCapabilities(onApprove)).toEqual(["mcp.sites.read"]);
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

// WHAT IS SHOWN TICKED IS WHAT IS SENT, IN EVERY STATE. The operator decides
// from the boxes, and the approval is built from the tick list, so the two have
// to be the same set. This walks every combination of what the app asked for
// (scopes) and what the server offered, in three starting positions: as the
// screen opens, with every enabled box ticked, and with every enabled box
// cleared. In each, the capability names read off the ticked boxes must equal
// the list the approval hands to onApprove, and when nothing is ticked there
// must be nothing to approve.
describe("ConsentScreen, what is shown ticked is what is sent", () => {
  const FULL = [...reads(SERVER_READS), CACHE_PURGE, ABILITY_READ, ABILITY_REQUEST];
  const without = (name: string) => FULL.filter((c) => c.name !== name);
  const OFFERS: Record<string, readonly { name: string; effect: string }[]> = {
    "everything offered": FULL,
    "no cache clear offered": without("mcp.cache.purge"),
    "no read for site tools offered": without("mcp.ability.read"),
    "no ask for changes offered": without("mcp.ability.request"),
    "no capability list at all (an older server)": [],
  };
  const SCOPES: Record<string, readonly string[]> = {
    "reading only": [SCOPE_READ],
    "site tools": [SCOPE_READ, SCOPE_SITE],
    "cache clear": [SCOPE_READ, SCOPE_CACHE],
    "site tools and cache clear": [SCOPE_READ, SCOPE_SITE, SCOPE_CACHE],
  };

  const permissions = () =>
    within(screen.getByRole("region", { name: "What this connection can do" }));

  /** The capability names whose box is ticked on the screen right now. */
  function shownTicked(): string[] {
    const names: string[] = [];
    const picker = screen.queryByRole("group", { name: "It will be able to read" });
    if (picker !== null) {
      for (const cap of SERVER_READS) if (rowBox(picker, cap).checked) names.push(cap);
    }
    const cache = screen.queryByTestId("consent-cache-capability");
    if (cache !== null && within(cache).getByRole<HTMLInputElement>("checkbox").checked) {
      names.push("mcp.cache.purge");
    }
    for (const cap of ["mcp.ability.read", "mcp.ability.request"]) {
      if (screen.queryByTestId<HTMLInputElement>(`ability-box-${cap}`)?.checked === true) {
        names.push(cap);
      }
    }
    return names.sort();
  }

  const starts: Record<string, () => void> = {
    "as it opens": () => undefined,
    "with every enabled box ticked": () => {
      for (const box of permissions().queryAllByRole<HTMLInputElement>("checkbox")) {
        if (!box.disabled && !box.checked) fireEvent.click(box);
      }
    },
    "with every enabled box cleared": () => {
      for (const box of permissions().queryAllByRole<HTMLInputElement>("checkbox")) {
        if (!box.disabled && box.checked) fireEvent.click(box);
      }
    },
  };

  for (const [offerName, offered] of Object.entries(OFFERS)) {
    for (const [scopeName, scopes] of Object.entries(SCOPES)) {
      it(`${offerName}, app asks for ${scopeName}`, async () => {
        for (const [startName, start] of Object.entries(starts)) {
          const onApprove = await renderScreen(consentFor(offered, scopes));
          start();
          const shown = shownTicked();
          const approve = screen.getByTestId("consent-approve");
          if (approve.hasAttribute("disabled")) {
            // Nothing to approve is exactly nothing ticked.
            expect(shown, `${startName}: Approve is off, so nothing may be ticked`).toEqual([]);
          } else {
            submit();
            expect(onApprove).toHaveBeenCalledTimes(1);
            const input = onApprove.mock.calls[0]![0] as { capabilities?: string[] };
            expect([...(input.capabilities ?? [])].sort(), startName).toEqual(shown);
          }
          cleanup();
        }
      });
    }
  }

  it("is not satisfied by empty lists: the walk does reach ticked boxes and a request that carries them", async () => {
    // The positive control for the walk above. If every state came out as
    // nothing ticked and nothing sent, equality would hold and prove nothing.
    const onApprove = await renderScreen(consentFor(FULL, [SCOPE_READ, SCOPE_SITE, SCOPE_CACHE]));
    starts["with every enabled box ticked"]!();
    const shown = shownTicked();
    expect(shown).toEqual(
      [...SERVER_READS, "mcp.cache.purge", "mcp.ability.read", "mcp.ability.request"].sort(),
    );
    submit();
    const input = onApprove.mock.calls[0]![0] as { capabilities?: string[] };
    expect([...(input.capabilities ?? [])].sort()).toEqual(shown);
  });
});
