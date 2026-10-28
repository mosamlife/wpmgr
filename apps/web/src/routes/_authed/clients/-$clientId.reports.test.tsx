import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type {
  ClientReportSchedule,
  ClientReportScheduleUpdate,
  ClientReportSectionFlags,
} from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as ReportsRoute } from "./$clientId.reports";

// GH #972. The client report schedule form had no Overview switch, so a save
// never sent `sections.overview`, and the server used to read a missing flag as
// off. The form now carries all six section switches.
//
// The real route is re-attached to a throwaway root (the technique
// routes/_authed/admin/-content-report.test.tsx uses) and rendered through the
// router and a QueryClient. Only the generated operations are mocked, so the
// real domain hooks (features/clients/use-reports.ts), the form and the cache
// run, and the PUT arguments below are exactly what the app would send.
//
// Wire shapes, each found in the source rather than assumed:
//   - Response: scheduleResponseDTO in apps/api/internal/report/handler.go
//     carries all six `sections` flags (SectionFlags in
//     apps/api/internal/report/reportdata/reportdata.go).
//   - Request: scheduleUpdateBody in the same handler takes `sections` as a
//     SectionFlagsPatch (apps/api/internal/report/model.go), which the service
//     merges over the stored flags one flag at a time (UpsertSchedule in
//     apps/api/internal/report/service.go). The fake server below does the same.
//   - Every section defaults to on (DefaultSectionFlags in reportdata.go).
//   - Section order is the order the report shows them: Overview first, then per
//     site Uptime, Backups, Updates, Performance, Email
//     (apps/api/internal/report/render/html/templates/report.html.tmpl and
//     apps/api/internal/report/render/pdf/fpdf.go).

interface ScheduleGetOptions {
  path: { clientId: string };
}

interface SchedulePutOptions {
  path: { clientId: string };
  body: ClientReportScheduleUpdate;
}

const { getSchedule, putSchedule, listReports } = vi.hoisted(() => ({
  getSchedule: vi.fn<(options: ScheduleGetOptions) => Promise<unknown>>(),
  putSchedule: vi.fn<(options: SchedulePutOptions) => Promise<unknown>>(),
  listReports: vi.fn<(options: ScheduleGetOptions) => Promise<unknown>>(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getClientReportSchedule: getSchedule,
    putClientReportSchedule: putSchedule,
    listClientReports: listReports,
  };
});

const CLIENT_ID = "00000000-0000-0000-0000-0000000000c1";

type SectionKey = keyof ClientReportSectionFlags;

const SECTIONS: Array<[SectionKey, string]> = [
  ["overview", "Overview"],
  ["uptime", "Uptime"],
  ["backups", "Backups"],
  ["updates", "Updates"],
  ["performance", "Performance"],
  ["email", "Email"],
];

// A pattern that is not all one value, so a switch wired to the wrong flag, or a
// flag that changes when another is saved, shows up as a different value.
const MIXED: Required<ClientReportSectionFlags> = {
  overview: true,
  uptime: true,
  backups: false,
  updates: true,
  performance: false,
  email: true,
};

const ALL_OFF: Required<ClientReportSectionFlags> = {
  overview: false,
  uptime: false,
  backups: false,
  updates: false,
  performance: false,
  email: false,
};

let stored: ClientReportSchedule;

function seed(sections: ClientReportSectionFlags): void {
  stored = {
    client_id: CLIENT_ID,
    enabled: true,
    cadence: "monthly",
    send_day: 1,
    send_hour: 9,
    timezone: "UTC",
    recipients: ["client@example.com"],
    sections,
    intro_text: "",
    closing_text: "",
    powered_by_removed: false,
    instance_mailer_configured: true,
  };
}

/** What a generated operation resolves to when the request succeeds. */
function ok(data: unknown) {
  return { data, error: undefined, response: new Response(null, { status: 200 }) };
}

beforeEach(() => {
  vi.resetAllMocks();
  getSchedule.mockImplementation(() => Promise.resolve(ok(stored)));
  listReports.mockImplementation(() => Promise.resolve(ok({ items: [] })));
  putSchedule.mockImplementation(({ body }) => {
    stored = {
      ...stored,
      enabled: body.enabled,
      cadence: body.cadence,
      send_day: body.send_day,
      send_hour: body.send_hour,
      recipients: body.recipients,
      sections: { ...stored.sections, ...body.sections },
      intro_text: body.intro_text ?? "",
      closing_text: body.closing_text ?? "",
      powered_by_removed: body.powered_by_removed ?? false,
    };
    return Promise.resolve(ok(stored));
  });
});

function renderReportsTab(): void {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ReportsRoute.update>[0];
  const tab = ReportsRoute.update({
    id: "/clients/$clientId/reports",
    path: "/clients/$clientId/reports",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([tab]),
    history: createMemoryHistory({
      initialEntries: [`/clients/${CLIENT_ID}/reports`],
    }),
  });
  renderWithProviders(<RouterProvider router={router} />, {
    queryClient: createTestQueryClient(),
  });
}

/** The route is lazy under code splitting, so the first lookup must be async. */
function sectionSwitch(label: string): Promise<HTMLElement> {
  return screen.findByRole("switch", { name: `Include ${label} section` });
}

/** Asserts every section switch against the expected on/off state. */
async function expectSwitches(
  expected: Required<ClientReportSectionFlags>,
): Promise<void> {
  for (const [key, label] of SECTIONS) {
    const sw = await sectionSwitch(label);
    if (expected[key]) expect(sw).toBeChecked();
    else expect(sw).not.toBeChecked();
  }
}

/** Presses Save and returns the arguments the PUT was called with. */
async function saveSchedule(): Promise<SchedulePutOptions> {
  fireEvent.click(screen.getByRole("button", { name: "Save schedule" }));
  await waitFor(() => expect(putSchedule).toHaveBeenCalledTimes(1));
  const call = putSchedule.mock.calls[0];
  if (!call) throw new Error("the schedule PUT was not called");
  return call[0];
}

/** Waits for the save to finish: the form is reset to the saved values. */
async function settled(): Promise<void> {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Save schedule" })).toBeDisabled(),
  );
}

describe("client report schedule form: section switches", () => {
  it("lists Overview first, then Uptime, Backups, Updates, Performance and Email, each labelled like the others", async () => {
    seed({ ...MIXED });
    renderReportsTab();

    const switches = await screen.findAllByRole("switch", {
      name: /^Include .+ section$/,
    });
    expect(switches.map((s) => s.getAttribute("aria-label"))).toEqual(
      SECTIONS.map(([, label]) => `Include ${label} section`),
    );
    // Each switch has a visible label bound to it, as the existing five do.
    SECTIONS.forEach(([, label]) => {
      const sw = screen.getByRole("switch", { name: `Include ${label} section` });
      expect(sw.id).not.toBe("");
      expect(screen.getByText(label, { selector: "label" })).toHaveAttribute(
        "for",
        sw.id,
      );
    });
  });

  it("shows Overview on when the stored schedule has it on, and the other five as stored", async () => {
    seed({ ...MIXED, overview: true });
    renderReportsTab();

    await expectSwitches({ ...MIXED, overview: true });
  });

  it("shows Overview off when the stored schedule has it off, and the other five as stored", async () => {
    seed({ ...MIXED, overview: false });
    renderReportsTab();

    await expectSwitches({ ...MIXED, overview: false });
  });

  it.each(SECTIONS)(
    "reads a response that leaves out %s as on, and leaves the other five as stored",
    async (missing) => {
      const flags: ClientReportSectionFlags = { ...ALL_OFF };
      delete flags[missing];
      seed(flags);
      renderReportsTab();

      await expectSwitches({ ...ALL_OFF, [missing]: true });
    },
  );
});

describe("client report schedule form: saving the section switches", () => {
  it("sends Overview off when the switch is turned off, and the other five flags as stored", async () => {
    seed({ ...MIXED, overview: true });
    renderReportsTab();

    fireEvent.click(await sectionSwitch("Overview"));
    expect(await sectionSwitch("Overview")).not.toBeChecked();
    const sent = await saveSchedule();

    expect(sent.path).toEqual({ clientId: CLIENT_ID });
    expect(sent.body.sections).toEqual({ ...MIXED, overview: false });
    // The rest of the schedule goes back exactly as it was loaded.
    expect(sent.body).toMatchObject({
      enabled: true,
      cadence: "monthly",
      send_day: 1,
      send_hour: 9,
      recipients: ["client@example.com"],
      intro_text: "",
      closing_text: "",
      powered_by_removed: false,
    });
    await settled();
    expect(await sectionSwitch("Overview")).not.toBeChecked();
  });

  it("sends Overview on when the switch is turned on from a stored off, and the other five flags as stored", async () => {
    seed({ ...MIXED, overview: false });
    renderReportsTab();

    fireEvent.click(await sectionSwitch("Overview"));
    expect(await sectionSwitch("Overview")).toBeChecked();
    const sent = await saveSchedule();

    expect(sent.body.sections).toEqual({ ...MIXED, overview: true });
    await settled();
    expect(await sectionSwitch("Overview")).toBeChecked();
  });

  it("keeps a stored-off Overview off when only another switch is changed", async () => {
    seed({ ...MIXED, overview: false });
    renderReportsTab();

    fireEvent.click(await sectionSwitch("Email"));
    const sent = await saveSchedule();

    expect(sent.body.sections).toEqual({ ...MIXED, overview: false, email: false });
    await settled();
    expect(await sectionSwitch("Overview")).not.toBeChecked();
  });

  it("sends a flag the response left out as on, because that is what the form shows", async () => {
    const flags: ClientReportSectionFlags = { ...MIXED };
    delete flags.overview;
    delete flags.email;
    seed(flags);
    renderReportsTab();

    expect(await sectionSwitch("Overview")).toBeChecked();
    expect(await sectionSwitch("Email")).toBeChecked();
    fireEvent.click(await sectionSwitch("Performance"));
    const sent = await saveSchedule();

    expect(sent.body.sections).toEqual({
      ...MIXED,
      overview: true,
      email: true,
      performance: true,
    });
  });
});
