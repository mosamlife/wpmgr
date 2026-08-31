import { describe, it, expect } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { SiteEnforcementBox } from "./site-enforcement-box";
import { bannedWordHits } from "./site-enforcement";
import type { ResolvedSiteScope, ScopedSite } from "./site-scope";

const SITES: ScopedSite[] = [
  { id: "s1", name: "Alpha", url: "https://alpha.example" },
  { id: "s2", name: "Beta", url: "https://beta.example" },
];

const LIST_SCOPE: ResolvedSiteScope = {
  kind: "sites",
  sites: SITES,
  basis: "list",
  listComplete: true,
};

const TAG_SCOPE: ResolvedSiteScope = {
  kind: "sites",
  sites: SITES,
  basis: "tags",
  listComplete: true,
};

const ALL_SCOPE: ResolvedSiteScope = {
  kind: "all",
  shown: SITES,
  listComplete: true,
};

describe("SiteEnforcementBox — named-sites mode", () => {
  it("states the request-time check and that no other site is covered", () => {
    renderWithProviders(<SiteEnforcementBox scope={LIST_SCOPE} />);
    const box = screen.getByTestId("site-enforcement-box");
    expect(box.textContent).toMatch(/checked against this list before WPMgr contacts any site/i);
    expect(box.textContent).toMatch(/refused and recorded in the audit log/i);
    expect(box.textContent).toMatch(/no other site is covered/i);
    // The drift sentence belongs to tag mode only.
    expect(box.textContent).not.toMatch(/included automatically/i);
  });

  it("omits the refusals line when no refusals summary is supplied", () => {
    renderWithProviders(<SiteEnforcementBox scope={LIST_SCOPE} />);
    expect(screen.queryByTestId("site-enforcement-refusals")).toBeNull();
  });
});

describe("SiteEnforcementBox — tag mode states the drift consequence", () => {
  it("says sites added to the tag later are included automatically, without approval", () => {
    renderWithProviders(<SiteEnforcementBox scope={TAG_SCOPE} />);
    const box = screen.getByTestId("site-enforcement-box");
    expect(box.textContent).toMatch(/included automatically/i);
    expect(box.textContent).toMatch(/without anyone approving it/i);
  });
});

describe("SiteEnforcementBox — every-site mode replaces the box rather than decorating it", () => {
  it("says nothing is checked because there is no list", () => {
    renderWithProviders(<SiteEnforcementBox scope={ALL_SCOPE} />);
    const box = screen.getByTestId("site-enforcement-box");
    expect(box.dataset.mode).toBe("all");
    expect(box.textContent).toMatch(/nothing is checked against a list because there is no list/i);
  });

  it("has no 'how we check this' link — every-site mode has no list to explain", () => {
    renderWithProviders(<SiteEnforcementBox scope={ALL_SCOPE} />);
    expect(screen.queryByText(/how we check this/i)).toBeNull();
  });
});

describe("SiteEnforcementBox — 'none' and 'unresolved' render nothing", () => {
  it("renders no box when nothing is selected", () => {
    renderWithProviders(
      <SiteEnforcementBox scope={{ kind: "none", because: "no-selection" }} />,
    );
    expect(screen.queryByTestId("site-enforcement-box")).toBeNull();
  });

  it("renders no box while the scope is unresolved", () => {
    renderWithProviders(
      <SiteEnforcementBox scope={{ kind: "unresolved", because: "loading" }} />,
    );
    expect(screen.queryByTestId("site-enforcement-box")).toBeNull();
  });
});

describe("SiteEnforcementBox — the refusals block, when supplied", () => {
  it("renders an explicit not-tracked sentence rather than a zero or a dash", () => {
    renderWithProviders(<SiteEnforcementBox scope={LIST_SCOPE} refusals={{ kind: "unavailable" }} />);
    const line = screen.getByTestId("site-enforcement-refusals");
    expect(line.textContent).toMatch(/do not yet track/i);
    expect(line.textContent?.trim()).not.toBe("0");
  });

  it("renders a real zero as a stated fact", () => {
    renderWithProviders(
      <SiteEnforcementBox scope={LIST_SCOPE} refusals={{ kind: "zero", windowDays: 7 }} />,
    );
    expect(screen.getByTestId("site-enforcement-refusals").textContent).toMatch(
      /no requests have been refused/i,
    );
  });

  it("renders a real count", () => {
    renderWithProviders(
      <SiteEnforcementBox scope={LIST_SCOPE} refusals={{ kind: "count", count: 2, windowDays: 7 }} />,
    );
    expect(screen.getByTestId("site-enforcement-refusals").textContent).toMatch(
      /refused 2 times in the last 7 days/i,
    );
  });
});

describe("SiteEnforcementBox — the 'How we check this' dialog", () => {
  it("opens on click and says this is a check the application makes, not a database boundary", () => {
    renderWithProviders(<SiteEnforcementBox scope={TAG_SCOPE} />);
    fireEvent.click(screen.getByText(/how we check this/i));
    expect(screen.getByText("How WPMgr checks site access")).toBeTruthy();
    expect(screen.getByText(/is a check WPMgr makes when the assistant asks/i)).toBeTruthy();
    expect(screen.getByText(/not a separate boundary inside the database/i)).toBeTruthy();
  });

  it("closes on the Close button", () => {
    renderWithProviders(<SiteEnforcementBox scope={TAG_SCOPE} />);
    fireEvent.click(screen.getByText(/how we check this/i));
    fireEvent.click(screen.getByText("Close"));
    expect(screen.queryByText("How WPMgr checks site access")).toBeNull();
  });
});

describe("SiteEnforcementBox — banned-word guard over the mounted screen", () => {
  // Plant the guard against what actually renders, not against the source
  // file: a component could import clean constants and still interpolate a
  // banned word around them.
  it("every mode's rendered text is free of the wireframe's banned words", () => {
    for (const scope of [LIST_SCOPE, TAG_SCOPE, ALL_SCOPE]) {
      const { unmount } = renderWithProviders(
        <SiteEnforcementBox
          scope={scope}
          refusals={{ kind: "count", count: 2, windowDays: 7 }}
        />,
      );
      const box = screen.getByTestId("site-enforcement-box");
      expect(bannedWordHits(box.textContent ?? "")).toEqual([]);
      unmount();
    }
  });

  it("the 'How we check this' dialog is free of the wireframe's banned words", () => {
    renderWithProviders(<SiteEnforcementBox scope={LIST_SCOPE} />);
    fireEvent.click(screen.getByText(/how we check this/i));
    const dialogText = screen.getByText("How WPMgr checks site access").closest("div")?.parentElement
      ?.textContent;
    expect(bannedWordHits(dialogText ?? "")).toEqual([]);
  });
});
