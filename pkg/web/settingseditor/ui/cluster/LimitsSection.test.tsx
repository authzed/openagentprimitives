import "@testing-library/jest-dom/vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach, beforeAll } from "vitest";
import { LimitsSection } from "./LimitsSection";
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

// fixtureSpec covers every curated Limits field this section renders.
const fixtureSpec: SettingsSpec = {
  limits: {
    budget: { maxTurns: 20, maxTokens: 500000, maxDuration: "2h", sessionExpiration: "24h", maxDelegatedAgents: 3 },
    authz: { maxApprovalTimeout: "10m", maxInformationLeakageApprovalTTL: "1h" },
    minPlanGateMode: "logging",
    deniedModels: ["gpt-3.5"],
    deniedSkills: ["repo//bad@*"],
    allowModelOverride: true,
    nativeFileHandling: false,
    // requireSubagentDigestPins omitted: unset tri-state
    allowedToolkits: ["toolkit-a"],
    allowedSkills: [], // deny-all
    // allowedSandboxKinds omitted: unrestricted
    pinning: { rules: [{ kind: "skill", minStrength: "frozen", mode: "block" }] },
    toolGuard: { maxFailureThreshold: 5 },
    contentInspectors: [{ id: "url-allowlist" }],
    allowedMCPServers: [{ name: "github" }],
  },
};

describe("LimitsSection", () => {
  it("renders every curated budget/authz field from the fixture", () => {
    render(<LimitsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect((screen.getByLabelText(/^ceiling: max turns$/i) as HTMLInputElement).value).toBe("20");
    expect((screen.getByLabelText(/^ceiling: max tokens$/i) as HTMLInputElement).value).toBe("500000");
    expect((screen.getByLabelText(/^ceiling: max duration$/i) as HTMLInputElement).value).toBe("2h");
    expect((screen.getByLabelText(/^ceiling: session expiration$/i) as HTMLInputElement).value).toBe("24h");
    expect((screen.getByLabelText(/^ceiling: max delegated agents$/i) as HTMLInputElement).value).toBe("3");
    expect((screen.getByLabelText(/^ceiling: max approval timeout$/i) as HTMLInputElement).value).toBe("10m");
    expect((screen.getByLabelText(/leakage approval ttl/i) as HTMLInputElement).value).toBe("1h");
  });

  it("renders the denied-model/skill lists as newline text", () => {
    render(<LimitsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect((screen.getByLabelText(/^denied models$/i) as HTMLTextAreaElement).value).toBe("gpt-3.5");
    expect((screen.getByLabelText(/^denied skills$/i) as HTMLTextAreaElement).value).toBe("repo//bad@*");
  });

  it("renders the summary cards, mirroring admind's flattening", () => {
    render(<LimitsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect(screen.getByText("skill(min=frozen,mode=block)")).toBeInTheDocument();
    expect(screen.getByText("maxFailureThreshold=5")).toBeInTheDocument();
    expect(screen.getByText("url-allowlist")).toBeInTheDocument();
    expect(screen.getByText("github")).toBeInTheDocument();
    expect(screen.getAllByText(/edit in advanced tab/i).length).toBe(4);
  });

  it("shows the deny-all warning for an allowlist that is present but empty", () => {
    render(<LimitsSection spec={fixtureSpec} onChange={vi.fn()} />);
    expect(screen.getByText(/deny-all: no entries are permitted/i)).toBeInTheDocument();
  });

  it("editing max turns emits the updated spec", () => {
    const onChange = vi.fn();
    render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^ceiling: max turns$/i), { target: { value: "42" } });
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ limits: expect.objectContaining({ budget: expect.objectContaining({ maxTurns: 42 }) }) }),
    );
  });

  it("clearing a duration field deletes the key", () => {
    const onChange = vi.fn();
    render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^ceiling: max duration$/i), { target: { value: "" } });
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("maxDuration" in (next.limits?.budget ?? {})).toBe(false);
  });

  it("changing minPlanGateMode to enforcing emits it, and back to unset deletes the key", async () => {
    const onChange = vi.fn();
    const { rerender } = render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /minimum plan-gate mode/i }));
    fireEvent.click(await screen.findByRole("option", { name: "enforcing" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ limits: expect.objectContaining({ minPlanGateMode: "enforcing" }) }),
    );

    onChange.mockClear();
    const unsetSpec: SettingsSpec = { limits: { minPlanGateMode: "enforcing" } };
    rerender(<LimitsSection spec={unsetSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /minimum plan-gate mode/i }));
    fireEvent.click(await screen.findByRole("option", { name: /unset/i }));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("minPlanGateMode" in (next.limits ?? {})).toBe(false);
  });

  it("editing the denied-models textarea emits the parsed array, and clearing it deletes the key", () => {
    const onChange = vi.fn();
    const { rerender } = render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^denied models$/i), { target: { value: "gpt-3.5\ngpt-4-legacy" } });
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({
        limits: expect.objectContaining({ deniedModels: ["gpt-3.5", "gpt-4-legacy"] }),
      }),
    );

    onChange.mockClear();
    rerender(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(/^denied models$/i), { target: { value: "" } });
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("deniedModels" in (next.limits ?? {})).toBe(false);
  });

  it("bool tri-state select round-trips unset -> true -> false -> unset", async () => {
    const onChange = vi.fn();
    const unset: SettingsSpec = {};
    const { rerender } = render(<LimitsSection spec={unset} onChange={onChange} />);

    expect(screen.getByRole("combobox", { name: /native file handling/i })).toHaveTextContent(/unset/i);

    fireEvent.click(screen.getByRole("combobox", { name: /native file handling/i }));
    fireEvent.click(await screen.findByRole("option", { name: "True" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ limits: expect.objectContaining({ nativeFileHandling: true }) }),
    );

    onChange.mockClear();
    const trueSpec: SettingsSpec = { limits: { nativeFileHandling: true } };
    rerender(<LimitsSection spec={trueSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /native file handling/i }));
    fireEvent.click(await screen.findByRole("option", { name: "False" }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ limits: expect.objectContaining({ nativeFileHandling: false }) }),
    );

    onChange.mockClear();
    const falseSpec: SettingsSpec = { limits: { nativeFileHandling: false } };
    rerender(<LimitsSection spec={falseSpec} onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: /native file handling/i }));
    fireEvent.click(await screen.findByRole("option", { name: /unset/i }));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("nativeFileHandling" in (next.limits ?? {})).toBe(false);
  });

  it("allowed-toolkits radio starts on Restrict to list when the fixture has entries, and switching to Unrestricted deletes the key", () => {
    const onChange = vi.fn();
    render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    expect(screen.getByLabelText(/allowed toolkits: restrict to list/i)).toBeChecked();

    fireEvent.click(screen.getByLabelText(/allowed toolkits: unrestricted/i));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect("allowedToolkits" in (next.limits ?? {})).toBe(false);
  });

  it("switching allowed-sandbox-kinds from Unrestricted to Restrict to list sets an empty (deny-all) array", () => {
    const onChange = vi.fn();
    render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    expect(screen.getByLabelText(/allowed sandbox kinds: unrestricted/i)).toBeChecked();

    fireEvent.click(screen.getByLabelText(/allowed sandbox kinds: restrict to list/i));
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect(next.limits?.allowedSandboxKinds).toEqual([]);
  });

  it("typing into a restricted allowlist's textarea emits the parsed items", () => {
    const onChange = vi.fn();
    const { container } = render(<LimitsSection spec={fixtureSpec} onChange={onChange} />);
    const textarea = container.querySelector<HTMLTextAreaElement>("#limits-allowed-toolkits-list")!;
    expect(textarea).toBeTruthy();
    fireEvent.change(textarea, { target: { value: "toolkit-a\ntoolkit-b" } });
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    expect(next.limits?.allowedToolkits).toEqual(["toolkit-a", "toolkit-b"]);
  });
});
