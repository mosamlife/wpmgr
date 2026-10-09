import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mediaFact } from "@/test/ability-request-rows";

import { LayoutPreview, type LayoutPreviewProps } from "./layout-preview";
import { parsePagePreview } from "./outline-model";

// The alt line of an image row is the block editor's unless the caller says the
// page is built by Elementor, whose images show the media library's alt text.
// The card is the only caller today and always says which; this holds the
// optional prop to its default for the next caller.

const input = JSON.stringify({
  post_type: "page",
  editor: "wordpress_blocks",
  title: "Spring menu",
  outline: [
    { type: "image", attachment_id: 5, alt: "A van" },
    { type: "image", attachment_id: 7, alt: "" },
  ],
});

const parsed = parsePagePreview(input, [mediaFact(5), mediaFact(7)]);
if (parsed === null) throw new Error("the fixture outline is not showable");

const base: LayoutPreviewProps = {
  preview: parsed,
  siteHost: "example.com",
  siteUrl: "https://example.com",
  labelledBy: "outline-label",
};

/** The alt line of each image row, in document order. */
function altLines(): string[] {
  const outline = screen.getByTestId("ability-outline");
  return Array.from(outline.querySelectorAll("p"), (p) => p.textContent ?? "").filter((t) => /^(No )?alt text/i.test(t));
}

function renderPreview(props: LayoutPreviewProps) {
  return renderWithProviders(
    <>
      <p id="outline-label">Chosen by the AI</p>
      <LayoutPreview {...props} />
    </>,
  );
}

describe("LayoutPreview alt lines", () => {
  it.each<[string, LayoutPreviewProps]>([
    ["is left out", base],
    ["is false", { ...base, altFromLibrary: false }],
  ])("are the block editor's when altFromLibrary %s", (_name, props) => {
    renderPreview(props);
    expect(altLines()).toEqual(['Alt text: "A van"', "No alt text (decorative)"]);
    expect(screen.getByTestId("ability-outline")).not.toHaveTextContent("media library");
  });

  it("say they come from the media library when altFromLibrary is true", () => {
    renderPreview({ ...base, altFromLibrary: true });
    expect(altLines()).toEqual(['Alt text (from the media library): "A van"', "No alt text in the media library"]);
  });
});
