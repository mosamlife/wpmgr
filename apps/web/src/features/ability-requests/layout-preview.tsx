import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

import { editDraftHref } from "./ability-card-model";
import {
  buttonLabel,
  buttonsLabel,
  classifyLink,
  columnsLabel,
  headingLabel,
  imageAlignLabel,
  imageAltLabel,
  imageSizeLabel,
  quoted,
  spacerLabel,
  tableLabel,
  type ImageNode,
  type LinkTarget,
  type OutlineNode,
  type PagePreview,
} from "./outline-model";

// The readable outline of an AI page request (design: the page is shown as an
// outline, never as a rendered page). Everything here is a React text node:
// no innerHTML, no <img> of a site file, and no href built from an AI string.
// The one link is to the image's edit screen in wp-admin, built from the
// site's own address and an integer id by editDraftHref.
//
// Our words are the small muted labels; the AI's words are the plain text
// beside them; the site's facts (file name, size) are muted text.

export interface LayoutPreviewProps {
  preview: PagePreview;
  /** The site's host as WPMgr recorded it (site_host), to tell its own links from others. */
  siteHost: string;
  /** The site's own address, for "View image in WordPress". Unknown is fine. */
  siteUrl?: string | null;
  /** The id of the element that names this region. */
  labelledBy: string;
  /**
   * True on a page Elementor builds, where an image shows the alt text saved
   * with it in the media library: an alt that differs from the library's is
   * refused before any card exists, so the outline's alt is the library's and
   * each alt line says so. Absent or false, the lines are the block editor's.
   */
  altFromLibrary?: boolean;
}

interface Ctx {
  readonly preview: PagePreview;
  readonly siteHost: string;
  readonly siteUrl: string | null;
  readonly altFromLibrary: boolean;
}

export function LayoutPreview({ preview, siteHost, siteUrl, labelledBy, altFromLibrary = false }: LayoutPreviewProps) {
  const ctx: Ctx = { preview, siteHost, siteUrl: siteUrl ?? null, altFromLibrary };
  return (
    // A scrolling region has to be reachable by keyboard to be scrolled by it.
    <div
      data-testid="ability-outline"
      role="region"
      aria-labelledby={labelledBy}
      tabIndex={0}
      className="max-h-96 space-y-2 overflow-y-auto rounded-md bg-muted/30 p-3 text-sm text-foreground"
    >
      <p className="break-words font-semibold">
        <Txt>{preview.title}</Txt>
      </p>
      <Nodes nodes={preview.outline} ctx={ctx} />
    </div>
  );
}

/** The AI's or the site's words, isolated so odd text direction cannot reorder what is around it. */
function Txt({ children, className }: { children: ReactNode; className?: string }) {
  return <bdi className={className}>{children}</bdi>;
}

/** One of our words: small and muted. */
function Label({ children }: { children: ReactNode }) {
  return <span className="text-xs font-medium text-muted-foreground">{children}</span>;
}

function Nodes({ nodes, ctx }: { nodes: readonly OutlineNode[]; ctx: Ctx }) {
  return (
    <div className="space-y-2">
      {nodes.map((node, i) => (
        <NodeView key={i} node={node} ctx={ctx} />
      ))}
    </div>
  );
}

/** A labelled container whose contents are indented one step. */
function Container({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div role="group" aria-label={label}>
      <p>
        <Label>{label}</Label>
      </p>
      <div className="ml-1 mt-1 space-y-2 border-l border-border pl-3">{children}</div>
    </div>
  );
}

function NodeView({ node, ctx }: { node: OutlineNode; ctx: Ctx }) {
  switch (node.type) {
    case "heading":
      return (
        <p className="break-words">
          <Label>{headingLabel(node.level)}</Label>{" "}
          <Txt className="font-semibold">{node.text}</Txt>
        </p>
      );
    case "paragraph":
      return (
        <p className="break-words">
          <Label>Paragraph</Label> <Txt>{node.text}</Txt>
        </p>
      );
    case "list": {
      const Tag = node.ordered ? "ol" : "ul";
      return (
        <div>
          <p>
            <Label>{node.ordered ? "Numbered list" : "List"}</Label>
          </p>
          <Tag className={cn("mt-0.5 space-y-0.5 pl-5", node.ordered ? "list-decimal" : "list-disc")}>
            {node.items.map((item, i) => (
              <li key={i} className="break-words">
                <Txt>{item}</Txt>
              </li>
            ))}
          </Tag>
        </div>
      );
    }
    case "group":
      return (
        <Container label="Section">
          <Nodes nodes={node.children} ctx={ctx} />
        </Container>
      );
    case "columns":
      return (
        <Container label={columnsLabel(node.columns.length, node.widths)}>
          {node.columns.map((column, i) => (
            <Container key={i} label={`Column ${i + 1}`}>
              <Nodes nodes={column.children} ctx={ctx} />
            </Container>
          ))}
        </Container>
      );
    case "image":
      return <ImageView node={node} ctx={ctx} />;
    case "buttons":
      return (
        <Container label={buttonsLabel(node.align)}>
          {node.buttons.map((button, i) => (
            <div key={i} className="space-y-0.5">
              <p className="break-words">
                <Label>{buttonLabel(button.style)}</Label> <Txt>{quoted(button.text)}</Txt>
              </p>
              <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-muted-foreground">
                {/* The address is shown as text and is never a link. It is ASCII by the
                    grammar, so it needs no isolation. */}
                <span className="break-all">{`Links to ${button.url}`}</span>{" "}
                <LinkChip target={classifyLink(button.url, ctx.siteHost)} />
              </p>
            </div>
          ))}
        </Container>
      );
    case "quote":
      return (
        <div>
          <p>
            <Label>Quote</Label>
          </p>
          <div className="ml-1 mt-1 space-y-1 border-l border-border pl-3">
            {node.paragraphs.map((paragraph, i) => (
              <p key={i} className="break-words italic">
                <Txt>{paragraph}</Txt>
              </p>
            ))}
            {node.citation !== undefined ? (
              <p className="break-words">
                <Label>Citation:</Label> <Txt>{node.citation}</Txt>
              </p>
            ) : null}
          </div>
        </div>
      );
    case "separator":
      return (
        <p>
          <Label>Separator</Label>
        </p>
      );
    case "spacer":
      return (
        <p>
          <Label>{spacerLabel(node.size)}</Label>
        </p>
      );
    case "table":
      return <TableView node={node} />;
  }
}

function ImageView({ node, ctx }: { node: ImageNode; ctx: Ctx }) {
  // parsePagePreview refuses an outline with an image that has no fact, so a
  // missing fact is not reachable; the guard keeps the type honest.
  const fact = ctx.preview.media.get(node.attachment_id);
  if (fact === undefined) return null;
  const size = imageSizeLabel(fact.width, fact.height);
  const align = imageAlignLabel(node.align);
  const href = editDraftHref(ctx.siteUrl, fact.id);
  return (
    <div className="space-y-0.5">
      <p className="break-words">
        <Label>Image</Label>
        <span className="text-muted-foreground">
          {" · "}
          <Txt>{fact.filename}</Txt>
          {size ? ` · ${size}` : null}
        </span>
      </p>
      <div className="space-y-0.5 pl-3">
        <p className="break-words">
          <Label>{imageAltLabel(node.alt, ctx.altFromLibrary)}</Label>
          {node.alt === "" ? null : (
            <>
              {" "}
              <Txt>{quoted(node.alt)}</Txt>
            </>
          )}
        </p>
        {node.caption !== undefined ? (
          <p className="break-words">
            <Label>Caption:</Label> <Txt>{quoted(node.caption)}</Txt>
          </p>
        ) : null}
        {align ? (
          <p>
            <Label>{align}</Label>
          </p>
        ) : null}
        {href ? (
          <p>
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              className="font-medium text-primary underline underline-offset-2 hover:opacity-80"
            >
              View image in WordPress{" "}
              <span className="sr-only">{`(${fact.filename})`}</span>
            </a>
          </p>
        ) : null}
      </div>
    </div>
  );
}

function LinkChip({ target }: { target: LinkTarget | null }) {
  // A null target cannot reach here (the grammar refuses the link first).
  if (target === null) return null;
  if (target.kind === "site") {
    return (
      <span
        data-chip="site"
        className="inline-flex items-center rounded bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground"
      >
        This site
      </span>
    );
  }
  return (
    <span
      data-chip="external"
      className="inline-flex items-center gap-1.5 rounded bg-warning-subtle px-2 py-0.5 text-xs font-medium text-warning-subtle-fg"
    >
      <span aria-hidden="true" className="size-1.5 shrink-0 rounded-full bg-warning" />
      {`Another website: ${target.host}`}
    </span>
  );
}

function TableView({ node }: { node: Extract<OutlineNode, { type: "table" }> }) {
  const columns = node.header?.length ?? node.rows[0]?.length ?? 0;
  const label = tableLabel(columns, node.rows.length, node.header !== undefined);
  return (
    <div>
      <p>
        <Label>{label}</Label>
      </p>
      <div className="ml-1 mt-1 overflow-x-auto border-l border-border pl-3">
        <table aria-label={label} className="w-full table-fixed border-collapse text-left">
          {node.header ? (
            <thead>
              <tr>
                {node.header.map((cell, i) => (
                  <th key={i} scope="col" className={cellClass(i, "font-semibold")}>
                    <Cell>{cell}</Cell>
                  </th>
                ))}
              </tr>
            </thead>
          ) : null}
          <tbody>
            {node.rows.map((row, r) => (
              <tr key={r} className="border-t border-border">
                {row.map((cell, i) => (
                  <td key={i} className={cellClass(i)}>
                    <Cell>{cell}</Cell>
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** A divider between cells, so a row still reads as cells in a line. */
function cellClass(index: number, extra?: string): string {
  return cn("break-words px-2 py-1 align-top", index > 0 && "border-l border-border", extra);
}

function Cell({ children }: { children: string }) {
  return children === "" ? <span className="italic text-muted-foreground">(empty)</span> : <Txt>{children}</Txt>;
}
