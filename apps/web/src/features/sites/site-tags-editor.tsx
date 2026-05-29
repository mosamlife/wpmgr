import { useState } from "react";
import { X, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { FieldError } from "@/components/forms/field-error";
import { StickySaveBar } from "@/components/forms/sticky-save-bar";
import { useSetSiteTags } from "@/features/sites/use-sites";
import type { Site } from "@wpmgr/api";

// Inline editor for a site's tag set. Local draft state mirrors the site's
// tags; the global StickySaveBar surfaces when the draft diverges from the
// server set. Adds are de-duped and capped at 64 chars to match the API
// contract.
//
// Sprint 4 (forms): removed the inline "Save tags" button. The shared
// StickySaveBar handles save + discard at the viewport bottom for parity
// with every other editable settings surface.
export function SiteTagsEditor({ site }: { site: Site }) {
  const mutation = useSetSiteTags();
  const [draft, setDraft] = useState<string[]>(site.tags);
  const [input, setInput] = useState("");
  const [inputError, setInputError] = useState<string | null>(null);

  // React's "reset state from props during render" pattern: when the server's
  // tag set changes (e.g. after a successful save reconciles, or another tab
  // updates), re-seed the local draft. `serverKey` is a stable join of the
  // tags so we only reset on actual changes.
  const serverKey = site.tags.join(" ");
  const [lastServerKey, setLastServerKey] = useState(serverKey);
  if (serverKey !== lastServerKey) {
    setLastServerKey(serverKey);
    setDraft(site.tags);
  }

  const dirty =
    draft.length !== site.tags.length ||
    draft.some((t, i) => t !== site.tags[i]);

  function addTag() {
    const tag = input.trim();
    if (!tag) {
      setInputError(null);
      return;
    }
    if (tag.length > 64) {
      setInputError("Tag too long");
      return;
    }
    if (draft.includes(tag)) {
      setInputError("Duplicate tag");
      return;
    }
    setDraft([...draft, tag]);
    setInput("");
    setInputError(null);
  }

  function removeTag(tag: string) {
    setDraft(draft.filter((t) => t !== tag));
  }

  function save() {
    mutation.mutate({ siteId: site.id, tags: draft });
  }

  function discard() {
    setDraft(site.tags);
    setInput("");
    setInputError(null);
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-1">
        {draft.length === 0 ? (
          <span className="text-sm text-muted-foreground">No tags</span>
        ) : (
          draft.map((tag) => (
            <Badge key={tag} variant="outline" className="pr-1">
              {tag}
              <button
                type="button"
                onClick={() => removeTag(tag)}
                aria-label={`Remove tag ${tag}`}
                className="ml-0.5 rounded-full p-0.5 hover:bg-[var(--color-accent)]"
              >
                <X aria-hidden="true" className="size-3" />
              </button>
            </Badge>
          ))
        )}
      </div>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          addTag();
        }}
        className="flex flex-wrap items-end gap-2"
      >
        <div className="space-y-1">
          <Label htmlFor="new-tag">Add tag</Label>
          <Input
            id="new-tag"
            value={input}
            onChange={(e) => {
              setInput(e.target.value);
              if (inputError) setInputError(null);
            }}
            onBlur={() => {
              const tag = input.trim();
              if (tag && tag.length > 64) setInputError("Tag too long");
            }}
            maxLength={64}
            placeholder="e.g. production"
            className="w-48"
            aria-invalid={inputError ? "true" : undefined}
            aria-describedby="new-tag-help"
          />
          <p id="new-tag-help" className="text-sm text-muted-foreground">
            Up to 64 characters. Tags are unique per site.
          </p>
          <FieldError
            what={inputError ?? undefined}
            why={
              inputError === "Tag too long"
                ? "Tags are capped at 64 characters."
                : inputError === "Duplicate tag"
                  ? "This tag is already on the site."
                  : undefined
            }
            how={
              inputError === "Tag too long"
                ? "Shorten the tag above."
                : inputError === "Duplicate tag"
                  ? "Pick a different name."
                  : undefined
            }
          />
        </div>
        <Button type="submit" variant="outline" size="sm" disabled={!input.trim()}>
          <Plus aria-hidden="true" />
          Add tag
        </Button>
      </form>

      <StickySaveBar
        isDirty={dirty}
        isPending={mutation.isPending}
        errorMessage={mutation.isError ? mutation.error.message : null}
        onSave={save}
        onDiscard={discard}
        saveLabel="Save tags"
        discardLabel="Discard changes"
      />
    </div>
  );
}
