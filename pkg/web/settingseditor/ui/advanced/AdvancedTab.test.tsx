import "@testing-library/jest-dom/vitest";
import { render, screen, waitFor, fireEvent, cleanup, act } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { AdvancedTab } from "./AdvancedTab";
import type { ClusterSettingsResponse, ClusterSettingsUpdateResponse, ValidationResult } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

interface Call {
  method: string;
  path: string;
  body?: unknown;
}

// stubFetch mirrors ClusterTab.test.tsx's helper: dispatch on "METHOD /path",
// recording each call (with parsed JSON body) so a test can assert the
// request body actually sent — needed to prove {yaml, takeOwnership:true}
// landed on the wire, not just that a re-PUT happened.
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

function textResponse(body: string, status: number): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/plain" } });
}

const SEED_YAML = "limits:\n  minPlanGateMode: logging\n";

const foundResp: ClusterSettingsResponse = {
  clusterDown: false,
  found: true,
  yaml: SEED_YAML,
};

const cleanValidation: ValidationResult = {
  errors: [],
  violations: [],
  effective: { limits: { minPlanGateMode: "logging" } },
};

function getTextarea(): HTMLTextAreaElement {
  return screen.getByLabelText(/cluster settings yaml/i) as HTMLTextAreaElement;
}

describe("AdvancedTab", () => {
  it("seeds the textarea from the GET's yaml field", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));
  });

  it("shows the Unsaved changes indicator once the text is edited, and clears it after a successful apply", async () => {
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    fireEvent.change(getTextarea(), { target: { value: SEED_YAML + "extra: 1\n" } });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/unsaved changes/i));

    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    fireEvent.click(screen.getByRole("button", { name: /apply without validating\?/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("bounded shell: the textarea is the growing scroll region and the ActionBar sits after it", async () => {
    // jsdom cannot verify actual layout/scrolling — this pins the structure
    // (grow classes on the textarea, ActionBar a later sibling); visual
    // confirmation is the user's live check.
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse(foundResp) });
    const { container } = render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    expect((container.firstChild as HTMLElement).className).toMatch(/h-full/);

    const ta = getTextarea();
    expect(ta.className).toMatch(/flex-1/);
    expect(ta.className).toMatch(/min-h-0/);

    const apply = screen.getByRole("button", { name: /^apply$/i });
    expect(ta.compareDocumentPosition(apply) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows an Alert replacing the body when the cluster is down", async () => {
    stubFetch({ "GET /api/cluster/settings": () => jsonResponse({ clusterDown: true, found: false }) });
    render(<AdvancedTab apiBase="/api" running={false} />);
    await waitFor(() => expect(screen.getByText(/desktop cluster isn.t running/i)).toBeInTheDocument());
    expect(screen.queryByLabelText(/cluster settings yaml/i)).not.toBeInTheDocument();
  });

  it("Validate renders the ValidationPanel (errors and violations)", async () => {
    const badValidation: ValidationResult = {
      errors: ["limits.budget.maxTurns: must be >= 1"],
      violations: [{ reason: "AuthzClamped", message: "approval timeout clamped to cluster ceiling", fatal: false }],
    };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(badValidation, 422),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));
    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));

    await waitFor(() => expect(screen.getByText(/must be >= 1/i)).toBeInTheDocument());
    expect(screen.getByText("AuthzClamped")).toBeInTheDocument();
  });

  it("Apply on validated text applies directly with a single click", async () => {
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    const calls = stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () => jsonResponse(cleanValidation),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));
    await waitFor(() => expect(screen.getByText(/effective settings \(preview\)/i)).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    await waitFor(() => expect(screen.getByText(/^applied\.$/i)).toBeInTheDocument());

    const puts = calls.filter((c) => c.method === "PUT" && c.path === "/api/cluster/settings");
    expect(puts).toHaveLength(1);
  });

  it("Apply without validating arms a confirm, then applies on a second click within the window", async () => {
    vi.useFakeTimers();
    const putResp: ClusterSettingsUpdateResponse = { applied: true, validation: cleanValidation };
    const calls = stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(putResp),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await vi.waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    const applyButton = () => screen.getByRole("button", { name: /apply/i });
    expect(applyButton()).toHaveTextContent(/^apply$/i);

    fireEvent.click(applyButton());
    // First click only arms — no PUT yet, label changes to the confirm prompt.
    expect(calls.filter((c) => c.method === "PUT")).toHaveLength(0);
    expect(applyButton()).toHaveTextContent(/apply without validating\?/i);

    await act(() => vi.advanceTimersByTimeAsync(3000));
    fireEvent.click(applyButton());

    await vi.waitFor(() => expect(calls.filter((c) => c.method === "PUT")).toHaveLength(1));
    const put = calls.find((c) => c.method === "PUT" && c.path === "/api/cluster/settings");
    expect((put?.body as { yaml?: string }).yaml).toBe(SEED_YAML);

    vi.useRealTimers();
  });

  it("the confirm disarms after the 10-second window — a later click re-arms instead of applying", async () => {
    vi.useFakeTimers();
    const calls = stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse({ applied: true, validation: cleanValidation }),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await vi.waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    const applyButton = () => screen.getByRole("button", { name: /apply/i });
    fireEvent.click(applyButton());
    expect(applyButton()).toHaveTextContent(/apply without validating\?/i);

    await act(() => vi.advanceTimersByTimeAsync(10_001));
    expect(applyButton()).toHaveTextContent(/^apply$/i);

    fireEvent.click(applyButton());
    expect(calls.filter((c) => c.method === "PUT")).toHaveLength(0);
    expect(applyButton()).toHaveTextContent(/apply without validating\?/i);

    vi.useRealTimers();
  });

  it("a server 400 (multi-document YAML) renders inline, not through the ValidationPanel", async () => {
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "POST /api/cluster/settings/validate": () =>
        textResponse("settings: multi-document YAML is not supported here: only the first document would be applied", 400),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));
    fireEvent.click(screen.getByRole("button", { name: /^validate$/i }));

    await waitFor(() => expect(screen.getByText(/multi-document yaml is not supported/i)).toBeInTheDocument());
    expect(screen.queryByRole("alert", { name: /must be >=/i })).not.toBeInTheDocument();
  });

  it("a 422 apply (validation failure) recovers the structured result and renders it", async () => {
    const failedPut: ClusterSettingsUpdateResponse = {
      applied: false,
      validation: { errors: ["defaults.model.provider: required"], violations: [] },
    };
    stubFetch({
      "GET /api/cluster/settings": () => jsonResponse(foundResp),
      "PUT /api/cluster/settings": () => jsonResponse(failedPut, 422),
    });
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    fireEvent.click(screen.getByRole("button", { name: /apply without validating\?/i }));

    await waitFor(() => expect(screen.getByText(/provider: required/i)).toBeInTheDocument());
    expect(screen.getByText(/not applied/i)).toBeInTheDocument();
  });

  it("drift on Apply renders DriftPanel; take-ownership re-PUTs {yaml, takeOwnership:true}", async () => {
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
    render(<AdvancedTab apiBase="/api" running={true} />);
    await waitFor(() => expect(getTextarea().value).toBe(SEED_YAML));

    fireEvent.click(screen.getByRole("button", { name: /^apply$/i }));
    fireEvent.click(screen.getByRole("button", { name: /apply without validating\?/i }));

    await waitFor(() => expect(screen.getByText("limits.budget.maxTurns")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /take full ownership and re-apply/i }));
    await waitFor(() => expect(screen.queryByText("limits.budget.maxTurns")).not.toBeInTheDocument());

    const puts = calls.filter((c) => c.method === "PUT" && c.path === "/api/cluster/settings");
    expect(puts).toHaveLength(2);
    expect(puts[1].body).toEqual({ yaml: SEED_YAML, takeOwnership: true });
  });
});
