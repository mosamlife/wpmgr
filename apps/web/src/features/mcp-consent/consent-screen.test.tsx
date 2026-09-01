import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { ConsentScreen, type ConsentScreenProps } from "./consent-screen";
import { parseConsentContext, SCOPE_READ } from "./consent-context";
import { bannedWordHits } from "./site-enforcement";
import type { ScopedSite } from "./site-scope";

// The rendered half of m124 obligation 7 and of the empty-scope rule.
//
// THE ATTACK THESE TESTS PIN. An attacker performs an unauthenticated RFC 7591
// registration with client_name "Claude Desktop" and a redirect_uri they
// control. The database stores it cleanly -- the unique index is on client_id
// alone -- and no constraint can refuse it. The consent screen is the only
// thing between that registration and a user granting fleet read access to a
// stranger.

const ATTACKER = parseConsentContext({
  client_id: "c_attacker",
  // The attacker's chosen string. Identical to what a legitimate registration
  // would send.
  client_name_unverified: "Claude Desktop",
  client_uri_unverified: "https://claude.ai",
  identity_verified: false,
  // The tell, and the only verified fact on the screen.
  redirect_uri: "https://claude-desktop-oauth.example.net/cb",
  redirect_host: "claude-desktop-oauth.example.net",
  scopes: [SCOPE_READ],
  grant_lifetime_days: 90,
  state: "s",
  code_challenge: "cc",
  code_challenge_method: "S256",
});

const SITES: ScopedSite[] = [
  { id: "s1", name: "Alpha", url: "https://alpha.example" },
  { id: "s2", name: "Beta", url: "https://beta.example" },
];

function props(over: Partial<ConsentScreenProps> = {}): ConsentScreenProps {
  return {
    consent: ATTACKER,
    tags: [{ id: "t1", name: "prod" }],
    fleet: { sites: SITES, complete: true },
    tagsBySiteId: { s1: ["prod"], s2: ["staging"] },
    sitesLoading: false,
    isApproving: false,
    approveError: null,
    onApprove: vi.fn(),
    onDeny: vi.fn(),
    ...over,
  };
}

describe("ConsentScreen — the self-declared name is never presented as verified", () => {
  it("shows the verified redirect host and marks it as the thing we verified", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    const host = screen.getByTestId("consent-redirect-host");
    expect(host.textContent).toBe("claude-desktop-oauth.example.net");
    expect(screen.getByText(/We verified this/i)).toBeTruthy();
  });

  it("marks the attacker-supplied name as unverified, in the same region as the name", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    const nameNode = screen.getByTestId("consent-client-name");
    expect(nameNode.textContent).toContain("Claude Desktop");
    // The disclaimer must be a sibling of the name inside one labelled block,
    // not a footnote somewhere else on the page. A user who reads the name and
    // stops reading must already have seen it.
    const block = nameNode.closest("section");
    expect(block).not.toBeNull();
    expect(block!.textContent).toMatch(/We did not verify this/i);
    expect(block!.textContent).toMatch(/Anyone can register a client under any name/i);
  });

  it("puts the verified host BEFORE the self-declared name in reading order", () => {
    // Order is the control. Every OAuth consent screen in the world leads with
    // the client's chosen name, and that habit is what an impersonating
    // registration is buying.
    renderWithProviders(<ConsentScreen {...props()} />);
    const host = screen.getByTestId("consent-redirect-host");
    const name = screen.getByTestId("consent-client-name");
    const position = host.compareDocumentPosition(name);
    expect(position & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("never renders the self-declared homepage as a link", () => {
    // A link is an endorsement and a click target we did not check.
    renderWithProviders(<ConsentScreen {...props()} />);
    const uri = screen.getByTestId("consent-client-uri");
    expect(uri.tagName).not.toBe("A");
    expect(uri.closest("a")).toBeNull();
  });

  it("says a client gave no name rather than rendering a blank", () => {
    const nameless = parseConsentContext({
      client_id: "c_x",
      client_name_unverified: "",
      identity_verified: false,
      redirect_uri: "https://x.example/cb",
      redirect_host: "x.example",
      scopes: [SCOPE_READ],
      grant_lifetime_days: 90,
    });
    renderWithProviders(<ConsentScreen {...props({ consent: nameless })} />);
    expect(screen.getByTestId("consent-client-name-absent").textContent).toMatch(
      /did not give a name/i,
    );
  });
});

describe("ConsentScreen — what it can do and what it cannot", () => {
  it("states the read permission and the four things it cannot do", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.getByText(/Read your fleet's data/i)).toBeTruthy();
    expect(screen.getByText(/It cannot change anything\./i)).toBeTruthy();
    expect(screen.getByText(/It cannot approve anything\./i)).toBeTruthy();
    expect(screen.getByText(/It cannot reach your sites directly\./i)).toBeTruthy();
    expect(screen.getByText(/It cannot read your backups' contents\./i)).toBeTruthy();
  });

  it("says who makes a change, without claiming an approval screen this connection cannot reach", () => {
    // Nothing in the product can propose anything (no proposal table, no
    // approval queue, no approval screen anywhere in the route tree). This
    // bullet used to say "It can suggest work to you and nothing more.
    // Anything that changes a site is approved by a person, in this
    // dashboard, on a screen this connection cannot reach" -- both a false
    // capability claim and a pointer at a screen that does not exist.
    renderWithProviders(<ConsentScreen {...props()} />);
    // The bold lead-in stays exactly as it was.
    expect(screen.getByText(/It cannot approve anything\./i)).toBeTruthy();
    const bullet = screen.getByText(/It cannot approve anything\./i).closest("li")!;
    expect(bullet).toHaveTextContent(/everything that changes a site is done by a person/i);
    expect(bullet).not.toHaveTextContent(/propose/i);
    expect(bullet).not.toHaveTextContent(/screen this connection cannot reach/i);
  });

  it("refuses approval when a requested scope is not recognised", () => {
    const odd = parseConsentContext({
      client_id: "c_x",
      identity_verified: false,
      redirect_uri: "https://x.example/cb",
      redirect_host: "x.example",
      scopes: [SCOPE_READ, "mcp:write"],
      grant_lifetime_days: 90,
    });
    renderWithProviders(<ConsentScreen {...props({ consent: odd })} />);
    expect(screen.getByTestId("consent-unrecognised-scope")).toBeTruthy();
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("tells the user where to revoke it", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.getByText(/AI connections/i)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The lifetime sentence
// ---------------------------------------------------------------------------
//
// THIS SCREEN SHIPPED A FALSE ONE. It said the connection "does not expire on
// its own", over a comment asserting mcp_grants had no expires_at column. m127
// made that column NOT NULL and CreateGrant stamps every grant with
// grantAbsoluteTTL, so the sentence was untrue at the exact moment a person
// decides how long to authorise an assistant for. It was predicted in writing
// before it shipped, which is why the tests below assert the replacement BY
// VALUE and pin the retired wording as a thing that must never render again.
//
// Rendered through the real router and a real QueryClient (withRouter: true),
// because a copy assertion that only ever sees a bare component is one mount
// away from being about something the user does not get.
describe("ConsentScreen — the lifetime it states is the one the server stamps", () => {
  it("states the term in days, from the payload, not from a constant in this file", async () => {
    const shortTerm = parseConsentContext({
      client_id: "c_short",
      identity_verified: false,
      redirect_uri: "https://x.example/cb",
      redirect_host: "x.example",
      scopes: [SCOPE_READ],
      // NOT 90. If the screen ever hard-codes the term, or recomputes it from
      // its own idea of the default, this is the assertion that catches it.
      grant_lifetime_days: 30,
    });
    renderWithProviders(<ConsentScreen {...props({ consent: shortTerm })} />, {
      withRouter: true,
    });
    const expiry = await screen.findByTestId("consent-duration-expiry");
    expect(expiry.textContent).toBe(
      "This connection expires on its own 30 days after you approve it. Once it expires the " +
        "client is refused at its next request and reads nothing further. Getting it working " +
        "again means coming back to this screen and approving a new connection.",
    );
  });

  it("never tells the user the connection does not expire", async () => {
    renderWithProviders(<ConsentScreen {...props()} />, { withRouter: true });
    const duration = (await screen.findByTestId("consent-duration-expiry")).closest("section");
    expect(duration).not.toBeNull();
    const text = duration!.textContent ?? "";

    // The retired sentence and its two companions, each pinned as an exact
    // substring. A future edit that restores any of them reddens here.
    expect(text).not.toContain("does not expire on its own");
    expect(text).not.toContain("It lasts until you revoke it");
    expect(text).not.toContain("short-lived and renews itself");

    // And the claim in its general form, however it might be reworded. The
    // grant expires, so no phrasing of "no expiry" belongs on this screen.
    expect(text).not.toMatch(/does not expire|never expires|no expiry|until you revoke it/i);

    // The truthful half of the old copy survives: revocation still works and
    // is immediate.
    expect(text).toContain("You can end it sooner in Settings, under AI connections");
    expect(text).toContain("takes effect immediately");
  });

  it("says nothing about idle expiry, which is not in force", async () => {
    // mcp_grants.idle_expire_after_days exists but CreateGrant writes NULL
    // (apps/api/internal/mcp/service.go, IdleExpireAfterDays: nil) and NULL
    // means never idle-expire. A caveat about a control that is not running
    // would be noise on a screen whose whole job is candour.
    renderWithProviders(<ConsentScreen {...props()} />, { withRouter: true });
    const duration = (await screen.findByTestId("consent-duration-expiry")).closest("section");
    const text = duration!.textContent ?? "";
    expect(text).not.toMatch(/idle|inactiv|unused|if you stop using/i);
  });

  it("keeps the duration copy clear of the words that overclaim", async () => {
    renderWithProviders(<ConsentScreen {...props()} />, { withRouter: true });
    const duration = (await screen.findByTestId("consent-duration-expiry")).closest("section");
    expect(bannedWordHits(duration!.textContent ?? "")).toEqual([]);
  });
});

describe("ConsentScreen — an empty site scope is a working state, not an error (2026-08-23 revision)", () => {
  it("starts with nothing selected, states the consequence, and CAN be approved", () => {
    // Superseded framing (pre-2026-08-23): this blocked approval. The current
    // wireframe rules an empty allowlist mints a credential that reads and
    // proposes nothing, decided later -- a working state, not an error.
    renderWithProviders(<ConsentScreen {...props()} />);
    const summary = screen.getByTestId("consent-scope-summary").textContent ?? "";
    expect(summary).toMatch(/No sites are selected/i);
    expect(summary).toMatch(/will read nothing/i);
    expect(summary).not.toMatch(/propose/i);
    expect(summary).toMatch(/working state, not an error/i);
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(false);
  });

  it("submits an empty list-mode scope when approved with nothing selected", () => {
    const onApprove = vi.fn();
    renderWithProviders(<ConsentScreen {...props({ onApprove })} />);
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
    expect(onApprove).toHaveBeenCalledWith({
      name: "Claude Desktop",
      siteScopeMode: "list",
      scopeTagIds: [],
      scopeSiteIds: [],
    });
  });

  it("a tag matching no site says so, states the consequence, and CAN be approved", () => {
    renderWithProviders(
      <ConsentScreen
        {...props({ tags: [{ id: "t9", name: "archived" }], tagsBySiteId: { s1: [], s2: [] } })}
      />,
    );
    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /archived/i }));
    const summary = screen.getByTestId("consent-scope-summary");
    expect(summary.textContent).toMatch(/matches no sites/i);
    expect(summary.textContent).toMatch(/will read nothing/i);
    expect(summary.textContent).not.toMatch(/propose/i);
    expect(summary.textContent).toMatch(/working state, not an error/i);
    expect(screen.queryByTestId("consent-scope-sites")).toBeNull();
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(false);
  });

  it("a failed site load blocks approval instead of resolving to zero or to all", () => {
    renderWithProviders(<ConsentScreen {...props({ fleet: null, sitesLoading: false })} />);
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(
      /could not read every site/i,
    );
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("enumerates the covered sites rather than only counting them", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    fireEvent.click(screen.getByRole("checkbox", { name: /Alpha/i }));
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(/^1 site, listed below/);
    expect(screen.getByTestId("consent-scope-sites").textContent).toContain("Alpha");
  });

  it("sends only the payload the chosen mode is allowed to carry", () => {
    const onApprove = vi.fn();
    renderWithProviders(<ConsentScreen {...props({ onApprove })} />);
    fireEvent.click(screen.getByRole("checkbox", { name: /Alpha/i }));
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
    expect(onApprove).toHaveBeenCalledWith({
      name: "Claude Desktop",
      siteScopeMode: "list",
      scopeTagIds: [],
      scopeSiteIds: ["s1"],
    });
  });
});


describe("ConsentScreen — a page of sites is not presented as the fleet", () => {
  // The org has more sites than the page we hold. The screen must not let the
  // operator believe the list in front of them is the whole fleet, because
  // approving "every site" then grants read access to sites they never saw.
  const CAPPED = { sites: SITES, complete: false };

  it("mode 'all' over a capped list refuses to state a fleet size", () => {
    renderWithProviders(<ConsentScreen {...props({ fleet: CAPPED })} />);
    fireEvent.click(screen.getByText("Every site"));
    const summary = screen.getByTestId("consent-scope-summary").textContent ?? "";
    expect(summary).not.toMatch(/That is \d+ sites? today/);
    expect(summary).toMatch(/cannot tell you whether there are others/i);
    // And it must not assert that more exist: listComplete false means we
    // cannot tell, not that a 201st site is out there.
    expect(summary).not.toMatch(/there are more/i);
  });

  it("labels the enumeration as partial rather than letting it pose as the list", () => {
    renderWithProviders(<ConsentScreen {...props({ fleet: CAPPED })} />);
    fireEvent.click(screen.getByText("Every site"));
    expect(screen.getByTestId("consent-list-partial").textContent).toMatch(
      /not the whole list/i,
    );
  });

  it("does NOT label the enumeration partial when the list really is complete", () => {
    // The over-fire case. A warning that shows on correct work gets ignored,
    // and then it warns about nothing.
    renderWithProviders(<ConsentScreen {...props()} />);
    fireEvent.click(screen.getByText("Every site"));
    expect(screen.queryByTestId("consent-list-partial")).toBeNull();
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(
      /That is 2 sites today/,
    );
  });

  it("warns that the site picker is not offering every site", () => {
    renderWithProviders(<ConsentScreen {...props({ fleet: CAPPED })} />);
    expect(screen.getByTestId("consent-picker-truncated").textContent).toMatch(
      /not all of them/i,
    );
  });

  it("does not warn on the picker when every site is offered", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.queryByTestId("consent-picker-truncated")).toBeNull();
  });

  it("says an all-sites grant covers sites added later", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    fireEvent.click(screen.getByText("Every site"));
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(
      /added later is covered/i,
    );
  });
});


describe("ConsentScreen — a registry that disappears under a selection", () => {
  // resolveSiteScope reads `fleet` and `tagsBySiteId`. It never reads `tags`.
  // So a registry going null AFTER a tag is ticked leaves the scope resolving
  // to a real site set while the id lookup behind it has nothing to look in,
  // and the submitted payload silently loses the tag the operator picked.
  //
  // The display path already refused to guess at null. This is its neighbour.

  it("blocks approval when the registry drops away after a tag is selected", () => {
    const onApprove = vi.fn();
    const { rerender } = renderWithProviders(
      <ConsentScreen {...props({ onApprove, tags: [{ id: "t1", name: "prod" }] })} />,
    );

    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /prod/i }));

    // The selection is live and approvable while the registry is there.
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(false);

    // The registry disappears underneath the operator, selection intact.
    rerender(<ConsentScreen {...props({ onApprove, tags: null })} />);

    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
    expect(screen.getByTestId("consent-tags-failed")).toBeTruthy();
  });

  it("submits nothing at all rather than a tag scope with an empty tag list", () => {
    const onApprove = vi.fn();
    const { rerender } = renderWithProviders(
      <ConsentScreen {...props({ onApprove, tags: [{ id: "t1", name: "prod" }] })} />,
    );

    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /prod/i }));
    rerender(<ConsentScreen {...props({ onApprove, tags: null })} />);

    // Submit the form directly, past the disabled button, because the button
    // being disabled is not the same guarantee as the payload being refused.
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);

    expect(onApprove).not.toHaveBeenCalled();
  });

  it("refuses a selected tag that no longer resolves to an id", () => {
    // The registry is present but has been reloaded without the tag -- deleted
    // by someone else mid-flow. The ids resolve to [], which is the same
    // absence wearing a different shape.
    const onApprove = vi.fn();
    const { rerender } = renderWithProviders(
      <ConsentScreen {...props({ onApprove, tags: [{ id: "t1", name: "prod" }] })} />,
    );

    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /prod/i }));
    rerender(<ConsentScreen {...props({ onApprove, tags: [{ id: "t9", name: "other" }] })} />);

    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
    expect(onApprove).not.toHaveBeenCalled();
  });

  it("still submits a real tag scope when the registry is intact", () => {
    // The over-fire case. A gate that blocks correct work gets removed.
    const onApprove = vi.fn();
    renderWithProviders(
      <ConsentScreen {...props({ onApprove, tags: [{ id: "t1", name: "prod" }] })} />,
    );

    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /prod/i }));
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);

    expect(onApprove).toHaveBeenCalledWith({
      name: "Claude Desktop",
      siteScopeMode: "tags",
      scopeTagIds: ["t1"],
      scopeSiteIds: [],
    });
  });
});

describe("ConsentScreen — the site picker over a fleet we could not read", () => {
  // The neighbour the sweep found: `sites={fleet?.sites ?? []}` rendered an
  // empty picker with no explanation when the site load failed, which reads as
  // "this organisation has no sites".

  it("says the sites could not be loaded rather than showing an empty picker", () => {
    renderWithProviders(<ConsentScreen {...props({ fleet: null, sitesLoading: false })} />);
    expect(screen.getByTestId("consent-sites-failed").textContent).toMatch(
      /not the same as having no sites/i,
    );
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("does not show that message when the sites loaded", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.queryByTestId("consent-sites-failed")).toBeNull();
  });
});
