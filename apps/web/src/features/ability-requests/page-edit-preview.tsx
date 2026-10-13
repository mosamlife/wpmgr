import { useId, type ReactNode } from "react";

import { cn } from "@/lib/utils";

import { NativeOutlineNode, OutlineFragmentView, OutlineNodeView } from "./layout-preview";
import { DesignedSections } from "./designed-sections";
import { patternWords } from "./design-model";
import { PatternBody } from "./pattern-body";
import { type OutlineNode, type PageDesign } from "./outline-model";
import {
  EDIT_TOO_LARGE_COPY,
  editFieldWords,
  nativeWidgetWords,
  type AfterNode,
  type AfterView,
  type ChangeLine,
  type EditField,
  type Mark,
  type PageEditView,
  type Seg,
  type NativeValue,
} from "./page-edit-model";

// The changes of an AI page edit and the page after them, as text. Every
// string from the AI or the site is a React text node, isolated in a <bdi> so
// odd text direction cannot reorder what is around it: no innerHTML, no <img>
// of site content, no href built from a string. Our own words are the plain
// and the small muted labels. A site address inside the page (a button's link)
// is shown as text and is never a link.

export interface PageEditPreviewProps {
  view: PageEditView;
  /** The site's host as WPMgr recorded it (site_host). */
  siteHost: string;
  /** The site's own address, for "View image in WordPress". Unknown is fine. */
  siteUrl?: string | null;
  /** The id of the element that names the list of changes. */
  labelledBy: string;
}

export function PageEditPreview({ view, siteHost, siteUrl, labelledBy }: PageEditPreviewProps) {
  return (
    <div data-testid="page-edit-preview" className="rounded-md border border-border">
      <ol aria-labelledby={labelledBy} className="divide-y divide-border">
        {view.lines.map((line, i) => (
          <ChangeItem key={i} n={i + 1} line={line} design={view.design} siteHost={siteHost} siteUrl={siteUrl} />
        ))}
      </ol>
      <div className="space-y-2 border-t border-border p-3">
        <div className="flex flex-wrap items-baseline justify-between gap-x-3">
          <p id={`${labelledBy}-after`} className="text-xs font-medium text-muted-foreground">
            Page after the change
          </p>
          <FromTheSite />
        </div>
        {view.after ? (
          <AfterOutline after={view.after} labelledBy={`${labelledBy}-after`} />
        ) : (
          <p data-testid="after-omitted" className="text-sm text-muted-foreground">
            {EDIT_TOO_LARGE_COPY}
          </p>
        )}
      </div>
    </div>
  );
}

/** The existing "From the site" tag: the words beside it are the site's. */
function FromTheSite() {
  return <span className="text-xs font-medium text-muted-foreground">From the site</span>;
}

/** One of our words: small and muted. */
function Label({ children }: { children: ReactNode }) {
  return <span className="text-xs font-medium text-muted-foreground">{children}</span>;
}

/** The AI's or the site's words, isolated. */
function Txt({ children, className, testId }: { children: ReactNode; className?: string; testId?: string }) {
  return (
    <bdi className={className} data-testid={testId}>
      {children}
    </bdi>
  );
}

function Segments({ segs }: { segs: readonly Seg[] }) {
  return (
    <>
      {segs.map((seg, i) => (seg.site ? <Txt key={i}>{seg.s}</Txt> : <span key={i}>{seg.s}</span>))}
    </>
  );
}

function ChangeItem({
  n,
  line,
  siteHost,
  siteUrl,
  design,
}: {
  design: PageDesign | null;
  n: number;
  line: ChangeLine;
  siteHost: string;
  siteUrl?: string | null;
}) {
  const fragmentId = useId();
  return (
    <li data-testid="change-line" className="space-y-1.5 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3">
        <p className="min-w-0 break-words text-foreground">
          <span className="mr-2 text-xs font-medium tabular-nums text-muted-foreground">{n}</span>
          <span data-testid="change-title" className="font-medium">
            <Segments segs={line.title} />
          </span>
        </p>
        {line.fromSite ? <FromTheSite /> : null}
      </div>
      {line.native ? <NativeChange line={line} /> : null}
      {line.op === "set_text" ? (
        <div className="space-y-0.5 pl-6">
          <p className="break-words">
            <Label>was</Label> {'"'}
            <Txt testId="was">{line.was ?? ""}</Txt>
            {'"'}
          </p>
          <p className="break-words">
            <Label>now</Label> {'"'}
            <Txt testId="now">{line.now ?? ""}</Txt>
            {'"'}
          </p>
        </div>
      ) : null}
      {line.outline ? (
        <div className="pl-6">
          {design ? (
            <div data-testid="change-outline" role="region" aria-label={`Change ${n}: what is put on the page`}
              tabIndex={0} className="max-h-96 space-y-2 overflow-y-auto">
              <span id={fragmentId} className="sr-only">{`Change ${n}: what is put on the page`}</span>
              {line.outline.map((node, i) => node.type === "section" ? (
                <DesignedSections key={i} preview={{ design, sections: [node], media: null }}
                  siteHost={siteHost} siteUrl={siteUrl} labelledBy={fragmentId} />
              ) : node.type === "pattern" ? (
                <div key={i}>
                  <p className="font-medium">{patternWords(node)}</p>
                  <PatternBody pattern={node} media={null} siteHost={siteHost} siteUrl={siteUrl ?? null} />
                </div>
              ) : (
                <OutlineNodeView key={i} node={node} media={null} siteHost={siteHost} siteUrl={siteUrl} />
              ))}
            </div>
          ) : (
            <OutlineFragmentView nodes={line.outline as readonly OutlineNode[]} siteHost={siteHost}
              siteUrl={siteUrl} label={`Change ${n}: what is put on the page`} />
          )}
        </div>
      ) : null}
      {line.note ? <p className="pl-6 text-xs text-muted-foreground">{line.note}</p> : null}
    </li>
  );
}

function valueText(value: NativeValue): string {
  return typeof value === "string" ? (value === "" ? '""' : value) : JSON.stringify(value, null, 2);
}

function NativeChange({ line }: { line: ChangeLine }) {
  const native = line.native!;
  return <div className="space-y-2 pl-6">
    {native.widget && line.op !== "set_native" && line.op !== "add_native"
      ? <p>{`${nativeWidgetWords(native.widget)} widget`}</p> : null}
    {native.keys ? <div className="overflow-x-auto">
      <table aria-label="Widget settings" className="w-full table-fixed border-collapse text-left">
        <thead><tr>
          <th scope="col" className="p-2">Setting</th>
          <th scope="col" className="p-2">Before <FromTheSite /></th>
          <th scope="col" className="p-2">After <Label>Chosen by the AI</Label></th>
        </tr></thead>
        <tbody>{native.keys.map((key) => <tr key={key.key} className="border-t border-border">
          <th scope="row" className="break-words p-2 align-top font-medium">
            {key.key.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase())}
          </th>
          <td className="whitespace-pre-wrap break-words p-2 align-top">
            {key.from_the_site ? <Txt testId="native-before">{valueText(key.from_the_site.before)}</Txt> : <Label>Not set</Label>}
          </td>
          <td className="whitespace-pre-wrap break-words p-2 align-top">
            {key.removed ? <span>Removed</span> : <Txt>{valueText(key.after!)}</Txt>}
            {key.links ? <ul aria-label="Links" className="mt-1 space-y-1">
              {key.links.map((link, i) => <li key={i}>
                <Txt>{link.text}</Txt>{link.external_domain ? <Label>{` · Another website: ${link.external_domain}`}</Label> : null}
              </li>)}
            </ul> : null}
          </td>
        </tr>)}</tbody>
      </table>
    </div> : line.op === "set_native" || line.op === "add_native" ? <p>No settings change.</p> : null}
    {line.newRefs ? <p className="break-words"><Label>New element IDs: </Label><Txt>{line.newRefs.join(", ")}</Txt></p> : null}
    {native.removed_count !== undefined ? <>
      <p>{`${native.removed_count} ${native.removed_count === 1 ? "element" : "elements"} removed`}</p>
      <ol aria-label="Removed elements" className="space-y-1">
        {native.removed_outline!.map((node) => <li key={node.ref} className="break-words">
          {node.widget ? <NativeOutlineNode widget={node.widget} /> : <Label>{node.kind === "container" ? "Section" : nativeWidgetWords(node.kind)}</Label>}
          {" · "}<Txt>{node.ref}</Txt>
        </li>)}
      </ol>
    </> : null}
    {native.from ? <p className="break-words"><Label>From: </Label>
      <Txt>{native.from.parent === "root" ? "page root" : `parent ${native.from.parent}`}</Txt>{`, position ${native.from.index + 1}`}
    </p> : null}
    {native.to ? <p className="break-words"><Label>To: </Label>
      <Txt>{native.to.parent === "root" ? "page root" : `parent ${native.to.parent}`}</Txt>{`, position ${native.to.index + 1}`}
    </p> : null}
  </div>;
}

const MARK_TEXT: Record<Mark, string> = { changed: "changed", new: "new", moved: "moved" };
const MARK_CLASS: Record<Mark, string> = {
  changed: "bg-info-subtle text-info-subtle-fg",
  new: "bg-success-subtle text-success-subtle-fg",
  moved: "bg-muted text-muted-foreground",
};

function MarkChip({ mark }: { mark: Mark }) {
  return (
    <span
      data-mark={mark}
      className={cn("ml-1 inline-flex items-center rounded px-1.5 py-0.5 text-xs font-medium", MARK_CLASS[mark])}
    >
      {MARK_TEXT[mark]}
    </span>
  );
}

const CONTAINER_KINDS: ReadonlySet<string> = new Set(["group", "columns", "section", "column"]);

const FIELD_WORDS: Record<EditField, string> = {
  text: "",
  caption: "caption",
  url: "links to",
  alt: "alt text",
};

function AfterOutline({ after, labelledBy }: { after: AfterView; labelledBy: string }) {
  return (
    <div
      data-testid="page-after"
      role="region"
      aria-labelledby={labelledBy}
      tabIndex={0}
      className="max-h-96 space-y-2 overflow-y-auto rounded-md bg-muted/30 p-3 text-sm text-foreground"
    >
      <AfterNodes nodes={after.nodes} />
      {after.truncated ? (
        <p data-testid="after-truncated" className="text-xs text-muted-foreground">
          {`Only the first ${countNodes(after.nodes)} of ${after.nodeCount} parts of the page are listed.`}
        </p>
      ) : null}
    </div>
  );
}

function countNodes(nodes: readonly AfterNode[]): number {
  return nodes.reduce((sum, n) => sum + 1 + countNodes(n.children), 0);
}

function AfterNodes({ nodes }: { nodes: readonly AfterNode[] }) {
  return (
    <div className="space-y-2">
      {nodes.map((node) => (
        <AfterNodeView key={node.ref} node={node} />
      ))}
    </div>
  );
}

function AfterNodeView({ node }: { node: AfterNode }) {
  if (CONTAINER_KINDS.has(node.kind)) {
    return (
      <div role="group" aria-label={node.label} data-ref={node.ref}>
        <p>
          <Label>{node.label}</Label>
          {node.mark ? <MarkChip mark={node.mark} /> : null}
        </p>
        {node.children.length > 0 ? (
          <div className="ml-1 mt-1 space-y-2 border-l border-border pl-3">
            <AfterNodes nodes={node.children} />
          </div>
        ) : null}
      </div>
    );
  }
  return (
    <div data-ref={node.ref}>
      <p className="break-words">
        {node.kind === "native" ? <NativeOutlineNode widget={node.widget!} summary={node.summary} /> : <Label>{node.label}</Label>}
        {node.kind === "locked" ? <span className="text-muted-foreground"> (WPMgr does not edit this)</span> : null}
        {node.texts.map((t) => (
          <span key={t.field}>
            {FIELD_WORDS[t.field] === "" ? " " : <Label>{` ${FIELD_WORDS[t.field] ?? editFieldWords(t.field)}`}</Label>}
            {" "}
            {'"'}
            <Txt>{t.text}</Txt>
            {'"'}
          </span>
        ))}
        {node.mark ? <MarkChip mark={node.mark} /> : null}
      </p>
      {node.children.length > 0 ? (
        <div className="ml-1 mt-1 space-y-2 border-l border-border pl-3">
          <AfterNodes nodes={node.children} />
        </div>
      ) : null}
    </div>
  );
}
