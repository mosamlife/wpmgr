import { describe, it, expect } from "vitest";

import { buildDenialTarget } from "./use-consent";
import { parseConsentContext, SCOPE_READ } from "./consent-context";

// REFUSAL IS A PROTOCOL MESSAGE.
//
// RFC 6749 section 4.1.2.1: when the operator denies the request, the
// authorization server redirects back to the client with `error=access_denied`
// and the original `state`. A screen that answers "no" with history.back()
// leaves the initiating client waiting for a callback that never arrives, so
// the user's refusal is invisible to the thing they refused.

function ctx(over: Record<string, unknown> = {}) {
  return parseConsentContext({
    client_id: "c1",
    client_name_unverified: "Some Client",
    identity_verified: false,
    redirect_uri: "https://client.example/oauth/cb",
    redirect_host: "client.example",
    scopes: [SCOPE_READ],
    state: "opaque-csrf-token",
    ...over,
  });
}

describe("buildDenialTarget", () => {
  it("returns access_denied to the client", () => {
    const url = new URL(buildDenialTarget(ctx()));
    expect(url.origin + url.pathname).toBe("https://client.example/oauth/cb");
    expect(url.searchParams.get("error")).toBe("access_denied");
  });

  it("echoes the original state unchanged", () => {
    // The client's own CSRF token, and the only way it can match this answer to
    // the request it sent. Dropping it makes the refusal an unattributable
    // stray callback.
    const url = new URL(buildDenialTarget(ctx()));
    expect(url.searchParams.get("state")).toBe("opaque-csrf-token");
  });

  it("omits state when the request carried none, rather than inventing one", () => {
    const url = new URL(buildDenialTarget(ctx({ state: "" })));
    expect(url.searchParams.has("state")).toBe(false);
    expect(url.searchParams.get("error")).toBe("access_denied");
  });

  it("never carries an authorization code", () => {
    const url = new URL(buildDenialTarget(ctx()));
    expect(url.searchParams.has("code")).toBe(false);
  });

  it("uses the SERVER's exact-matched redirect URI, not one from the request", () => {
    // internal/mcp/service.go only fills ConsentContext.RedirectURI after
    // exactMatchRedirectURI proves it equals a registered URI. Building this
    // from the browser's query string instead would make the refusal path an
    // open redirector on a screen whose premise is that the request is
    // untrusted.
    const target = buildDenialTarget(ctx({ redirect_uri: "https://registered.example/cb" }));
    expect(target.startsWith("https://registered.example/cb")).toBe(true);
  });

  it("preserves a query string the registered URI already carried", () => {
    const url = new URL(
      buildDenialTarget(ctx({ redirect_uri: "https://client.example/cb?tenant=acme" })),
    );
    expect(url.searchParams.get("tenant")).toBe("acme");
    expect(url.searchParams.get("error")).toBe("access_denied");
  });
});
