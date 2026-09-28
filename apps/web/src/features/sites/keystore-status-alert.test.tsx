import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import type { Site, SiteKeystoreStatus } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { KeystoreStatusAlert } from "./keystore-status-alert";

// GH #753 slice 1b review follow-up (PR #778 item 1). Before this file the
// alert had NO test at all: planting `if (state === "__never__") return
// null;` (i.e. always show, for every state including "ok") left the whole
// suite green. This file pins both directions: which states show the alert
// and, per item 2, exactly what each shown state says.
//
// Rendered with `withRouter: true` (see src/test/render.tsx) even though
// the component itself never touches the router, matching the project's
// component-render convention (backups-section.test.tsx). RouterProvider's
// first paint is asynchronous, so every test awaits a stable sentinel
// rendered alongside the alert before asserting either presence or absence:
// asserting synchronously right after render would pass trivially before
// anything has painted at all.

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

function siteWithKeystore(keystore_status: unknown, overrides: Partial<Site> = {}): Site {
  return buildSite({
    keystore_status: keystore_status as SiteKeystoreStatus,
    ...overrides,
  });
}

function AlertHarness({ site }: { site: Site }) {
  return (
    <>
      <span data-testid="ready" />
      <KeystoreStatusAlert site={site} />
    </>
  );
}

function renderAlert(site: Site) {
  return renderWithProviders(<AlertHarness site={site} />, {
    withRouter: true,
    initialPath: "/sites/site-1/backups",
  });
}

describe("KeystoreStatusAlert: visibility", () => {
  it("renders nothing for state ok", async () => {
    renderAlert(siteWithKeystore({ state: "ok" }));
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing for state not_reported", async () => {
    renderAlert(siteWithKeystore({ state: "not_reported" }));
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing when keystore_status is absent entirely", async () => {
    renderAlert(buildSite());
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing for a wrong-case state string ('UNREADABLE')", async () => {
    renderAlert(siteWithKeystore({ state: "UNREADABLE" }));
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing when state is object-valued instead of a string", async () => {
    renderAlert(siteWithKeystore({ state: {} }));
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders nothing when keystore_status itself is a string, not an object", async () => {
    renderAlert(siteWithKeystore("unreadable"));
    await screen.findByTestId("ready");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders for state unreadable with the backup-critical item affected", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "unreadable" },
        unreadable: ["age_identity"],
      }),
    );
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });

  it("renders for state key_unavailable", async () => {
    renderAlert(siteWithKeystore({ state: "key_unavailable" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });
});

describe("KeystoreStatusAlert: wp-admin link", () => {
  it("links to the site's wp-admin, stripping a trailing slash from the site URL", async () => {
    renderAlert(siteWithKeystore({ state: "key_unavailable" }, { url: "https://example.com/" }));
    const link = await screen.findByRole("link", { name: "Open wp-admin" });
    expect(link).toHaveAttribute("href", "https://example.com/wp-admin/");
  });
});

describe("KeystoreStatusAlert: copy branches (item 2)", () => {
  it("says backups cannot run for key_unavailable, without asserting the salts cause when key_source isn't salts", async () => {
    renderAlert(siteWithKeystore({ state: "key_unavailable", key_source: "constant" }));
    expect(await screen.findByText("Backups cannot run for this site.")).toBeInTheDocument();
    expect(screen.queryByText(/security keys changed/)).not.toBeInTheDocument();
  });

  it("says backups cannot run for key_unavailable AND states the salts cause when key_source is salts", async () => {
    renderAlert(siteWithKeystore({ state: "key_unavailable", key_source: "salts" }));
    expect(await screen.findByText("Backups cannot run for this site.")).toBeInTheDocument();
    expect(screen.getByText(/security keys changed/)).toBeInTheDocument();
  });

  it("says backups cannot run when items.age_identity is unreadable, even though state is merely 'unreadable'", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "unreadable", email_secret: "ok" },
        unreadable: ["age_identity"],
      }),
    );
    expect(await screen.findByText("Backups cannot run for this site.")).toBeInTheDocument();
  });

  it("does NOT say backups cannot run when only an email credential is unreadable, and names it in plain words instead", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", email_secret: "unreadable" },
        unreadable: ["email_secret"],
      }),
    );
    expect(
      await screen.findByText("This site's email credentials cannot be read."),
    ).toBeInTheDocument();
    expect(screen.queryByText("Backups cannot run for this site.")).not.toBeInTheDocument();
  });

  it("names connection keys in plain words when site_keypair/cp_public_key are the unreadable items", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", site_keypair: "unreadable", cp_public_key: "unreadable" },
        unreadable: ["site_keypair", "cp_public_key"],
      }),
    );
    expect(
      await screen.findByText("This site's connection keys cannot be read."),
    ).toBeInTheDocument();
    expect(screen.queryByText("Backups cannot run for this site.")).not.toBeInTheDocument();
  });

  // PR #778 item 2. With no `items` map at all (the agent sent state=unreadable
  // but the items/unreadable detail didn't parse), the old copy fell through to
  // describeUnreadableItems' empty-set fallback and rendered the ungrammatical
  // "This site's some stored credentials cannot be read.", while backupsAffected
  // silently defaulted to false, i.e. under-warning: it never said backups'
  // status was actually unknown. This pins the fixed copy: grammatical, and it
  // says plainly that whether backups are affected isn't known, never that they
  // still run.
  it("says plainly that backup impact is unknown when state is unreadable with no items map at all", async () => {
    renderAlert(siteWithKeystore({ state: "unreadable" }));
    expect(
      await screen.findByText(
        "This site has stored credentials that cannot be read, and it is not known whether backups are affected.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/some stored credentials/)).not.toBeInTheDocument();
    expect(screen.queryByText("Backups cannot run for this site.")).not.toBeInTheDocument();
  });

  // PR #778 item 1 (Greptile review). Before this fix, describeUnreadableItems
  // took `status.unreadable` directly and fell back to the no-detail copy the
  // moment that list wasn't a usable array, even when `status.items` still
  // named exactly which items were unreadable. These three pin the fallback
  // to items alone when the list is unusable: missing, malformed as an
  // object, and malformed as a string. The union with a *usable* list is
  // pinned separately below.
  it("derives the unreadable set from items when the unreadable list is missing but items is usable", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", email_secret: "unreadable" },
      }),
    );
    expect(
      await screen.findByText("This site's email credentials cannot be read."),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(
        "This site has stored credentials that cannot be read, and it is not known whether backups are affected.",
      ),
    ).not.toBeInTheDocument();
  });

  it("derives the unreadable set from items when the unreadable list is malformed (an object, not an array)", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", site_keypair: "unreadable" },
        unreadable: { age_identity: "ok", site_keypair: "unreadable" },
      }),
    );
    expect(
      await screen.findByText("This site's connection keys cannot be read."),
    ).toBeInTheDocument();
  });

  it("derives the unreadable set from items when the unreadable list is malformed (a string, not an array)", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", email_secret: "unreadable" },
        unreadable: "email_secret",
      }),
    );
    expect(
      await screen.findByText("This site's email credentials cannot be read."),
    ).toBeInTheDocument();
  });

  // PR #778 P1 (Greptile). The old rule preferred the `unreadable` list and
  // ignored `items` the moment the list was non-empty, which threw away real
  // detail whenever the two sources named different items. The resolved set
  // is now the union of both: everything either source calls unreadable gets
  // named, never fewer.
  it("unions the two sources: list names email_secret, items names connection keys, both are named", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        // list names only email_secret; items separately marks site_keypair
        // (connection keys) unreadable but not email_secret. Neither source
        // alone has both; the union must.
        items: { age_identity: "ok", email_secret: "ok", site_keypair: "unreadable" },
        unreadable: ["email_secret"],
      }),
    );
    expect(
      await screen.findByText("This site's email credentials and connection keys cannot be read."),
    ).toBeInTheDocument();
  });

  it("names an item present in both sources only once", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        items: { age_identity: "ok", email_secret: "unreadable" },
        unreadable: ["email_secret"],
      }),
    );
    // Singular phrasing proves the union deduplicated rather than naming
    // "email credentials" twice (which formatList would otherwise join with
    // "and", producing a garbled, doubled heading).
    expect(
      await screen.findByText("This site's email credentials cannot be read."),
    ).toBeInTheDocument();
  });

  // PR #778 item 3 (CodeRabbit) plus the item-1 P1 fix above. `backupsAffected`
  // is computed from the same resolved (unioned) set `describeUnreadableItems`
  // uses, so it can never disagree with the heading's own item list, and it
  // can never miss an item that only one of the two sources named.
  it("says backups cannot run when the unreadable list names age_identity and the items map is missing entirely", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        unreadable: ["age_identity"],
      }),
    );
    expect(await screen.findByText("Backups cannot run for this site.")).toBeInTheDocument();
  });

  it("says backups cannot run when the list omits age_identity but items still marks it unreadable, per the union rule", async () => {
    renderAlert(
      siteWithKeystore({
        state: "unreadable",
        // The list is non-empty and usable, but omits age_identity; only
        // `items` names it. The old "list wins" rule dropped it here and
        // showed no backup warning at all, which is the wrong failure for a
        // warning about backups not running.
        items: { age_identity: "unreadable", email_secret: "ok" },
        unreadable: ["email_secret"],
      }),
    );
    expect(await screen.findByText("Backups cannot run for this site.")).toBeInTheDocument();
  });
});
