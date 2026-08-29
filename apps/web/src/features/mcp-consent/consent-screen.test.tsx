import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { ConsentScreen, type ConsentScreenProps } from "./consent-screen";
import { parseConsentContext, SCOPE_READ } from "./consent-context";
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
    allSites: SITES,
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

  it("refuses approval when a requested scope is not recognised", () => {
    const odd = parseConsentContext({
      client_id: "c_x",
      identity_verified: false,
      redirect_uri: "https://x.example/cb",
      redirect_host: "x.example",
      scopes: [SCOPE_READ, "mcp:write"],
    });
    renderWithProviders(<ConsentScreen {...props({ consent: odd })} />);
    expect(screen.getByTestId("consent-unrecognised-scope")).toBeTruthy();
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("tells the user the grant lasts until revoked and where to revoke it", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.getByText(/does not expire on its own/i)).toBeTruthy();
    expect(screen.getByText(/AI connections/i)).toBeTruthy();
  });
});

describe("ConsentScreen — an empty site scope is not everything", () => {
  it("starts with nothing selected and cannot be approved", () => {
    renderWithProviders(<ConsentScreen {...props()} />);
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(
      /No sites are selected yet/i,
    );
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("a tag matching no site says so, never that it covers everything", () => {
    renderWithProviders(
      <ConsentScreen
        {...props({ tags: [{ id: "t9", name: "archived" }], tagsBySiteId: { s1: [], s2: [] } })}
      />,
    );
    fireEvent.click(screen.getByText("Sites with a tag"));
    fireEvent.click(screen.getByRole("checkbox", { name: /archived/i }));
    const summary = screen.getByTestId("consent-scope-summary");
    expect(summary.textContent).toMatch(/matches no sites/i);
    expect(summary.textContent).toMatch(/read nothing/i);
    expect(screen.queryByTestId("consent-scope-sites")).toBeNull();
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("a failed site load blocks approval instead of resolving to zero or to all", () => {
    renderWithProviders(<ConsentScreen {...props({ allSites: null, sitesLoading: false })} />);
    expect(screen.getByTestId("consent-scope-summary").textContent).toMatch(
      /could not load your sites/i,
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
