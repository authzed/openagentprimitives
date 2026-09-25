import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { WorkshopsView } from "./WorkshopsView";
import type { WorkshopRow } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// jsonResponse builds a fetch Response the way admind's handlers do: a JSON
// body at the given status (mirrors InstallAgentForm.test.tsx's helper).
function jsonResponse(body: unknown, status: number) {
  return new Response(JSON.stringify(body), { status });
}

// row builds one WorkshopRow fixture, overridable per test. Fixture names
// throughout (ws-session, builder-x, agents/weather-ai, demo-admin, ns-x)
// mirror pkg/web/admind/workshops_test.go's own fake fixtures.
function row(overrides: Partial<WorkshopRow> = {}): WorkshopRow {
  return {
    namespace: "ns-x",
    name: "ws-session-workshop",
    session: "ns-x/ws-session",
    starter: "builder-x",
    phase: "Ready",
    pendingInstall: false,
    pendingCapability: false,
    exported: true,
    created: "2026-06-30T12:00:00Z",
    ...overrides,
  };
}

describe("WorkshopsView", () => {
  it("a pending-install row shows Install/Decline; an installed row shows its installedRef; a capability-only row says so — none of that on a plain row", async () => {
    const rows: WorkshopRow[] = [
      row({ name: "ws-pending", pendingInstall: true, suggestedName: "weather-ai" }),
      row({ name: "ws-installed", installPhase: "Installed", installedRef: "agents/weather-ai" }),
      row({ name: "ws-capability", pendingCapability: true }),
    ];
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(rows, 200)));

    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("ns-x/ws-pending")).toBeTruthy());

    // Only the pending row gets Install + Decline.
    expect(screen.getByRole("button", { name: "Install" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Decline" })).toBeTruthy();

    // The installed row shows its terminal phase + resolved AgentClass ref.
    expect(screen.getByText("Installed")).toBeTruthy();
    expect(screen.getByText("agents/weather-ai")).toBeTruthy();

    // The capability-only row (no install requested) says so, not "—".
    expect(screen.getByText("capability requested")).toBeTruthy();

    // Kill is always available — one per row, regardless of state.
    expect(screen.getAllByRole("button", { name: "Kill" })).toHaveLength(3);
  });

  it("a 400 missing-questions install response renders the returned question — a secret renders masked and EMPTY, never pre-filled — and re-POSTs with the filled answer; the list refetches after", async () => {
    const listRow = row({ pendingInstall: true, suggestedName: "weather-ai" });
    const fetchMock = vi
      .fn()
      // Initial list load.
      .mockResolvedValueOnce(jsonResponse([listRow], 200))
      // First submit: no answers yet — the bundle's required secret question.
      .mockResolvedValueOnce(
        jsonResponse(
          { error: "missing required question(s)", questions: [{ name: "githubToken", type: "secret", prompt: "GitHub PAT" }] },
          400,
        ),
      )
      // Second submit: the answer is included — installs cleanly.
      .mockResolvedValueOnce(
        jsonResponse({ name: "weather-ai", appliedKinds: ["AgentClass"], secretsCreated: 1 }, 200),
      )
      // The post-install refetch of the list.
      .mockResolvedValueOnce(jsonResponse([{ ...listRow, pendingInstall: false, installPhase: "Installed" }], 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Install" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm install" }));

    // The missing question renders as a MASKED (password) input — and it
    // starts EMPTY: no secret answer is ever pre-filled by this form.
    const secretInput = (await screen.findByLabelText("GitHub PAT *")) as HTMLInputElement;
    expect(secretInput.getAttribute("type")).toBe("password");
    expect(secretInput.value).toBe("");

    fireEvent.change(secretInput, { target: { value: "fake-pat-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    // The dialog closes and the list refetches (a 4th fetch call happens).
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Install" })).toBeNull());

    // The second install POST carries the typed answer — never a pre-filled one.
    const secondCallBody = JSON.parse((fetchMock.mock.calls[2][1] as RequestInit).body as string);
    expect(secondCallBody).toEqual({
      namespace: "ns-x",
      name: "weather-ai",
      values: { githubToken: "fake-pat-value" },
      adopt: [],
    });
    // The install POST hit THIS workshop's own route, not the generic oap-install one.
    expect(fetchMock.mock.calls[1][0]).toBe("/admin/api/workshops/ns-x/ws-session-workshop/install");
  });

  it("a 409 conflicts install response renders a tickable row per conflict (Secret starts unticked) and re-submits only the ticked keys", async () => {
    const listRow = row({ pendingInstall: true, suggestedName: "weather-ai" });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse([listRow], 200))
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [
              { kind: "AgentClass", namespace: "agents", name: "weather-ai" },
              { kind: "Secret", namespace: "agents", name: "weather-ai-token", secret: true },
            ],
          },
          409,
        ),
      )
      .mockResolvedValueOnce(jsonResponse({ name: "weather-ai", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200))
      .mockResolvedValueOnce(jsonResponse([{ ...listRow, pendingInstall: false, installPhase: "Installed" }], 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Install" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm install" }));

    // Both conflicts render; the Secret starts UNTICKED and names the risk.
    const classRow = (await screen.findByLabelText(/AgentClass agents\/weather-ai/)) as HTMLInputElement;
    expect(classRow.checked).toBe(false);
    const secretRow = screen.getByLabelText(/Secret agents\/weather-ai-token/) as HTMLInputElement;
    expect(secretRow.checked).toBe(false);
    expect(screen.getByText(/overwrites this Secret's data/i)).toBeTruthy();

    fireEvent.click(classRow);
    fireEvent.click(screen.getByRole("button", { name: /adopt and install/i }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
    const secondCallBody = JSON.parse((fetchMock.mock.calls[2][1] as RequestInit).body as string);
    // Only the ticked key rides the re-submit — never the Secret conflict too.
    expect(secondCallBody.adopt).toEqual(["AgentClass/weather-ai"]);
  });

  it("Decline POSTs the decline route and the list refetches", async () => {
    const listRow = row({ pendingInstall: true, suggestedName: "weather-ai" });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse([listRow], 200))
      .mockResolvedValueOnce(jsonResponse({ phase: "Declined", approvedBy: "user:demo-admin" }, 200))
      .mockResolvedValueOnce(jsonResponse([{ ...listRow, pendingInstall: false, installPhase: "Declined" }], 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Decline" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Decline" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(fetchMock.mock.calls[1][0]).toBe("/admin/api/workshops/ns-x/ws-session-workshop/decline");
    await waitFor(() => expect(screen.getByText("Declined")).toBeTruthy());
  });

  it("Kill requires the confirm dialog — del() does not fire on opening the dialog, only on confirming — and the list refetches after", async () => {
    const listRow = row();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse([listRow], 200))
      // The DELETE itself: admind answers 204 No Content, no body.
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(jsonResponse([], 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Kill" })).toBeTruthy());

    // Opening the confirm dialog must NOT call fetch — only the initial list GET so far.
    fireEvent.click(screen.getByRole("button", { name: "Kill" }));
    expect(await screen.findByText(/cannot be undone/)).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Only clicking the confirm button fires the DELETE.
    fireEvent.click(screen.getByRole("button", { name: "Kill it" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(fetchMock.mock.calls[1][0]).toBe("/admin/api/workshops/ns-x/ws-session-workshop");
    expect((fetchMock.mock.calls[1][1] as RequestInit).method).toBe("DELETE");
    // The row is gone once the refetched (now-empty) list lands.
    await waitFor(() => expect(screen.getByText(/No workshops yet/)).toBeTruthy());
  });

  it("a failed list fetch surfaces the error alert", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("nope", { status: 500 })));
    render(<WorkshopsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load workshops/)).toBeTruthy());
  });
});
