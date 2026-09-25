import "@testing-library/jest-dom/vitest";
import { render, screen, waitFor, fireEvent, cleanup, within } from "@testing-library/react";
import { describe, expect, it, vi, afterEach, beforeAll } from "vitest";
import { ClusterTab } from "./ClusterTab";
import type { ClusterSettingsResponse, ClusterSettingsUpdateResponse, ValidationResult } from "../lib/api";

beforeAll(() => {
  if (!Element.prototype.hasPointerCapture) {
    Element.prototype.hasPointerCapture = () => false;
  }
  if (!Element.prototype.releasePointerCapture) {
    Element.prototype.releasePointerCapture = () => {};
  }
  if (!Element.prototype.scrollIntoView) {
    Element.prototype.scrollIntoView = () => {};
  }
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

interface Call {
  method: string;
  path: string;
  body?: unknown;
}

// stubFetch dispatches on "METHOD /path", recording each call (with parsed
// JSON body, when present) so a test can both script responses and assert
// what was actually sent — needed for the take-ownership flow, which must
// prove takeOwnership:true landed on the wire, not just that a re-PUT
// happened.
function stubFetch(handlers: Record<string, (call: Call) => Response>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      const path = new URL(String(input), "http://settings.local").pathname;
      let body: unknown;
      if (typeof init?.body === "string") {
        try {
          body = JSON.parse(init.body);
        } catch {
          body = init.body;
        }
      }
      const call: Call = { method, path, body };
      calls.push(call);
      const key = `${method} ${path}`;
      const handler = handlers[key];
      if (!handler) throw new Error(`unstubbed fetch: ${key}`);
      return handler(call);
    }),
  );
  return calls;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const foundResp: ClusterSettingsResponse = {
  clusterDown: false,
  found: true,
  spec: { limits: { minPlanGateMode: "logging" }, modelCatalog: [] },
};

const cleanValidation: ValidationResult = {
  errors: [],
  violations: [],
  effective: { limits: { minPlanGateMode: "logging" } },
};

describe("ClusterTab", () => {
  it("shows an Alert replacing the body when the cluster is down, with no editable sections", async () => {
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse({ clusterDown: true, found: false }),
    });
    render(<ClusterTab apiBase="/api" running={false} />);
    await waitFor(() => expect(screen.getByText(/desktop cluster isn.t running/i)).toBeInTheDocument());
    expect(screen.queryByText(/model catalog/i)).not.toBeInTheDocument();
  });

  it("loads the spec and lets each sub-tab reveal its curated section", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<ClusterTab apiBase="/api" running={true} />);
    // Catalog is the default sub-tab, so its section is visible immediately.
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByText(/limits \(ceilings\)/i)).toBeInTheDocument());

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Defaults" }));
    await waitFor(() => expect(screen.getByText(/defaults \(fallbacks\)/i)).toBeInTheDocument());
  });

  it("renders a vertical sub-tab list (Catalog/Limits/Defaults) with Catalog active by default", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["Catalog", "Limits", "Defaults"]);
    expect(screen.getByRole("tab", { name: "Catalog" })).toHaveAttribute("data-state", "active");
    expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "inactive");
    expect(screen.getByRole("tab", { name: "Defaults" })).toHaveAttribute("data-state", "inactive");
  });

  it("switching sub-tabs via mouseDown reveals the target section and hides the others", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    // Capture the Catalog panel's wrapper while it's still active — Radix
    // keeps the tabpanel DIV mounted (with a toggling `hidden` attribute)
    // even after its own section content unmounts, so this reference stays
    // valid to assert on after switching away.
    const catalogPanel = screen.getByText(/model catalog/i).closest('[role="tabpanel"]');
    expect(catalogPanel).not.toHaveAttribute("hidden");

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));

    const limitsPanel = screen.getByText(/limits \(ceilings\)/i).closest('[role="tabpanel"]');
    expect(limitsPanel).not.toHaveAttribute("hidden");
    expect(catalogPanel).toHaveAttribute("hidden");
  });

  it("an edit made in Limits survives switching to Catalog and back", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));

    const maxTurns = screen.getByLabelText(/ceiling: max turns/i) as HTMLInputElement;
    fireEvent.change(maxTurns, { target: { value: "42" } });
    expect(maxTurns.value).toBe("42");

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Catalog" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Catalog" })).toHaveAttribute("data-state", "active"));

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));

    expect((screen.getByLabelText(/ceiling: max turns/i) as HTMLInputElement).value).toBe("42");
  });

  it("Validate/Save are reachable and functional from a non-default sub-tab", async () => {
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(cleanValidation),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Defaults" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Defaults" })).toHaveAttribute("data-state", "active"));

    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));
    await waitFor(() => expect(screen.getByText(/effective settings \(preview\)/i)).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
  });

  it("dirty is interaction-based: false on load, true across a clear-and-retype revert, false after save and it STAYS false", async () => {
    // The revert reorders JSON keys (setPath delete→re-set appends the key),
    // which is exactly why dirty must be an explicit edited flag, not a
    // serialization comparison: the flag is TRUE here because an interaction
    // happened (that's fine), and a successful save clears it for good.
    const specWithSiblings = {
      clusterDown: false,
      found: true,
      spec: { limits: { budget: { maxTurns: 5, maxTokens: 10 }, minPlanGateMode: "logging" } },
    };
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(specWithSiblings),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    // Load alone is not an edit.
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));

    const maxTurns = () => screen.getByLabelText(/ceiling: max turns/i) as HTMLInputElement;
    expect(maxTurns().value).toBe("5");
    fireEvent.change(maxTurns(), { target: { value: "" } });
    fireEvent.change(maxTurns(), { target: { value: "5" } });
    // An interaction happened, so dirty is true even though the value matches.
    expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i);

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    // ...and it STAYS false across non-editing interactions like sub-tab
    // switches (the old serialization compare could re-trip here since the
    // saved spec's key order never matches the loaded baseline's again).
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Catalog" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Catalog" })).toHaveAttribute("data-state", "active"));
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("Edit→Save of an unchanged catalog entry marks dirty (it IS an interaction); a successful save clears it", async () => {
    // The entry is crafted so the dialog's rebuild is byte-identical (same
    // keys, same order, same values, same array position) — a serialization
    // comparison sees NO change here and never shows the indicator, which is
    // the bug: the user interacted, so dirty must be true until saved.
    const entry = {
      name: "m1",
      provider: "anthropic",
      default: false,
      inputPerMTok: 1,
      outputPerMTok: 2,
      tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-m1", key: "token" },
    };
    const respWithEntry = { clusterDown: false, found: true, spec: { modelCatalog: [entry] } };
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(respWithEntry),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // The user went through Edit→Save: that is an interaction, so the
    // indicator must show even though the rebuilt entry is byte-identical.
    expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i);

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("shows the Unsaved changes indicator once the spec is edited, and clears it after a successful save", async () => {
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));
    fireEvent.change(screen.getByLabelText(/ceiling: max turns/i), { target: { value: "7" } });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i));

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("a token staged during the drift window keeps dirty true through a take-ownership save, until an ordinary save sends it", async () => {
    // The take-ownership PUT omits `tokens` (it re-applies the spec only),
    // so a token staged while the DriftPanel is showing is NOT sent by it —
    // clearing the indicator there would be a false-clean over a pending
    // Secret write. Only the ordinary save that actually carries the token
    // may clear it.
    const entry = {
      name: "m1",
      provider: "anthropic",
      default: false,
      inputPerMTok: 1,
      outputPerMTok: 2,
      tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-m1", key: "token" },
    };
    const respWithEntry = { clusterDown: false, found: true, spec: { modelCatalog: [entry] } };
    const putResponses: ClusterSettingsUpdateResponse[] = [
      { applied: true, validation: cleanValidation, drift: ["limits.budget.maxTurns"] },
      { applied: true, validation: cleanValidation, drift: [] },
      { applied: true, validation: cleanValidation },
    ];
    let putCount = 0;
    const calls = stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(respWithEntry),
      "PUT /api/cluster/settings": () => {
        const resp = putResponses[putCount];
        putCount += 1;
        return jsonResponse(resp);
      },
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    // Ordinary save comes back with drift; nothing is pending afterwards.
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByText("limits.budget.maxTurns")).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    // Stage a token while the DriftPanel is up: Edit the entry, type a
    // token value, dialog-Save. The staged write marks dirty.
    fireEvent.click(screen.getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/api token/i), { target: { value: "tok-pending" } });
    fireEvent.click(within(dialog).getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i);

    // Take ownership: the re-PUT applies the spec WITHOUT the staged token,
    // so the indicator must survive this save.
    fireEvent.click(screen.getByRole("button", { name: /take full ownership and re-apply/i }));
    await waitFor(() => expect(screen.queryByText("limits.budget.maxTurns")).not.toBeInTheDocument());
    const ownershipPut = calls.filter((c) => c.method === "PUT")[1].body as {
      takeOwnership?: boolean;
      tokens?: unknown[];
    };
    expect(ownershipPut.takeOwnership).toBe(true);
    expect(ownershipPut.tokens).toBeUndefined();
    expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i);

    // The ordinary save DOES send the token — only now may dirty clear.
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());
    const finalPut = calls.filter((c) => c.method === "PUT")[2].body as { tokens?: { value?: string }[] };
    expect(finalPut.tokens).toHaveLength(1);
    expect(finalPut.tokens?.[0].value).toBe("tok-pending");
  });

  it("an open catalog dialog blocks sub-tab interaction via Dialog modality (regression pin)", async () => {
    // The open catalog dialog blocking sub-tab switches — which would
    // unmount CatalogSection and silently discard a half-typed form — is
    // incidental to Radix Dialog's modal default. jsdom cannot exercise the
    // block itself: a direct fireEvent.mouseDown on a trigger bypasses the
    // pointer-events/aria-hidden shield and switches anyway (verified — the
    // dialog unmounts and the typed data is lost), an event a real browser
    // can never deliver while the shield is up. So this pins the SHIELD:
    // while the dialog is open, body pointer events are off and the tab
    // triggers are hidden from the accessibility tree; on close, both
    // revert and switching works again. A future modality change
    // (modal={false}, a non-portal dialog) drops the shield and fails here
    // loudly instead of shipping as data loss.
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    expect(screen.getByRole("tab", { name: "Limits" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /add model/i }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/^name$/i), { target: { value: "half-typed" } });

    // The modal shield: pointer events disabled outside, triggers gone from
    // the accessibility tree (still in the DOM — hidden:true finds them).
    expect(document.body.style.pointerEvents).toBe("none");
    expect(screen.queryByRole("tab", { name: "Limits" })).not.toBeInTheDocument();
    expect(
      screen.getAllByRole("tab", { hidden: true }).find((t) => t.textContent === "Limits"),
    ).toBeDefined();
    // The dialog itself is interactive with its typed data intact.
    expect((within(dialog).getByLabelText(/^name$/i) as HTMLInputElement).value).toBe("half-typed");

    // Closing the dialog lifts the shield: triggers are reachable again and
    // switching works — proving the block is scoped to the open dialog.
    fireEvent.click(within(dialog).getByRole("button", { name: /^close$/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(document.body.style.pointerEvents).not.toBe("none");
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Limits" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("data-state", "active"));
  });

  it("bounded shell: sub-tab panels live in a scroll region; the capped panels row and ActionBar sit after it, outside", async () => {
    // jsdom cannot verify actual layout/scrolling, so this asserts the
    // structure the bounded shell depends on: the overflow container wraps
    // the section panels, the validation area is height-capped, and the
    // ActionBar is a later sibling outside the scroll region (always
    // visible). Visual confirmation is the user's live check.
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(cleanValidation),
    });
    const { container } = render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());

    expect((container.firstChild as HTMLElement).className).toMatch(/h-full/);

    const scrollRegion = screen.getByRole("tabpanel").parentElement as HTMLElement;
    expect(scrollRegion.className).toMatch(/overflow-y-auto/);
    expect(scrollRegion.className).toMatch(/min-w-0/);

    const save = screen.getByRole("button", { name: /^save$/i });
    expect(scrollRegion.contains(save)).toBe(false);
    expect(scrollRegion.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));
    await waitFor(() => expect(screen.getByText(/effective settings \(preview\)/i)).toBeInTheDocument());
    const panelsRow = screen.getByText(/effective settings \(preview\)/i).closest(".max-h-48");
    expect(panelsRow).not.toBeNull();
    expect((panelsRow as HTMLElement).className).toMatch(/overflow-y-auto/);
    // The panels row also precedes the ActionBar, keeping Save on-screen
    // however long a violations list gets.
    expect((panelsRow as HTMLElement).compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("Validate renders errors as blocking alerts and violations with reason badges", async () => {
    const badValidation: ValidationResult = {
      errors: ["limits.budget.maxTurns: must be >= 1"],
      violations: [{ reason: "AuthzClamped", message: "approval timeout clamped to cluster ceiling", fatal: false }],
    };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(badValidation, 422),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));

    await waitFor(() => expect(screen.getByText(/must be >= 1/i)).toBeInTheDocument());
    expect(screen.getByText("AuthzClamped")).toBeInTheDocument();
    expect(screen.getByText(/clamped to cluster ceiling/i)).toBeInTheDocument();
  });

  it("Validate renders the effective-settings summary on a clean spec", async () => {
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(cleanValidation),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));

    await waitFor(() => expect(screen.getByText(/effective settings \(preview\)/i)).toBeInTheDocument());
    expect(screen.getByText(/"minPlanGateMode": "logging"/)).toBeInTheDocument();
  });

  it("Save success with no drift shows Applied", async () => {
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
  });

  it("Save with non-empty drift renders the drift paths and a take-ownership button that re-PUTs with takeOwnership:true", async () => {
    const firstPut: ClusterSettingsUpdateResponse = {
      applied: true,
      validation: cleanValidation,
      drift: ["limits.budget.maxTurns"],
    };
    const secondPut: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation, drift: [] };
    let putCount = 0;
    const calls = stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => {
        putCount += 1;
        return jsonResponse(putCount === 1 ? firstPut : secondPut);
      },
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(screen.getByText("limits.budget.maxTurns")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /take full ownership and re-apply/i })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /take full ownership and re-apply/i }));
    await waitFor(() => expect(screen.queryByText("limits.budget.maxTurns")).not.toBeInTheDocument());

    const puts = calls.filter((c) => c.method === "PUT" && c.path === "/api/cluster/settings");
    expect(puts).toHaveLength(2);
    expect((puts[0].body as { takeOwnership?: boolean }).takeOwnership).toBeFalsy();
    expect((puts[1].body as { takeOwnership?: boolean }).takeOwnership).toBe(true);
  });

  it("residual drift after take-ownership renders as a warning, not the retry button", async () => {
    const driftResp: ClusterSettingsUpdateResponse = {
      applied: true,
      validation: cleanValidation,
      drift: ["defaults.model.provider"],
    };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(driftResp),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByRole("button", { name: /take full ownership and re-apply/i })).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: /take full ownership and re-apply/i }));
    await waitFor(() => expect(screen.getByText(/drift remains even after taking full ownership/i)).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /take full ownership and re-apply/i })).not.toBeInTheDocument();
  });

  it("a 422 save (validation failure) renders the errors without crashing", async () => {
    const failedPut: ClusterSettingsUpdateResponse = {
      applied: false,
      validation: { errors: ["defaults.model.provider: required"], violations: [] },
    };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(failedPut, 422),
    });
    render(<ClusterTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/model catalog/i)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(screen.getByText(/provider: required/i)).toBeInTheDocument());
    expect(screen.getByText(/not applied/i)).toBeInTheDocument();
  });
});
