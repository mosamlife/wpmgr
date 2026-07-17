import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Check, MoreHorizontal, Palette, Tag as TagIcon } from "lucide-react";
import type { SiteTag } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { PageHeader } from "@/components/shared/page-header";
import { PageError } from "@/components/feedback";
import { TagChip } from "@/features/sites/tag-chip";
import { toast } from "@/components/toast";
import { cn, relativeTime } from "@/lib/utils";
import { TAG_SWATCHES, resolveTagDot } from "@/lib/tag-color";
import {
  useTags,
  useCreateTag,
  useUpdateTag,
  useDeleteTag,
  TagNameExistsError,
} from "@/features/tags/use-tags";

// GH #230 "rich tags" — the tag registry management page. Every tag in the
// tenant's registry, with rename/recolor/merge/delete row actions and a
// "New tag" dialog. Reached from the sidebar (Settings > Tags) and from the
// Sites page's Tags filter dropdown ("Manage tags" footer link).

export const Route = createFileRoute("/_authed/settings/tags")({
  component: TagsSettingsPage,
});

function TagsSettingsPage() {
  const { data: tags, isPending, isError, error, refetch, isRefetching } = useTags();
  const updateTag = useUpdateTag();
  const deleteTag = useDeleteTag();

  const [createOpen, setCreateOpen] = useState(false);

  // ── Inline rename ──────────────────────────────────────────────────────
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const [renameConflict, setRenameConflict] = useState<{
    tag: SiteTag;
    newName: string;
  } | null>(null);

  function startRename(tag: SiteTag) {
    setRenamingId(tag.id);
    setRenameValue(tag.name);
    setRenameConflict(null);
  }

  function cancelRename() {
    setRenamingId(null);
    setRenameValue("");
  }

  async function commitRename(tag: SiteTag) {
    const next = renameValue.trim();
    if (!next || next === tag.name) {
      cancelRename();
      return;
    }
    try {
      await updateTag.mutateAsync({ tagId: tag.id, body: { name: next } });
      cancelRename();
    } catch (err) {
      if (err instanceof TagNameExistsError) {
        setRenameConflict({ tag, newName: next });
      } else {
        toast.error("Could not rename tag", {
          description: err instanceof Error ? err.message : undefined,
        });
      }
    }
  }

  async function confirmMergeFromRenameConflict() {
    if (!renameConflict) return;
    const { tag, newName } = renameConflict;
    try {
      await updateTag.mutateAsync({
        tagId: tag.id,
        body: { name: newName, merge: true },
      });
      toast.success(`Merged "${tag.name}" into "${newName}"`);
      setRenameConflict(null);
      cancelRename();
    } catch (err) {
      toast.error("Could not merge tags", {
        description: err instanceof Error ? err.message : undefined,
      });
    }
  }

  // ── Change color ───────────────────────────────────────────────────────
  async function changeColor(tag: SiteTag, color: string) {
    if (tag.color === color) return;
    try {
      await updateTag.mutateAsync({ tagId: tag.id, body: { color } });
    } catch (err) {
      toast.error(`Could not recolor "${tag.name}"`, {
        description: err instanceof Error ? err.message : undefined,
      });
    }
  }

  // ── Merge (row action) ────────────────────────────────────────────────
  const [mergeSource, setMergeSource] = useState<SiteTag | null>(null);

  // ── Delete ─────────────────────────────────────────────────────────────
  const [deleteTarget, setDeleteTarget] = useState<SiteTag | null>(null);

  async function confirmDelete() {
    if (!deleteTarget) return;
    try {
      await deleteTag.mutateAsync(deleteTarget.id);
      toast.success(`Deleted "${deleteTarget.name}"`);
      setDeleteTarget(null);
    } catch (err) {
      toast.error(`Could not delete "${deleteTarget.name}"`, {
        description: err instanceof Error ? err.message : undefined,
      });
    }
  }

  return (
    <section aria-labelledby="tags-heading" className="space-y-6">
      <PageHeader
        title="Tags"
        subline="The tenant's tag registry. Tags added to sites appear here automatically."
        actions={
          <Button type="button" size="sm" onClick={() => setCreateOpen(true)}>
            New tag
          </Button>
        }
      />

      {isPending ? (
        <p role="status" className="text-muted-foreground">
          Loading tags…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load tags."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload tags"
          isRetrying={isRefetching}
        />
      ) : tags.length === 0 ? (
        <div
          role="status"
          aria-label="No tags"
          className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-border py-12 text-center"
        >
          <TagIcon aria-hidden="true" strokeWidth={1.5} className="size-8 text-muted-foreground/50" />
          <div className="space-y-1">
            <p className="text-balance text-sm font-medium text-foreground">No tags yet.</p>
            <p className="text-balance text-sm text-muted-foreground">
              Tags added to sites appear here automatically.
            </p>
          </div>
        </div>
      ) : (
        <div className="rounded-xl border border-border">
          <Table>
            <caption className="sr-only">List of tags</caption>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <span className="sr-only">Preview</span>
                </TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Color</TableHead>
                <TableHead>Usage</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="sr-only">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tags.map((tag) => (
                <TableRow key={tag.id}>
                  <TableCell>
                    <TagChip tag={tag} />
                  </TableCell>
                  <TableCell>
                    {renamingId === tag.id ? (
                      <Input
                        autoFocus
                        value={renameValue}
                        onChange={(e) => setRenameValue(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") {
                            e.preventDefault();
                            void commitRename(tag);
                          } else if (e.key === "Escape") {
                            e.preventDefault();
                            cancelRename();
                          }
                        }}
                        onBlur={() => void commitRename(tag)}
                        maxLength={64}
                        aria-label={`Rename tag ${tag.name}`}
                        className="h-8 w-48"
                      />
                    ) : (
                      <span className="font-medium text-foreground">{tag.name}</span>
                    )}
                    {renameConflict?.tag.id === tag.id ? (
                      <p role="alert" className="mt-1 flex flex-wrap items-center gap-2 text-xs text-destructive">
                        <span>
                          &quot;{renameConflict.newName}&quot; already exists. Merge &quot;
                          {renameConflict.tag.name}&quot; into &quot;{renameConflict.newName}&quot;?
                        </span>
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          className="h-6 px-2 text-xs"
                          onClick={() => void confirmMergeFromRenameConflict()}
                        >
                          Merge
                        </Button>
                      </p>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <ColorSwatchTrigger
                      tag={tag}
                      onChange={(color) => void changeColor(tag, color)}
                    />
                  </TableCell>
                  <TableCell>
                    <Link
                      to="/sites"
                      search={{ tags: [tag.name] }}
                      className="text-foreground underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    >
                      {tag.usage_count} {tag.usage_count === 1 ? "site" : "sites"}
                    </Link>
                  </TableCell>
                  <TableCell
                    className="text-muted-foreground"
                    title={new Date(tag.created_at).toLocaleString()}
                  >
                    {relativeTime(tag.created_at) ?? tag.created_at}
                  </TableCell>
                  <TableCell className="text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          aria-label={`Actions for tag ${tag.name}`}
                        >
                          <MoreHorizontal aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onSelect={() => startRename(tag)}>
                          Rename
                        </DropdownMenuItem>
                        <DropdownMenuItem onSelect={() => setMergeSource(tag)}>
                          Merge into…
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          onSelect={() => setDeleteTarget(tag)}
                          className="text-destructive focus:bg-destructive/10 focus:text-destructive"
                        >
                          Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <NewTagDialog open={createOpen} onClose={() => setCreateOpen(false)} />

      <MergeDialog
        source={mergeSource}
        tags={tags ?? []}
        onClose={() => setMergeSource(null)}
      />

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete &quot;{deleteTarget?.name}&quot;?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && deleteTarget.usage_count > 0
                ? `It will be removed from ${deleteTarget.usage_count} site(s). This cannot be undone.`
                : "It is not used by any site. This cannot be undone."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setDeleteTarget(null)} disabled={deleteTag.isPending} />
            <AlertDialogAction
              variant="destructive"
              disabled={deleteTag.isPending}
              onClick={() => void confirmDelete()}
            >
              Delete tag
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}

// ---------------------------------------------------------------------------
// ColorSwatchTrigger — the per-row "Color" cell: swatch button + popover
// ---------------------------------------------------------------------------

function ColorSwatchTrigger({
  tag,
  onChange,
}: {
  tag: SiteTag;
  onChange: (color: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const dot = resolveTagDot(tag);

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={`Change color for tag ${tag.name}`}
          className="inline-flex items-center gap-1.5 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <span
            aria-hidden="true"
            className={cn("size-3 shrink-0 rounded-full", dot.className)}
            style={dot.style}
          />
          {tag.color ? tag.color : "Auto"}
          <Palette aria-hidden="true" className="size-3" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-56 p-3">
        <ColorSwatchPicker
          value={tag.color}
          onChange={(color) => {
            onChange(color);
            setOpen(false);
          }}
        />
      </PopoverContent>
    </Popover>
  );
}

// ---------------------------------------------------------------------------
// ColorSwatchPicker — 12 swatches + "Auto", shared by the row popover and
// the New tag dialog
// ---------------------------------------------------------------------------

function ColorSwatchPicker({
  value,
  onChange,
}: {
  value: string;
  onChange: (color: string) => void;
}) {
  const normalized = value.toLowerCase();
  return (
    <div role="radiogroup" aria-label="Tag color" className="space-y-2">
      <button
        type="button"
        role="radio"
        aria-checked={value === ""}
        onClick={() => onChange("")}
        className={cn(
          "flex w-full items-center justify-center gap-1.5 rounded-md border px-2 py-1.5 text-xs font-medium",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          value === "" ? "border-primary bg-primary/5 text-foreground" : "border-border text-muted-foreground hover:bg-accent",
        )}
      >
        {value === "" ? <Check aria-hidden="true" className="size-3.5" /> : null}
        Auto
      </button>
      <div className="grid grid-cols-6 gap-1.5">
        {TAG_SWATCHES.map(({ hue, hex }) => {
          const active = normalized === hex.toLowerCase();
          return (
            <button
              key={hue}
              type="button"
              role="radio"
              aria-checked={active}
              aria-label={hue}
              title={hue}
              onClick={() => onChange(hex)}
              style={{ backgroundColor: hex }}
              className={cn(
                "relative flex size-7 items-center justify-center rounded-full border-2",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                active ? "border-foreground" : "border-transparent",
              )}
            >
              {active ? (
                <Check aria-hidden="true" className="size-3.5 text-white drop-shadow" />
              ) : null}
            </button>
          );
        })}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// NewTagDialog
// ---------------------------------------------------------------------------

function NewTagDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const createTag = useCreateTag();
  const [name, setName] = useState("");
  const [color, setColor] = useState("");
  const [error, setError] = useState<string | null>(null);

  // Reset the draft whenever the dialog transitions to open — the "adjust
  // state during render" pattern (see DestructiveConfirm's `prevOpen`
  // reset), not an effect: this codebase's stricter React Compiler lint
  // config flags a bare setState-in-effect as a cascading-render smell.
  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) {
      setName("");
      setColor("");
      setError(null);
    }
  }

  async function handleCreate() {
    const trimmed = name.trim();
    if (!trimmed) return;
    setError(null);
    try {
      await createTag.mutateAsync({ name: trimmed, color: color || undefined });
      onClose();
    } catch (err) {
      if (err instanceof TagNameExistsError) {
        setError("A tag with this name already exists.");
      } else {
        setError(err instanceof Error ? err.message : "Could not create tag.");
      }
    }
  }

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy="new-tag-title">
        <DialogHeader>
          <DialogTitle id="new-tag-title">New tag</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="new-tag-name">Name</Label>
            <Input
              id="new-tag-name"
              autoFocus
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (error) setError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void handleCreate();
                }
              }}
              maxLength={64}
              aria-invalid={error ? true : undefined}
            />
            {error ? (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
          </div>
          <div className="space-y-1.5">
            <Label>Color</Label>
            <ColorSwatchPicker value={color} onChange={setColor} />
          </div>
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="button"
            disabled={!name.trim() || createTag.isPending}
            onClick={() => void handleCreate()}
          >
            Create tag
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// MergeDialog — row action "Merge into…"
// ---------------------------------------------------------------------------

function MergeDialog({
  source,
  tags,
  onClose,
}: {
  source: SiteTag | null;
  tags: readonly SiteTag[];
  onClose: () => void;
}) {
  const updateTag = useUpdateTag();
  const [targetId, setTargetId] = useState<string | null>(null);

  // Reset the chosen target whenever a different tag is opened for merge —
  // same render-time-adjustment idiom as NewTagDialog above, not an effect.
  const [prevSource, setPrevSource] = useState(source);
  if (source !== prevSource) {
    setPrevSource(source);
    setTargetId(null);
  }

  const candidates = tags.filter((t) => t.id !== source?.id);
  const target = candidates.find((t) => t.id === targetId) ?? null;

  async function confirm() {
    if (!source || !target) return;
    try {
      await updateTag.mutateAsync({
        tagId: source.id,
        body: { name: target.name, merge: true },
      });
      toast.success(`Merged "${source.name}" into "${target.name}"`);
      onClose();
    } catch (err) {
      toast.error("Could not merge tags", {
        description: err instanceof Error ? err.message : undefined,
      });
    }
  }

  return (
    <Dialog open={source !== null} onClose={onClose}>
      <DialogContent ariaLabelledBy="merge-tag-title">
        <DialogHeader>
          <DialogTitle id="merge-tag-title">Merge &quot;{source?.name}&quot;</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-3">
          {candidates.length === 0 ? (
            <p className="text-sm text-muted-foreground">No other tags to merge into.</p>
          ) : (
            <div role="radiogroup" aria-label="Merge target" className="max-h-64 space-y-1 overflow-y-auto">
              {candidates.map((t) => (
                <button
                  key={t.id}
                  type="button"
                  role="radio"
                  aria-checked={targetId === t.id}
                  onClick={() => setTargetId(t.id)}
                  className={cn(
                    "flex w-full items-center justify-between rounded-md border px-3 py-2 text-sm transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    targetId === t.id ? "border-primary bg-primary/5" : "border-border hover:bg-accent",
                  )}
                >
                  <TagChip tag={t} />
                  <span className="text-xs text-muted-foreground">
                    {t.usage_count} {t.usage_count === 1 ? "site" : "sites"}
                  </span>
                </button>
              ))}
            </div>
          )}

          {source && target ? (
            <p role="alert" className="text-sm text-foreground">
              Merge &quot;{source.name}&quot; ({source.usage_count} sites) into &quot;
              {target.name}&quot; ({target.usage_count} sites)? Sites tagged &quot;
              {source.name}&quot; will be tagged &quot;{target.name}&quot; instead. This cannot
              be undone.
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            disabled={!target || updateTag.isPending}
            onClick={() => void confirm()}
          >
            Merge tags
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
