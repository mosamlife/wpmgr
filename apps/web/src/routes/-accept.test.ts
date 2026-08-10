import { describe, it, expect } from "vitest";
import { buildAcceptBody } from "./accept";

// The invite-accept page has to serve two people who cannot be served the same
// way: a stranger with a link, and someone already signed in whose account has
// no password and never can have one. What follows pins the difference.

describe("buildAcceptBody", () => {
  it("sends no password for a signed-in caller, because the session is the proof", () => {
    const body = buildAcceptBody({
      token: "tok-1",
      signedInEmail: "sarah@acme.com",
      typedEmail: "",
      typedName: "",
      typedPassword: "",
    });

    expect(body).toEqual({ token: "tok-1", email: "sarah@acme.com" });
    // An account created with Google or GitHub has no password hash and can
    // never be given one. Requiring one here was a door with no key.
    expect(body).not.toHaveProperty("password");
  });

  it("uses the SESSION's address, never one typed on the page", () => {
    const body = buildAcceptBody({
      token: "tok-1",
      signedInEmail: "sarah@acme.com",
      typedEmail: "someone.else@evil.test",
      typedName: "Someone Else",
      typedPassword: "hunter2",
    });

    // The API matches the session against the account the invitation names, so
    // a typed address could only ever disagree with it. Sending it would turn a
    // mismatch into a confusing 403 instead of a straightforward accept.
    expect(body["email"]).toBe("sarah@acme.com");
    expect(body).not.toHaveProperty("password");
    expect(body).not.toHaveProperty("name");
  });

  it("still sends email + password (and an optional name) when nobody is signed in", () => {
    const body = buildAcceptBody({
      token: "tok-1",
      signedInEmail: null,
      typedEmail: "  sarah@acme.com  ",
      typedName: "  Sarah  ",
      typedPassword: "hunter2",
    });

    expect(body).toEqual({
      token: "tok-1",
      email: "sarah@acme.com",
      password: "hunter2",
      name: "Sarah",
    });
  });

  it("omits an all-whitespace display name rather than sending a blank one", () => {
    const body = buildAcceptBody({
      token: "tok-1",
      signedInEmail: null,
      typedEmail: "sarah@acme.com",
      typedName: "   ",
      typedPassword: "hunter2",
    });

    expect(body).not.toHaveProperty("name");
  });

  // The page passes signedInEmail: null once the person says the invitation
  // went elsewhere, even though a session still exists. Without that door, one
  // person's two addresses meet one invitation with ten attempts, and the
  // address the page insists on can never be the right one.
  it("sends the typed address and password once the person chooses a different one", () => {
    const body = buildAcceptBody({
      token: "tok-1",
      signedInEmail: null, // useOtherAddress is on; the session is still live
      typedEmail: "sarah@work.example",
      typedName: "",
      typedPassword: "hunter2",
    });

    expect(body).toEqual({
      token: "tok-1",
      email: "sarah@work.example",
      password: "hunter2",
    });
  });
});
