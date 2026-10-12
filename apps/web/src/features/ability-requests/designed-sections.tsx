import { editDraftHref } from "./ability-card-model";
import {
  backgroundFraming,
  patternWords,
  sectionFacts,
  sectionTone,
  thumbSize,
  toneSwatch,
} from "./design-model";
import { OutlineNodeView } from "./layout-preview";
import { imageSizeLabel, type DesignedPagePreview, type PageMediaFact, type SectionNode } from "./outline-model";

// The sections of a designed page (outline version 3), in page order: the swatch
// of the tone each one is written in, its #anchor, its settings in words, a
// miniature of its background image, what it holds, and its patterns by name
// and variant. Everything is a React text node. The only values that reach a
// style are the tone's colour (a closed "#" and six hex digits, checked again
// by toneSwatch) and numbers the card works out. The miniature is drawn from
// the image's size, the overlay and the focal point; it never loads the site's
// file, because the site's description of the file carries no address and the
// card puts no site file on this screen. The site's file name is its word, and
// is shown as text in a bidi isolate.

export interface DesignedSectionsProps {
  preview: DesignedPagePreview;
  /** The site's host as WPMgr recorded it (site_host), to tell its own links from others. */
  siteHost: string;
  /** The site's own address, for "View image in WordPress". Unknown is fine. */
  siteUrl?: string | null;
  /** The id of the element that names this region. */
  labelledBy: string;
}

export function DesignedSections({ preview, siteHost, siteUrl, labelledBy }: DesignedSectionsProps) {
  return (
    // A scrolling region has to be reachable by keyboard to be scrolled by it.
    <div
      data-testid="design-sections"
      role="region"
      aria-labelledby={labelledBy}
      tabIndex={0}
      className="max-h-96 overflow-y-auto rounded-md bg-muted/30 p-3 text-sm text-foreground"
    >
      <ol aria-label="Page sections" className="space-y-3">
        {preview.sections.map((section, i) => (
          <SectionView key={i} section={section} preview={preview} siteHost={siteHost} siteUrl={siteUrl ?? null} />
        ))}
      </ol>
    </div>
  );
}

interface SectionViewProps {
  section: SectionNode;
  preview: DesignedPagePreview;
  siteHost: string;
  siteUrl: string | null;
}

function SectionView({ section, preview, siteHost, siteUrl }: SectionViewProps) {
  const swatch = toneSwatch(preview.design.colors, sectionTone(section));
  const fact = section.background === undefined ? undefined : preview.media.get(section.background.attachment_id);
  return (
    <li data-testid="design-section" className="space-y-2 rounded-md border border-border bg-card p-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span
          data-testid="section-tone-swatch"
          aria-hidden="true"
          className="size-5 shrink-0 rounded border border-border"
          style={swatch.chip === null ? undefined : { backgroundColor: swatch.chip }}
        />
        <span data-testid="section-tone" className="font-medium">
          {swatch.label}
        </span>
        <code data-testid="section-anchor" className="font-mono text-xs text-muted-foreground">
          {`#${section.anchor}`}
        </code>
      </div>
      <p data-testid="section-facts" className="text-xs text-muted-foreground">
        {sectionFacts(section).join(" · ")}
      </p>
      {section.background !== undefined && fact !== undefined ? (
        <BackgroundThumb
          attachmentId={section.background.attachment_id}
          framing={backgroundFraming(section.background)}
          fact={fact}
          ink={toneSwatch(preview.design.colors, "inverse").chip}
          siteUrl={siteUrl}
        />
      ) : null}
      <div className="space-y-2">
        {section.children.map((child, i) =>
          child.type === "pattern" ? (
            <p key={i}>
              <span className="text-xs font-medium text-muted-foreground">Pattern</span>{" "}
              <span data-testid="section-pattern" className="font-medium">
                {patternWords(child)}
              </span>
            </p>
          ) : (
            <OutlineNodeView key={i} node={child} media={preview.media} siteHost={siteHost} siteUrl={siteUrl} />
          ),
        )}
      </div>
    </li>
  );
}

interface BackgroundThumbProps {
  attachmentId: number;
  framing: ReturnType<typeof backgroundFraming>;
  fact: PageMediaFact;
  /** The design's ink, which the overlay is drawn in; null when it is not a closed shape. */
  ink: string | null;
  siteUrl: string | null;
}

function BackgroundThumb({ attachmentId, framing, fact, ink, siteUrl }: BackgroundThumbProps) {
  const size = thumbSize(fact.width, fact.height);
  const dimensions = imageSizeLabel(fact.width, fact.height);
  const href = editDraftHref(siteUrl, attachmentId);
  return (
    <figure data-testid="background-thumb" className="flex flex-wrap items-start gap-3">
      <div
        data-testid="thumb-tile"
        aria-hidden="true"
        className="relative shrink-0 overflow-hidden rounded border border-border bg-muted"
        style={{ width: `${size.width}px`, height: `${size.height}px` }}
      >
        {framing.opacity === null || ink === null ? null : (
          <span
            data-testid="thumb-overlay"
            className="absolute inset-0"
            style={{ backgroundColor: ink, opacity: framing.opacity }}
          />
        )}
        <span
          data-testid="thumb-focal"
          className="absolute left-1/2 size-2 -translate-x-1/2 -translate-y-1/2 rounded-full border border-background bg-primary"
          style={{ top: `${framing.focalTop}%` }}
        />
      </div>
      <figcaption className="min-w-0 space-y-0.5 text-xs text-muted-foreground">
        <p className="break-words">
          <span data-testid="thumb-file" className="font-medium text-foreground">
            <bdi>{fact.filename}</bdi>
          </span>
          {dimensions === null ? null : (
            <>
              {" · "}
              <span data-testid="thumb-size">{dimensions}</span>
            </>
          )}
        </p>
        <p data-testid="thumb-framing">{`${framing.overlayWords} · ${framing.focalWords}`}</p>
        {href ? (
          <p>
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              className="font-medium text-primary underline underline-offset-2 hover:opacity-80"
            >
              View image in WordPress{" "}
              <span className="sr-only">
                (<bdi>{fact.filename}</bdi>)
              </span>
            </a>
          </p>
        ) : null}
      </figcaption>
    </figure>
  );
}
