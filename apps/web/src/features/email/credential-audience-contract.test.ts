import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, it, expect } from "vitest";

import { CREDENTIAL_AUDIENCE_FIELDS } from "./email-provider-config";

/**
 * THE RULE, READ FROM THE SERVER RATHER THAN RESTATED HERE.
 *
 * The control plane lends the organisation's email credential to a site only
 * while that site's config still matches the organisation's on the fields that
 * decide where the credential goes and who it authenticates as. Diverge on one
 * of them and the credential is withheld and the site's copy revoked.
 *
 * The page mirrors that list so it can warn before a save rather than after,
 * which means the rule exists twice. Two lists that only have to agree with
 * themselves are exactly how apps/web/src/features/auth/social-errors.test.ts
 * ended up asserting three error codes no server path could emit. So this suite
 * parses credentialAudienceFields out of the Go source and holds the page's copy
 * against it in both directions: a provider or a field on one side and not the
 * other fails here, whichever way it drifted.
 *
 * A mismatch is not cosmetic. A field the page has and the server does not makes
 * it warn that a working credential is about to be lost. A field the server has
 * and the page does not makes it stay silent while the credential is revoked,
 * which is the shape of GH #380 itself.
 */
// Resolved from the vitest root (apps/web), which is where this suite runs.
const EMAIL_SERVICE_GO = resolve(process.cwd(), "../api/internal/email/service.go");

/** Every provider the Go table answers for, and its audience fields in order. */
function serverAudienceFields(): Map<string, string[]> {
  const source = readFileSync(EMAIL_SERVICE_GO, "utf8");
  const fn =
    /func credentialAudienceFields\(provider string\) \(\[\]string, bool\) \{([\s\S]*?)\n\}/.exec(
      source,
    );
  if (!fn) {
    throw new Error(
      `credentialAudienceFields not found in ${EMAIL_SERVICE_GO}. It is the source of truth for which config fields bind a credential to an endpoint; if it moved or changed signature, point this test at it rather than reinstating a hand-written list.`,
    );
  }
  const body = fn[1] ?? "";
  const table = new Map<string, string[]>();
  // Each `case "a", "b":` block up to the next case or the default.
  for (const block of body.matchAll(
    /\n\tcase ((?:"[a-z_]+"(?:,\s*)?)+):([\s\S]*?)(?=\n\tcase |\n\tdefault:)/g,
  )) {
    const slugs = [...(block[1] ?? "").matchAll(/"([a-z_]+)"/g)].map(([, s]) => s as string);
    const ret = /return (nil|\[\]string\{([^}]*)\}), (true|false)/.exec(block[2] ?? "");
    if (!ret) {
      throw new Error(`no parseable return for case ${slugs.join(", ")}`);
    }
    if (ret[3] !== "true") {
      // A case that reports the provider as unknown is not part of the table.
      continue;
    }
    const fields =
      ret[1] === "nil"
        ? []
        : [...(ret[2] ?? "").matchAll(/"([a-z_]+)"/g)].map(([, f]) => f as string);
    for (const slug of slugs) table.set(slug, fields);
  }
  return table;
}

describe("the credential-audience rule", () => {
  const server = serverAudienceFields();
  const ui = CREDENTIAL_AUDIENCE_FIELDS;

  it("reads a non-empty table from the server", () => {
    // A regex that silently matched nothing would make every assertion below
    // vacuous, which is the failure mode this whole approach exists to avoid.
    expect(server.size).toBeGreaterThan(3);
    expect([...server.values()].flat().length).toBeGreaterThan(5);
  });

  it("answers for exactly the providers the server answers for", () => {
    expect([...server.keys()].sort()).toEqual(Object.keys(ui).sort());
  });

  it("watches exactly the fields the server binds the credential to", () => {
    for (const [provider, fields] of server) {
      expect([...(ui[provider] ?? [])].sort(), provider).toEqual([...fields].sort());
    }
  });

  it("keeps the providers whose endpoint nobody supplies deliberately empty", () => {
    // sendgrid and postmark reach the provider's own endpoint, so there is no
    // operator-supplied field that could redirect their credential. An entry
    // that quietly grew a field would start warning about a divergence the
    // server does not see.
    for (const provider of ["sendgrid", "postmark"]) {
      expect(server.get(provider), `${provider} on the server`).toEqual([]);
      expect(ui[provider], `${provider} on the page`).toEqual([]);
    }
  });
});
