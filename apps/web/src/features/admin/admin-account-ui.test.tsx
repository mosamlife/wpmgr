import { describe, it, expect } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";

import { TooltipProvider } from "@/components/ui/tooltip";
import {
  AccountStatusBadge,
  AdminMeterList,
  PlanBadge,
  SitesMeterChip,
  StorageMeterChip,
} from "./admin-account-ui";
import { buildAccountMeters } from "./admin-accounts-format";

// Regression guard for the exact prod crash class: "Cannot read properties of
// undefined (reading 'toLocaleString')" in the /admin/accounts panel. These
// components are shallow-rendered (react-dom/server, no jsdom needed) against
// a MINIMAL backend payload — only required fields present, every omitempty
// field genuinely absent — mirroring the real AdminAccountListItem /
// AdminAccountUsage shapes in use-admin-accounts.ts. A future field-name drift
// that reintroduces an unguarded `.toLocaleString()`/`.toLocaleDateString()`
// on an absent field will throw here, not in a customer's browser.

describe("admin-account-ui shallow render — minimal payload", () => {
  it("AccountStatusBadge renders with no suspended_at (the omitempty case)", () => {
    expect(() =>
      renderToStaticMarkup(
        <AccountStatusBadge account={{ plan_status: "none", suspended_at: undefined }} />,
      ),
    ).not.toThrow();
  });

  it("AccountStatusBadge renders when suspended_at IS present", () => {
    expect(() =>
      renderToStaticMarkup(
        <AccountStatusBadge
          account={{ plan_status: "active", suspended_at: "2026-07-01T00:00:00Z" }}
        />,
      ),
    ).not.toThrow();
  });

  it("PlanBadge renders for the free plan with no overrides", () => {
    expect(() =>
      renderToStaticMarkup(<PlanBadge plan="free" comped={false} hasOverrides={false} />),
    ).not.toThrow();
  });

  it("SitesMeterChip renders at 0/1 (a brand-new minimal account)", () => {
    const html = renderToStaticMarkup(<SitesMeterChip used={0} cap={1} />);
    expect(html).not.toContain("undefined");
  });

  it("StorageMeterChip renders when storage_cap_bytes is 0 (free tier: no CP-managed cap)", () => {
    const html = renderToStaticMarkup(
      <TooltipProvider>
        <StorageMeterChip usedBytes={0} capBytes={0} />
      </TooltipProvider>,
    );
    expect(html).not.toContain("undefined");
    expect(html).toContain("no cap");
  });

  it("AdminMeterList renders the meters built from a minimal usage block (0/0 storage cap included)", () => {
    const meters = buildAccountMeters({
      sites: { used: 0, cap: 1 },
      storage_bytes_approx: { used: 0, cap: 0 },
    });
    const html = renderToStaticMarkup(<AdminMeterList meters={meters} />);
    expect(html).not.toContain("undefined");
    expect(html).toContain("no cap");
  });

  it("AdminMeterList renders an empty meters array without throwing", () => {
    expect(() => renderToStaticMarkup(<AdminMeterList meters={[]} />)).not.toThrow();
  });
});
