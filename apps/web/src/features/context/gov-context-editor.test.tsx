import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import type { GovContext } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { GovContextEditor } from "./gov-context-editor";
import {
  ContextSecretDetectedError,
  ContextVersionConflictError,
  ContextWidenForbiddenError,
} from "./use-context";

// ADR-064 S5 Stage B — outcome tests for the three write-path refusals
// (Decision 4/10/13) the org/site editors share via `GovContextEditor`.
// `GovContextEditor` takes `saveError` as a prop (it does not call any data
// hook itself), so these tests exercise it directly rather than mocking a
// hook module — the same "test the presentational component against real
// prop shapes" approach as health-cards.test.tsx.

function buildContext(overrides: Partial<GovContext> = {}): GovContext {
  return {
    version: 3,
    restrictions: { forbidden_tools: ["shell_exec"], forbidden_domains: [], forbidden_topics: [] },
    guidance: { brand_voice: "", audience: "", terminology: "", style: "" },
    author_type: "user",
    provenance: "manual",
    created_at: "2026-08-01T00:00:00Z",
    ...overrides,
  };
}

describe("GovContextEditor — widen refusal names which restriction was hit", () => {
  it("flags the specific restriction field, not just a generic banner", () => {
    const message =
      "this write would remove [shell_exec] from forbidden_tools, which was set by organisation default (layer 2) — a lower layer may narrow or add to a restriction but never remove what a higher layer set";
    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext()}
        onSave={vi.fn()}
        onReloadLatest={vi.fn()}
        saveError={
          new ContextWidenForbiddenError(message, {
            field: "forbidden_tools",
            layer: 2,
            layerName: "organisation default",
            removedItems: ["shell_exec"],
          })
        }
        isSaving={false}
        canWrite
      />,
    );

    // The specific restriction's own inline error carries the full,
    // field-naming message from the server.
    expect(screen.getByText(message)).toBeInTheDocument();
    // The top banner is a pointer to it, not a second copy of a generic
    // validation message — this is what distinguishes "which restriction
    // was hit" from a generic "could not save" the coordinator flagged.
    expect(
      screen.getByText("Could not save — see the highlighted restriction below."),
    ).toBeInTheDocument();
  });
});

describe("GovContextEditor — secret refusal names the kind found, never a field", () => {
  it("shows the category-based message and does not flag any restriction field", () => {
    const message =
      "this write was refused because it contains a value shaped like a bearer token — remove it and try again";
    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext()}
        onSave={vi.fn()}
        onReloadLatest={vi.fn()}
        saveError={new ContextSecretDetectedError(message, "bearer_token")}
        isSaving={false}
        canWrite
      />,
    );

    expect(screen.getByText(message)).toBeInTheDocument();
    // Never the widen copy — a secret refusal is not a widen refusal, and
    // DetectSecret never reports which field matched (use-context.ts's own
    // doc comment), so there is nothing to highlight inline.
    expect(
      screen.queryByText("Could not save — see the highlighted restriction below."),
    ).not.toBeInTheDocument();
  });
});

describe("GovContextEditor — version conflict offers reload, never a silent merge", () => {
  it("renders a reload action that fetches the latest version and re-baselines the form", () => {
    const fresh = buildContext({ version: 4 });
    const onReloadLatest = vi.fn().mockResolvedValue(fresh);
    const message =
      "base_version 3 does not match the current version 4 — reread the current context and retry";

    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext({ version: 3 })}
        onSave={vi.fn()}
        onReloadLatest={onReloadLatest}
        saveError={new ContextVersionConflictError(message, 4, 3)}
        isSaving={false}
        canWrite
      />,
    );

    expect(screen.getByText(message)).toBeInTheDocument();
    const reloadButton = screen.getByRole("button", {
      name: "Discard my changes and reload the latest version",
    });
    fireEvent.click(reloadButton);
    expect(onReloadLatest).toHaveBeenCalledTimes(1);
  });
});

describe("GovContextEditor — no false positives when there is no error", () => {
  it("renders none of the three refusal banners when saveError is null", () => {
    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext()}
        onSave={vi.fn()}
        onReloadLatest={vi.fn()}
        saveError={null}
        isSaving={false}
        canWrite
      />,
    );

    expect(
      screen.queryByText("Could not save — see the highlighted restriction below."),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /reload the latest version/i }),
    ).not.toBeInTheDocument();
  });
});

describe("GovContextEditor — a rejected guidance value tells the operator why", () => {
  // CodeRabbit finding on #566: contextFormSchema rejects a guidance value
  // over 2,000 characters, handleSubmit correctly never calls onSave for it —
  // but GuidanceField rendered no error at all, so the operator just saw the
  // save silently do nothing.
  it("shows a validation error on the specific field that failed, with aria-invalid/aria-describedby wired", async () => {
    const onSave = vi.fn();
    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext()}
        onSave={onSave}
        onReloadLatest={vi.fn()}
        saveError={null}
        isSaving={false}
        canWrite
      />,
    );

    const brandVoice = screen.getByLabelText("Brand voice");
    fireEvent.change(brandVoice, { target: { value: "x".repeat(2001) } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const error = await screen.findByText(/expected string to have <=2000 characters/i);
    expect(error).toBeInTheDocument();
    expect(error).toHaveAttribute("role", "alert");
    expect(brandVoice).toHaveAttribute("aria-invalid", "true");
    expect(brandVoice).toHaveAttribute("aria-describedby", error.id);
    // Non-vacuous: the save must genuinely be blocked, not just decorated
    // with an error while still submitting.
    expect(onSave).not.toHaveBeenCalled();
  });

  it("shows no guidance errors when every field is valid (must not over-fire)", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <GovContextEditor
        scopeLabel="site"
        current={buildContext()}
        onSave={onSave}
        onReloadLatest={vi.fn()}
        saveError={null}
        isSaving={false}
        canWrite
      />,
    );

    fireEvent.change(screen.getByLabelText("Brand voice"), {
      target: { value: "Friendly and direct." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
