import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { ModelTab } from "./ModelTab";
import type { ConfigResponse } from "../lib/api";

const baseConfig: ConfigResponse = {
  model: { provider: "anthropic", apiKeyMasked: "••••7890", name: "" },
  providers: ["anthropic", "openai", "openrouter"],
  ngrokConfigured: false,
  healthNotifications: true,
  passwordSet: true,
  configPath: "/tmp/support/config.json",
};

const fullSecretKey = "sk-super-secret-1234567890abcdef";

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

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ModelTab", () => {
  it("shows the masked key from the server, never the full key, in the rendered DOM", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    const { container } = render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/••••7890/)).toBeTruthy());
    expect(container.innerHTML).not.toContain(fullSecretKey);
  });

  it("leaves the API-key replacement field empty with an 'unchanged' placeholder", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByLabelText(/^api key$/i)).toBeTruthy());
    const input = screen.getByLabelText(/^api key$/i) as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.type).toBe("password");
    expect(input.placeholder).toMatch(/unchanged/i);
  });

  it("shows a per-provider default-model hint mentioning the selected provider", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/leave blank to use anthropic/i)).toBeTruthy());
  });

  it("shows ngrok as not configured when the server reports ngrokConfigured=false", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/not configured/i)).toBeTruthy());
  });

  it("shows ngrok as configured when the server reports ngrokConfigured=true", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse({ ...baseConfig, ngrokConfigured: true }) });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/^configured$/i)).toBeTruthy());
  });

  it("notes that changes apply at next start when the cluster isn't running", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<ModelTab apiBase="/api" running={false} />);
    await waitFor(() => expect(screen.getByText(/changes apply at the next start/i)).toBeTruthy());
  });

  it("does not show the next-start note when the cluster is running", async () => {
    stubFetch({ "GET /api/config": () => jsonResponse(baseConfig) });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/••••7890/)).toBeTruthy());
    expect(screen.queryByText(/changes apply at the next start/i)).toBeNull();
  });

  it("renders outcomes from a successful save", async () => {
    stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "PUT /api/config": () => jsonResponse({ outcomes: [{ field: "model", outcome: "applied" }] }),
    });
    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/••••7890/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await waitFor(() => expect(screen.getByText(/^applied$/i)).toBeTruthy());
  });

  it("sends the replacement API key only when the user typed one", async () => {
    let putBody: string | undefined;
    stubFetch({
      "GET /api/config": () => jsonResponse(baseConfig),
      "PUT /api/config": () => jsonResponse({ outcomes: [] }),
    });
    // Intercept body separately since the shared stub doesn't expose init.body.
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      const path = new URL(String(input), "http://settings.local").pathname;
      if (method === "GET" && path === "/api/config") return jsonResponse(baseConfig);
      if (method === "PUT" && path === "/api/config") {
        putBody = String(init?.body ?? "");
        return jsonResponse({ outcomes: [] });
      }
      throw new Error(`unstubbed fetch: ${method} ${path}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<ModelTab apiBase="/api" running={true} />);
    await waitFor(() => expect(screen.getByText(/••••7890/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /save/i }));
    await waitFor(() => expect(putBody).toBeTruthy());
    expect(JSON.parse(putBody!).model.apiKey).toBe("");
  });
});
