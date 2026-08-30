import { describe, it, expect } from "vitest";
import { screen, fireEvent, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { Route } from "@/routes/_authed/ai/connect";
import { MCP_CLIENTS } from "./client-table";

// The wizard, through the real route component, the real router and a real
// QueryClient -- not by calling a hook or rendering a subcomponent in
// isolation. What is being tested is the computation from client to method to
// snippet, and that computation only exists once the route has mounted the
// wizard with a real endpoint.

const ConnectPage = Route.options.component!;

function renderWizard() {
  return renderWithProviders(<ConnectPage />, {
    withRouter: true,
    initialPath: "/ai/connect",
  });
}

async function pickClient(name: string) {
  const card = await screen.findByRole("button", { name: new RegExp(name, "i") });
  fireEvent.click(card);
  return card;
}

function authCard(method: "oauth" | "token"): HTMLButtonElement {
  const el = document.querySelector<HTMLButtonElement>(`button[data-method="${method}"]`);
  // A missing card must fail the test rather than letting every assertion
  // below it be skipped.
  if (el === null) throw new Error(`no auth card rendered for "${method}"`);
  return el;
}

describe("the wizard asks for the client first", () => {
  it("renders every row in the table as a picker card, including the generic one", async () => {
    renderWizard();
    for (const client of MCP_CLIENTS) {
      expect(
        await screen.findByRole("button", { name: new RegExp(client.name, "i") }),
      ).toBeInTheDocument();
    }
    // The generic entry is present and carries the same affordance as the rest,
    // not a smaller one.
    const generic = await screen.findByRole("button", { name: /other \/ generic/i });
    expect(generic).toBeEnabled();
  });

  it("does not offer an auth method before a client is chosen", async () => {
    renderWizard();
    await screen.findByRole("button", { name: /claude code/i });
    expect(document.querySelector('button[data-method="oauth"]')).toBeNull();
  });
});

describe("the method step is computed from the client, with the reason on the card", () => {
  it("disables the token method for Claude Desktop and says exactly why", async () => {
    renderWizard();
    await pickClient("Claude Desktop");

    const token = authCard("token");
    expect(token).toBeDisabled();
    // NEVER A GENERIC "UNAVAILABLE". The card carries the client's own reason.
    expect(within(token).getByText(/no header field/i)).toBeInTheDocument();

    // And the method that DOES work is offered, so the guard is not blanket.
    expect(authCard("oauth")).toBeEnabled();
  });

  it("disables OAuth for VS Code as 'not yet verified by us', with the date", async () => {
    renderWizard();
    await pickClient("VS Code");

    const oauth = authCard("oauth");
    expect(oauth).toBeDisabled();
    // The two halves the design asks for: not verified BY US, and when we last
    // looked, so a stale row looks stale rather than permanent.
    expect(within(oauth).getByText(/not yet verified by us/i)).toBeInTheDocument();
    expect(within(oauth).getByText(/last checked 2026-08-24/i)).toBeInTheDocument();

    expect(authCard("token")).toBeEnabled();
  });

  it("enables both methods for a client that supports both", async () => {
    // The over-fire case: a correct client must not be blocked by the
    // disabling logic.
    renderWizard();
    await pickClient("Cursor");
    expect(authCard("oauth")).toBeEnabled();
    expect(authCard("token")).toBeEnabled();
  });
});

describe("the setup artefact is generated per client", () => {
  it("emits the required http type for Claude Code", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));

    const block = await screen.findByText(/"mcpServers"/);
    const text = block.textContent ?? "";
    expect(text).toContain('"type": "http"');
    expect(text).toContain('"url"');
    // The reason for that line is on screen, not only in a comment.
    expect(screen.getByText(/read as a local process/i)).toBeInTheDocument();
  });

  it("emits httpUrl and no url for Gemini CLI", async () => {
    renderWizard();
    await pickClient("Gemini CLI");
    fireEvent.click(authCard("oauth"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).toContain('"httpUrl"');
    expect(text).not.toContain('"url"');
  });

  it("emits the servers wrapper for VS Code", async () => {
    renderWizard();
    await pickClient("VS Code");
    fireEvent.click(authCard("token"));

    const text = (await screen.findByText(/"servers"/)).textContent ?? "";
    expect(text).toContain('"servers"');
    expect(text).not.toContain('"mcpServers"');
  });

  it("emits no type key for Cursor", async () => {
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("oauth"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).not.toContain('"type"');
  });

  it("renders the endpoint and a spec link for the generic entry, with no config block", async () => {
    renderWizard();
    await pickClient("Other / generic");
    fireEvent.click(authCard("oauth"));

    expect(await screen.findByText(/endpoint for other \/ generic/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /streamable http specification/i }),
    ).toBeInTheDocument();
  });

  it("gives GUI clients in-app steps rather than a file to edit", async () => {
    renderWizard();
    await pickClient("Claude Desktop");
    fireEvent.click(authCard("oauth"));

    expect(await screen.findByText(/set this up inside claude desktop/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
  });

  it("never prints a Windows path", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);
    // Every source documented POSIX only; a Windows path here would be invented.
    expect(document.body.textContent ?? "").not.toMatch(/[A-Z]:\\|%APPDATA%/);
  });

  it("tells the user a token cannot be minted here yet rather than showing a fake one", async () => {
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).toContain("YOUR_CONNECTION_TOKEN");
    expect(screen.getByText(/cannot mint a token here yet/i)).toBeInTheDocument();
  });
});

describe("the wizard does not promise things it cannot deliver", () => {
  it("does not claim the entered name appears on the approval screen", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);

    // The old copy said "shown on the approval screen". Nothing carries the
    // name there -- the client starts the OAuth flow itself -- so an operator
    // would go looking for something that is not there. Wrong prose is a defect.
    expect(screen.queryByText(/shown on the approval screen/i)).not.toBeInTheDocument();
    expect(screen.getByText(/used as the server key in the config/i)).toBeInTheDocument();
    expect(screen.getByText(/asks you to name\s+the connection separately/i)).toBeInTheDocument();
  });

  it("states the self-hosted proxy requirement beside the endpoint it printed", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);

    // The URL is derived from the origin, which does not prove anything
    // forwards it: infra/nginx/nginx.conf has no /mcp location and
    // apps/web/vite.config.ts does not proxy it, so on a self-hosted install
    // or in dev the copied URL reaches the SPA. Saying nothing would hand
    // someone a web page and let them debug their AI client.
    expect(screen.getByText(/reverse proxy must forward \/mcp to the API/i)).toBeInTheDocument();
  });
});

describe("changing the client recomputes rather than carrying a stale answer", () => {
  it("drops a method the newly chosen client cannot use", async () => {
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    expect(await screen.findByText(/"mcpServers"/)).toBeInTheDocument();

    // Claude Desktop cannot use a token. The wizard must fall back to asking,
    // not silently keep a selection that produces no valid artefact.
    await pickClient("Claude Desktop");
    expect(screen.queryByText(/set this up inside/i)).not.toBeInTheDocument();
    expect(authCard("token")).toBeDisabled();
  });
});
