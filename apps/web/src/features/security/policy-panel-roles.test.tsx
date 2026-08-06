import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { PolicyPanel } from "./policy-panel";
import {
  useSiteSecurityPolicy,
  buildRoleOptions,
  DEFAULT_WP_ROLES,
  type SiteSecurityPolicy,
} from "./use-policy";

// GH #350: the password/2FA policy could only ever be applied to the five
// default WordPress roles, because the panel rendered a hardcoded list of them.
// A WooCommerce site's shop manager, who has elevated access, could not be given
// a password strength policy at all. The agent has always ENFORCED the policy
// against whatever role slug a user really holds; only discovery was broken.
//
// Every test in this file FAILS against the pre-change code, where policy-panel
// rendered `const WP_ROLES = [...]` regardless of what the site reported and
// `buildRoleOptions` did not exist.
//
// Only `useSiteSecurityPolicy` is mocked. `useUpdateSiteSecurityPolicy` stays
// real: it is a `useMutation` that never touches the network until `.mutate()`
// is called, which these tests never do.

vi.mock("./use-policy", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-policy")>();
  return { ...actual, useSiteSecurityPolicy: vi.fn() };
});

const mockedUsePolicy = vi.mocked(useSiteSecurityPolicy);

function buildPolicy(overrides: Partial<SiteSecurityPolicy> = {}): SiteSecurityPolicy {
  return {
    two_factor_enabled: false,
    two_factor_methods: ["totp", "backup"],
    two_factor_required_roles: [],
    two_factor_grace_logins: 3,
    two_factor_remember_device_days: 30,
    block_xmlrpc_for_2fa_users: true,
    // Non-zero so the "Apply strength rule to" role picker renders at all.
    password_min_zxcvbn_score: 3,
    password_min_zxcvbn_roles: [],
    password_block_compromised: false,
    password_reuse_block_count: 0,
    password_max_age_days: 0,
    password_expiry_roles: [],
    hide_backend_enabled: false,
    hide_backend_slug: "",
    hide_backend_redirect: "",
    ...overrides,
  };
}

function renderPanel(policy: SiteSecurityPolicy) {
  mockedUsePolicy.mockReturnValue(
    mockQueryResult<SiteSecurityPolicy>({ data: policy }),
  );
  return renderWithProviders(
    <PolicyPanel siteId="site-1" canWrite section="password" />,
  );
}

/** The chips inside the "Apply strength rule to" picker. */
function strengthRoleGroup(): HTMLElement {
  return screen.getByRole("group", { name: /apply strength rule to/i });
}

// The reporter's real site: a WooCommerce install in Italian.
const REPORTED_ROLES = [
  { slug: "shop_manager", name: "Gestore negozio" },
  { slug: "translator", name: "Translator" },
  { slug: "calandrini_staff", name: "Calandrini Staff" },
  { slug: "customer_full_price", name: "Customer prezzo pieno" },
  { slug: "customer", name: "Cliente" },
  { slug: "administrator", name: "Amministratore" },
];

describe("PolicyPanel role chips (GH #350)", () => {
  // T4
  it("renders the roles the site reported instead of the hardcoded five", () => {
    renderPanel(buildPolicy({ site_roles: REPORTED_ROLES }));

    const group = strengthRoleGroup();
    for (const role of REPORTED_ROLES) {
      expect(
        within(group).getByRole("checkbox", { name: new RegExp(role.name, "i") }),
      ).toBeInTheDocument();
    }

    // The English defaults the panel used to hardcode must be GONE: the site
    // does not have an "Editor" and offering one is the bug.
    expect(
      within(group).queryByRole("checkbox", { name: /^editor$/i }),
    ).not.toBeInTheDocument();
    expect(
      within(group).queryByRole("checkbox", { name: /^subscriber$/i }),
    ).not.toBeInTheDocument();
  });

  it("keeps the slug discoverable when the display name does not identify the role", () => {
    renderPanel(buildPolicy({ site_roles: REPORTED_ROLES }));

    const group = strengthRoleGroup();
    // Two plugins can produce similar names, so the slug is shown whenever the
    // name alone cannot identify the role.
    expect(within(group).getByText("shop_manager")).toBeInTheDocument();
    expect(within(group).getByText("administrator")).toBeInTheDocument();
  });

  it("stores the slug, not the display name, when a role is selected", () => {
    renderPanel(buildPolicy({ site_roles: REPORTED_ROLES }));

    const chip = within(strengthRoleGroup()).getByRole("checkbox", {
      name: /Gestore negozio/i,
    });
    // The input id is derived from the slug the policy will store. If the panel
    // ever started keying on the display name, enforcement would silently stop
    // matching, because the agent matches slugs.
    expect(chip).toHaveAttribute("id", "pw-strength-roles-shop_manager");

    fireEvent.click(chip);
    expect(chip).toBeChecked();
  });

  // T5
  it("says so out loud when the site has not reported its roles", () => {
    renderPanel(buildPolicy({ site_roles: [] }));

    const group = strengthRoleGroup();
    // The defaults still appear so the panel is usable...
    expect(
      within(group).getByRole("checkbox", { name: /^administrator$/i }),
    ).toBeInTheDocument();
    // ...but never silently: a silent fallback is what produced this bug.
    const notice = within(group).getByRole("note");
    expect(notice).toHaveTextContent(/has not reported its WordPress roles/i);
    expect(notice).toHaveTextContent(/custom roles added by plugins/i);
  });

  it("shows no fallback notice once the site has reported its roles", () => {
    renderPanel(buildPolicy({ site_roles: REPORTED_ROLES }));

    expect(within(strengthRoleGroup()).queryByRole("note")).not.toBeInTheDocument();
  });

  // T3, dashboard half
  it("keeps a selected role the site no longer has visible, flagged and removable", () => {
    renderPanel(
      buildPolicy({
        site_roles: REPORTED_ROLES,
        password_min_zxcvbn_roles: ["shop_manager", "deactivated_plugin_role"],
      }),
    );

    const group = strengthRoleGroup();
    const stale = within(group).getByRole("checkbox", {
      name: /deactivated_plugin_role/i,
    });
    expect(stale).toBeChecked();
    // Flagged, so an operator can see WHY the rule is not applying.
    expect(
      within(group).getByText(/not on this site/i),
    ).toBeInTheDocument();

    // ...and removable.
    fireEvent.click(stale);
    expect(stale).not.toBeChecked();
  });

  it("does not call a role missing when it does not know the site's roles", () => {
    renderPanel(
      buildPolicy({
        site_roles: [],
        password_min_zxcvbn_roles: ["shop_manager"],
      }),
    );

    const group = strengthRoleGroup();
    expect(
      within(group).getByRole("checkbox", { name: /shop_manager/i }),
    ).toBeChecked();
    // Under the fallback we do not know the site's role set, so claiming the
    // role is absent would be a guess presented as a fact.
    expect(within(group).queryByText(/not on this site/i)).not.toBeInTheDocument();
  });

  // C4
  it("stays usable on a site with dozens of roles", () => {
    const many = Array.from({ length: 60 }, (_, i) => ({
      slug: `membership_tier_${i}`,
      name: `Membership tier ${i}`,
    }));
    renderPanel(buildPolicy({ site_roles: many }));

    const group = strengthRoleGroup();
    expect(within(group).getAllByRole("checkbox")).toHaveLength(60);

    // Above the threshold the picker grows a filter so a specific role is
    // findable without scrolling 60 chips.
    const filter = within(group).getByRole("searchbox");
    fireEvent.change(filter, { target: { value: "tier_42" } });
    expect(within(group).getAllByRole("checkbox")).toHaveLength(1);
    expect(
      within(group).getByRole("checkbox", { name: /Membership tier 42/i }),
    ).toBeInTheDocument();
  });

  it("does not show a filter for a normal-sized role list", () => {
    renderPanel(buildPolicy({ site_roles: REPORTED_ROLES }));

    expect(within(strengthRoleGroup()).queryByRole("searchbox")).not.toBeInTheDocument();
  });
});

describe("buildRoleOptions", () => {
  it("prefers the reported roles over the defaults", () => {
    const { options, usingFallback } = buildRoleOptions(
      [{ slug: "shop_manager", name: "Gestore negozio" }],
      [],
    );
    expect(usingFallback).toBe(false);
    expect(options.map((o) => o.value)).toEqual(["shop_manager"]);
  });

  it("flags the fallback when nothing was reported", () => {
    for (const reported of [undefined, [], [{ slug: "  ", name: "junk" }]]) {
      const { options, usingFallback } = buildRoleOptions(reported, []);
      expect(usingFallback).toBe(true);
      expect(options.map((o) => o.value)).toEqual(
        DEFAULT_WP_ROLES.map((r) => r.slug),
      );
    }
  });

  it("hides the slug when the name obviously corresponds to it", () => {
    const { options } = buildRoleOptions(
      [
        { slug: "administrator", name: "Administrator" },
        { slug: "shop_manager", name: "Shop manager" },
      ],
      [],
    );
    expect(options.every((o) => !o.showSlug)).toBe(true);
  });

  it("shows the slug when the name is localized or ambiguous", () => {
    const { options } = buildRoleOptions(
      [
        { slug: "administrator", name: "Amministratore" },
        { slug: "staff_a", name: "Staff" },
        { slug: "staff_b", name: "Staff" },
      ],
      [],
    );
    expect(options.map((o) => o.showSlug)).toEqual([true, true, true]);
  });

  it("appends a selected role that is not in the reported list", () => {
    const { options } = buildRoleOptions(
      [{ slug: "shop_manager", name: "Gestore negozio" }],
      ["shop_manager", "gone"],
    );
    expect(options.map((o) => o.value)).toEqual(["shop_manager", "gone"]);
    expect(options[1]).toMatchObject({ label: "gone", missing: true });
  });

  it("never duplicates a role that is both reported and selected", () => {
    const { options } = buildRoleOptions(
      [{ slug: "shop_manager", name: "Gestore negozio" }],
      ["shop_manager"],
    );
    expect(options).toHaveLength(1);
    expect(options[0]).toMatchObject({ value: "shop_manager", missing: false });
  });
});
