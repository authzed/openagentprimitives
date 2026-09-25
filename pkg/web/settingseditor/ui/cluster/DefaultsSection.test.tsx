import "@testing-library/jest-dom/vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach, beforeAll } from "vitest";
import { DefaultsSection } from "./DefaultsSection";
import type { SettingsSpec } from "./spec";

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

afterEach(() => cleanup());

// fixtureSpec covers every curated Defaults field this section renders,
// plus a modelCatalog so the fromCatalog Select has real options.
const fixtureSpec: SettingsSpec = {
  modelCatalog: [{ name: "claude-opus", provider: "anthropic" }],
  defaults: {
    model: { provider: "anthropic", name: "claude-opus-4", fromCatalog: "claude-opus" },
    budget: { maxTurns: 15, maxTokens: 200000, maxDuration: "1h", sessionExpiration: "12h", maxDelegatedAgents: 2 },
    authz: {
      approvalTimeout: "5m",
      informationLeakageApprovalTTL: "30m",
      scopeMaxLlmLatencyMs: 400,
      planGate: { mode: "logging" },
      metaagent: { trigger: "mention" },
    },
    reportSessionCost: false,
    sandbox: { kind: "agent-sandbox" },
  },
};

describe("DefaultsSection", () => {
  it("renders every curated field from the fixture", () => {
    render(<DefaultsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect((screen.getByLabelText(/^model name$/i) as HTMLInputElement).value).toBe("claude-opus-4");
    expect((screen.getByLabelText(/^default: max turns$/i) as HTMLInputElement).value).toBe("15");
    expect((screen.getByLabelText(/^default: max tokens$/i) as HTMLInputElement).value).toBe("200000");
    expect((screen.getByLabelText(/^default: max duration$/i) as HTMLInputElement).value).toBe("1h");
    expect((screen.getByLabelText(/^default: session expiration$/i) as HTMLInputElement).value).toBe("12h");
    expect((screen.getByLabelText(/^default: max delegated agents$/i) as HTMLInputElement).value).toBe("2");
    expect((screen.getByLabelText(/^default: approval timeout$/i) as HTMLInputElement).value).toBe("5m");
    expect((screen.getByLabelText(/leakage approval ttl/i) as HTMLInputElement).value).toBe("30m");
    expect((screen.getByLabelText(/scope max llm latency/i) as HTMLInputElement).value).toBe("400");
    expect((screen.getByLabelText(/^sandbox kind$/i) as HTMLInputElement).value).toBe("agent-sandbox");
  });

  it("shows the planGate/metaagent summary-only lines and an Advanced-tab note", () => {
    render(<DefaultsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect(screen.getByText(/plan-gate mode: logging/i)).toBeInTheDocument();
    expect(screen.getByText(/metaagent trigger: mention/i)).toBeInTheDocument();
    expect(screen.getByText(/edit in advanced tab/i)).toBeInTheDocument();
  });

  it("populates the fromCatalog select from spec.modelCatalog", () => {
    render(<DefaultsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect(screen.getByRole("combobox", { name: /from catalog/i })).toHaveTextContent("claude-opus");
  });

  it("editing model name emits the updated spec", () => {
    const onChange = vi.fn();
    render(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^model name$/i), { target: { value: "claude-opus-5" } });
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ model: expect.objectContaining({ name: "claude-opus-5" }) }) }),
    );
  });

  it("shows a placeholder (not a false default) for provider when defaults.model is entirely unset", () => {
    render(<DefaultsSection spec={{}} onChange={vi.fn()} />);
    expect(screen.getByRole("combobox", { name: /^provider$/i })).toHaveTextContent(/select a provider/i);
  });

  it("editing model name while provider is unset never silently sets a provider", () => {
    const onChange = vi.fn();
    render(<DefaultsSection spec={{}} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^model name$/i), { target: { value: "claude-opus-5" } });
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect(next.defaults?.model?.name).toBe("claude-opus-5");
    expect("provider" in (next.defaults?.model ?? {})).toBe(false);
  });

  it("changing the provider select emits the updated spec", async () => {
    const onChange = vi.fn();
    render(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /^provider$/i }));
    fireEvent.click(await screen.findByRole("option", { name: "openai" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ model: expect.objectContaining({ provider: "openai" }) }) }),
    );
  });

  it("selecting a catalog entry sets fromCatalog, and selecting 'use provider/name above' deletes it", async () => {
    const onChange = vi.fn();
    const { rerender } = render(
      <DefaultsSection spec={{ ...fixtureSpec, defaults: { ...fixtureSpec.defaults, model: { provider: "anthropic", name: "x" } } }} onChange={onChange} />,
    );
    fireEvent.click(screen.getByRole("combobox", { name: /from catalog/i }));
    fireEvent.click(await screen.findByRole("option", { name: "claude-opus" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ model: expect.objectContaining({ fromCatalog: "claude-opus" }) }) }),
    );

    onChange.mockClear();
    rerender(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /from catalog/i }));
    fireEvent.click(await screen.findByRole("option", { name: /use provider\/name above/i }));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("fromCatalog" in (next.defaults?.model ?? {})).toBe(false);
  });

  it("editing a budget/authz field emits the updated spec, and clearing a duration deletes the key", () => {
    const onChange = vi.fn();
    const { rerender } = render(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^default: max turns$/i), { target: { value: "99" } });
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ budget: expect.objectContaining({ maxTurns: 99 }) }) }),
    );

    onChange.mockClear();
    rerender(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^default: max duration$/i), { target: { value: "" } });
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("maxDuration" in (next.defaults?.budget ?? {})).toBe(false);
  });

  it("reportSessionCost tri-state round-trips unset -> false -> true -> unset, and shows the on-by-default note when unset", async () => {
    const onChange = vi.fn();
    const unset: SettingsSpec = {};
    const { rerender } = render(<DefaultsSection spec={unset} onChange={onChange} />);

    expect(screen.getByTestId("report-session-cost-default-note")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("combobox", { name: /report session cost/i }));
    fireEvent.click(await screen.findByRole("option", { name: "False" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ reportSessionCost: false }) }),
    );

    onChange.mockClear();
    const falseSpec: SettingsSpec = { defaults: { reportSessionCost: false } };
    rerender(<DefaultsSection spec={falseSpec} onChange={onChange} />);
    expect(screen.queryByTestId("report-session-cost-default-note")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("combobox", { name: /report session cost/i }));
    fireEvent.click(await screen.findByRole("option", { name: "True" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ reportSessionCost: true }) }),
    );

    onChange.mockClear();
    const trueSpec: SettingsSpec = { defaults: { reportSessionCost: true } };
    rerender(<DefaultsSection spec={trueSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /report session cost/i }));
    fireEvent.click(await screen.findByRole("option", { name: /unset/i }));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("reportSessionCost" in (next.defaults ?? {})).toBe(false);
  });

  it("editing sandbox kind emits the updated spec", () => {
    const onChange = vi.fn();
    render(<DefaultsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^sandbox kind$/i), { target: { value: "pod" } });
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ defaults: expect.objectContaining({ sandbox: expect.objectContaining({ kind: "pod" }) }) }),
    );
  });
});
