import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AbilityRequest } from "@wpmgr/api";
import { AbilityRequestCard } from "./ability-request-card";

afterEach(cleanup);

describe("cleared request record", () => {
  it.each(["wordpress", "elementor"])("keeps %s draft links", (editor) => {
    const request = {
      id: "r1",
      site_id: "s1",
      ability_name: "wpmgr/page-create",
      input_json: "{}",
      effect_copy: "draft",
      snapshot: "created_post_trash",
      site_label: "Shop",
      site_host: "shop.example",
      grant_label: "Laptop",
      grant_via: "token",
      card_copy_version: 1,
      state: "done",
      outcome: "created",
      created_at: "2026-09-01T10:00:00Z",
      expires_at: "2026-09-01T11:00:00Z",
      content_cleared_at: "2026-10-13T10:00:00Z",
      created_post_id: 418,
      editor: editor === "elementor" ? "builder:elementor" : "wordpress_blocks",
      undo_offered: false,
      resolve_gave_up: false,
      card_facts: null,
      page_edit: null,
      page_media: null,
    } satisfies AbilityRequest;
    render(
      <AbilityRequestCard
        request={request}
        siteUrl="https://shop.example"
        onApprove={vi.fn()}
        onDecline={vi.fn()}
        onUndo={vi.fn()}
      />,
    );
    const expected =
      editor === "elementor"
        ? [
            [
              "Open in Elementor",
              "https://shop.example/wp-admin/post.php?post=418&action=elementor",
            ],
            ["Preview", "https://shop.example/?page_id=418&preview=true"],
          ]
        : [
            [
              "Edit the draft in WordPress",
              "https://shop.example/wp-admin/post.php?post=418&action=edit",
            ],
          ];
    for (const [name, href] of expected) {
      const link = screen.getByRole("link", { name });
      expect(link.getAttribute("href")).toBe(href);
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
  });
  it.each(["wpmgr/page-create", "wpmgr/page-edit", "wpmgr/rest-write"])(
    "shows %s without a broken card",
    (ability_name) => {
      const request = {
        id: "r1",
        site_id: "s1",
        ability_name,
        input_json: "{}",
        effect_copy: "draft",
        snapshot: "created_post_trash",
        site_label: "Shop",
        site_host: "shop.example",
        grant_label: "Laptop",
        grant_via: "token",
        card_copy_version: 1,
        state: "done",
        outcome: ability_name === "wpmgr/page-create" ? "created" : "applied",
        created_at: "2026-09-01T10:00:00Z",
        expires_at: "2026-09-01T11:00:00Z",
        content_cleared_at: "2026-10-13T10:00:00Z",
        undo_offered: false,
        resolve_gave_up: false,
        card_facts: null,
        page_edit: null,
        page_media: null,
      } satisfies AbilityRequest;
      render(
        <AbilityRequestCard
          request={request}
          onApprove={vi.fn()}
          onDecline={vi.fn()}
          onUndo={vi.fn()}
        />,
      );
      expect(
        screen.getByText(
          "Details cleared 15 days after this request finished.",
        ),
      ).toBeTruthy();
      expect(screen.getByText("Laptop")).toBeTruthy();
      expect(screen.getByText("shop.example")).toBeTruthy();
      expect(screen.getByText(/Sep 1|1 Sep|September 1/)).toBeTruthy();
      expect(screen.getByTestId("cleared-result").textContent).toMatch(
        /created|changed/i,
      );
      expect(
        screen.queryByText(
          /cannot be shown|can't be shown|not available in full/i,
        ),
      ).toBeNull();
      expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    },
  );
});
