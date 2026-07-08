import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { CardPHP } from "./card-php";
import { CardPlugins } from "./card-plugins";

// P1 outcome test — GH #170 Wave 5.
//
// Before this file, `src/features/health/` had NO render test at all — every
// one of the 13 diagnostic cards was covered only by whatever pure helpers
// they happened to delegate to (`diagnostic-pick.ts`, `php-eol.ts`), never by
// actually rendering a card against a realistic `SiteDiagnosticsCard`
// payload. A regression that swapped a real field read for a hardcoded
// placeholder (or dropped the "never collected" branch, or broke the EOL
// warning flip) would pass every existing test — see the non-vacuous notes
// inline for exactly what each assertion catches.

function buildCard(
  category: string,
  payload: unknown,
  overrides: Partial<SiteDiagnosticsCard> = {},
): SiteDiagnosticsCard {
  return {
    category,
    payload,
    collected_at: "2026-07-08T00:00:00Z",
    fresh: true,
    ...overrides,
  };
}

describe("CardPHP — renders real diagnostics, not a placeholder", () => {
  it("shows the agent's real version/SAPI/memory/opcache values", () => {
    const card = buildCard("php", {
      version: "8.3.10",
      sapi: "fpm-fcgi",
      memory_limit: "256M",
      max_execution_time: 60,
      upload_max_filesize: "64M",
      opcache: { enabled: true },
    });

    renderWithProviders(<CardPHP card={card} />);

    // Non-vacuous: a version that renders a hardcoded/placeholder value (or
    // the field-absent "–") instead of the real payload fails every one of
    // these.
    expect(screen.getByText("8.3.10")).toBeInTheDocument();
    expect(screen.getByText("fpm-fcgi")).toBeInTheDocument();
    expect(screen.getByText("256M")).toBeInTheDocument();
    expect(screen.getByText("60 s")).toBeInTheDocument();
    expect(screen.getByText("64M")).toBeInTheDocument();
    expect(screen.getByText("Enabled")).toBeInTheDocument();
    expect(screen.queryByText("Disabled")).not.toBeInTheDocument();
  });

  it("flips the PHP-EOL chip to warning tone for a version already past its EOL date", () => {
    // PHP 8.1's official EOL (2025-12-31) is in the past relative to any
    // 2026+ test run — deterministically > 90 days past, so this is always
    // the warning branch (`eolTone`, php-eol.ts).
    const card = buildCard("php", { version: "8.1.5" });

    renderWithProviders(<CardPHP card={card} />);

    const chip = screen.getByText(/EOL/);
    expect(chip).toHaveClass("bg-warning-subtle", "text-warning-subtle-fg");
  });

  it("uses success tone for a version far from its EOL date", () => {
    // PHP 8.5's official EOL (2029-12-31) is always > 90 days out for any
    // test run before late 2029.
    const card = buildCard("php", { version: "8.5.0" });

    renderWithProviders(<CardPHP card={card} />);

    const chip = screen.getByText(/EOL/);
    expect(chip).toHaveClass("bg-success-subtle", "text-success-subtle-fg");
  });

  it("renders the honest 'never collected' state instead of empty/placeholder values when the agent has not shipped a payload yet", () => {
    // undefined card === category never sent by the agent (see cardFor()).
    renderWithProviders(<CardPHP card={undefined} />);

    expect(
      screen.getByText("Awaiting first sync from the agent."),
    ).toBeInTheDocument();
    expect(screen.getByText("Never")).toBeInTheDocument();
    // None of the definition-list rows (or their labels) render at all in
    // this state — the shell swaps the whole body for the honest copy above
    // rather than rendering empty-value dashes for every field.
    expect(screen.queryByText("Version")).not.toBeInTheDocument();
    expect(screen.queryByText("8.3.10")).not.toBeInTheDocument();
  });

  it("also renders the 'never collected' state when the category exists but the agent sent a null payload", () => {
    const card = buildCard("php", null);
    renderWithProviders(<CardPHP card={card} />);
    expect(
      screen.getByText("Awaiting first sync from the agent."),
    ).toBeInTheDocument();
  });
});

describe("CardPlugins — updates chip links to the Updates tab", () => {
  it("renders the real update count as a Link to /sites/$siteId/updates when updates are available", async () => {
    const card = buildCard("plugins", {
      installed_count: 24,
      active_count: 19,
      available_updates: 3,
      licensing: [
        { slug: "a", status: "present" },
        { slug: "b", status: "absent" },
      ],
    });

    renderWithProviders(<CardPlugins card={card} siteId="site-42" />, {
      withRouter: true,
    });

    const link = await screen.findByRole("link", { name: /3.*updates/ });
    expect(link).toHaveAttribute("href", "/sites/site-42/updates");

    // Non-vacuous: the definition-list body must show the SAME real counts,
    // not a placeholder — "24" installed / "19" active / "1 / 2" licensed.
    expect(screen.getByText("24")).toBeInTheDocument();
    expect(screen.getByText("19")).toBeInTheDocument();
    expect(screen.getByText("1 / 2")).toBeInTheDocument();
  });

  it("renders NO updates link when available_updates is 0", async () => {
    const card = buildCard("plugins", {
      installed_count: 10,
      active_count: 10,
      available_updates: 0,
      licensing: [],
    });

    renderWithProviders(<CardPlugins card={card} siteId="site-42" />, {
      withRouter: true,
    });

    // First paint under the router harness is async — wait for something
    // certain to exist before asserting an absence.
    await screen.findByText("Plugins");
    expect(
      screen.queryByRole("link", { name: /updates/ }),
    ).not.toBeInTheDocument();
  });
});
