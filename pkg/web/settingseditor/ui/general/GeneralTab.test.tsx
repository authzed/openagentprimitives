import "@testing-library/jest-dom/vitest";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { GeneralTab } from "./GeneralTab";
import type { ConfigResponse } from "../lib/api";

const baseConfig: ConfigResponse = {
  model: { provider: "anthropic", apiKeyMasked: "••••7890", name: "" },
  providers: ["anthropic", "openai", "openrouter"],
  ngrokConfigured: false,
  healthNotifications: true,
  passwordSet: true,
  configPath: "/tmp/support/config.json",
};

// stubFetch dispatches on "METHOD /path" so a test can supply per-route
// handlers without caring about fetch's exact call signature.
function stubFetch(handlers: Record<string, () => Response>) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      const path = new URL(String(input), "http://settings.local").pathname;
      const key = `${method} ${path}`;
      calls.push(key);
      const handler = handlers[key];
      if (!handler) throw new Error(`unstubbed fetch: ${key}`);
      return handler();
    }),
  );
  return calls;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function textResponse(body: string, status: number): Response {
  return new Response(body, { status });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("GeneralTab", () => {
  it("loads the health-notifications checkbox from GET /api/config", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByLabelText(/notify on cluster health issues/i)).toBeTruthy());
    const checkbox = screen.getByLabelText(/notify on cluster health issues/i) as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
  });

  it("always shows the minimum-length hint next to the new-password field", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByText(/at least 8 characters/i)).toBeTruthy());
  });

  it("blocks the save client-side when new password and confirmation don't match, without calling PUT", async () => {
    const calls = stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByLabelText(/notify on cluster health issues/i)).toBeTruthy());

    fireEvent.change(screen.getByLabelText(/^current password$/i), { target: { value: "correcthorse" } });
    fireEvent.change(screen.getByLabelText(/^new password$/i), { target: { value: "newpassword123" } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: "somethingelse123" } });
    fireEvent.click(screen.getByRole("button", { name: /save general settings/i }));

    await waitFor(() => expect(screen.getByText(/do not match/i)).toBeTruthy());
    expect(calls.filter((c) => c === "PUT /api/config")).toHaveLength(0);
  });

  it("renders the server's 403 message inline when the current password is wrong", async () => {
    stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "PUT /api/config": () => textResponse("settings: current password is incorrect", 403),
    });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByLabelText(/notify on cluster health issues/i)).toBeTruthy());

    fireEvent.change(screen.getByLabelText(/^current password$/i), { target: { value: "wrongpass" } });
    fireEvent.change(screen.getByLabelText(/^new password$/i), { target: { value: "newpassword123" } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: "newpassword123" } });
    fireEvent.click(screen.getByRole("button", { name: /save general settings/i }));

    await waitFor(() => expect(screen.getByText(/current password is incorrect/i)).toBeTruthy());
  });

  it("renders applied/next-start/failed outcomes from a successful PUT", async () => {
    stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "PUT /api/config": () =>
        jsonResponse({
          outcomes: [
            { field: "healthNotifications", outcome: "applied" },
            { field: "password", outcome: "next-start" },
            { field: "model", outcome: "failed", message: "apply failed: cluster unreachable" },
          ],
        }),
    });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByLabelText(/notify on cluster health issues/i)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /save general settings/i }));

    await waitFor(() => expect(screen.getByText(/applies at next start/i)).toBeTruthy());
    expect(screen.getByText(/^applied$/i)).toBeTruthy();
    expect(screen.getByText(/^failed$/i)).toBeTruthy();
    expect(screen.getByText(/cluster unreachable/i)).toBeTruthy();
  });

  it("shows the kubectl status line from the kubectlCurrent prop", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={true} />);
    await waitFor(() => expect(screen.getByText(/kubectl is pointing at this cluster/i)).toBeTruthy());
  });

  it("disables the kubectl button while the cluster is stopped, enables it while running", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    const { rerender } = render(<GeneralTab apiBase="/api" running={false} kubectlCurrent={false} />);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /point kubectl at this cluster/i })).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: /point kubectl at this cluster/i })).toBeDisabled();

    rerender(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /point kubectl at this cluster/i })).not.toBeDisabled(),
    );
  });

  it("invokes POST /api/kubectl/use when the kubectl button is clicked while running", async () => {
    const calls = stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "POST /api/kubectl/use": () => new Response(null, { status: 204 }),
    });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /point kubectl at this cluster/i })).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: /point kubectl at this cluster/i }));
    await waitFor(() => expect(calls).toContain("POST /api/kubectl/use"));
  });

  it("invokes the reveal routes from their respective buttons", async () => {
    const calls = stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "POST /api/reveal/config": () => new Response(null, { status: 204 }),
      "POST /api/reveal/logs": () => new Response(null, { status: 204 }),
    });
    render(<GeneralTab apiBase="/api" running={true} kubectlCurrent={false} />);
    await waitFor(() => expect(screen.getByRole("button", { name: /reveal config file/i })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /reveal config file/i }));
    await waitFor(() => expect(calls).toContain("POST /api/reveal/config"));

    fireEvent.click(screen.getByRole("button", { name: /view logs/i }));
    await waitFor(() => expect(calls).toContain("POST /api/reveal/logs"));
  });
});
