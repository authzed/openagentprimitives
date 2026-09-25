import "@testing-library/jest-dom/vitest";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach, beforeAll } from "vitest";
import { CatalogSection } from "./CatalogSection";
import type { SettingsSpec } from "./spec";
import type { TokenWrite } from "../lib/api";

// Radix Select opens/selects via pointer events jsdom doesn't implement
// (hasPointerCapture, scrollIntoView). A plain fireEvent.click still fires
// Select's onClick handlers (Radix's internal pointerTypeRef defaults to
// "touch", and the onClick branch runs whenever it isn't "mouse"), so a
// click-only interaction works once these two are stubbed so nothing throws.
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

const baseSpec: SettingsSpec = {
  modelCatalog: [
    {
      name: "claude-opus",
      provider: "anthropic",
      default: true,
      inputPerMTok: 15,
      outputPerMTok: 75,
      tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-claude-opus", key: "token" },
    },
    {
      name: "gpt-x",
      provider: "openai",
      tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-gpt-x", key: "token" },
    },
  ],
};

describe("CatalogSection", () => {
  it("renders a table row per catalog entry with name/provider/default/prices", () => {
    render(<CatalogSection spec={baseSpec} onChange={vi.fn()} tokens={[]} onTokensChange={vi.fn()} />);
    expect(screen.getByText("claude-opus")).toBeInTheDocument();
    expect(screen.getByText("gpt-x")).toBeInTheDocument();
    expect(screen.getAllByText("anthropic").length).toBeGreaterThan(0);
    expect(screen.getByText(/15/)).toBeInTheDocument();
    expect(screen.getByText(/75/)).toBeInTheDocument();
  });

  it("adding a model emits onChange with the new entry appended and the default tokenRef", async () => {
    const onChange = vi.fn();
    render(<CatalogSection spec={baseSpec} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);

    fireEvent.click(screen.getByRole("button", { name: /add model/i }));
    fireEvent.change(await screen.findByLabelText(/^name$/i), { target: { value: "gpt-new" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(onChange).toHaveBeenCalled());
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    const added = (next.modelCatalog ?? []).find((e) => e.name === "gpt-new");
    expect(added).toBeTruthy();
    expect(added?.tokenRef).toEqual({
      namespace: "agentprimitives-system",
      name: "model-default-token-gpt-new",
      key: "token",
    });
    // unedited entries survive the round trip untouched
    expect((next.modelCatalog ?? []).find((e) => e.name === "claude-opus")).toEqual(baseSpec.modelCatalog![0]);
    expect((next.modelCatalog ?? []).find((e) => e.name === "gpt-x")).toEqual(baseSpec.modelCatalog![1]);
  });

  it("typing a token value on add emits onTokensChange targeting the new entry's default tokenRef", async () => {
    const onTokensChange = vi.fn();
    render(
      <CatalogSection spec={baseSpec} onChange={vi.fn()} tokens={[]} onTokensChange={onTokensChange} />,
    );

    fireEvent.click(screen.getByRole("button", { name: /add model/i }));
    fireEvent.change(await screen.findByLabelText(/^name$/i), { target: { value: "gpt-new" } });
    fireEvent.change(screen.getByLabelText(/api token/i), { target: { value: "sk-newtoken" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(onTokensChange).toHaveBeenCalled());
    const tokens = onTokensChange.mock.calls[0][0] as TokenWrite[];
    expect(tokens).toEqual([
      { namespace: "agentprimitives-system", name: "model-default-token-gpt-new", key: "token", value: "sk-newtoken" },
    ]);
  });

  it("leaving the token field blank on add emits no tokens[] element", async () => {
    const onTokensChange = vi.fn();
    render(
      <CatalogSection spec={baseSpec} onChange={vi.fn()} tokens={[]} onTokensChange={onTokensChange} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /add model/i }));
    fireEvent.change(await screen.findByLabelText(/^name$/i), { target: { value: "gpt-new" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(onTokensChange).not.toHaveBeenCalled();
  });

  it("the token field is always rendered empty (write-only), never pre-filled on edit", async () => {
    render(<CatalogSection spec={baseSpec} onChange={vi.fn()} tokens={[]} onTokensChange={vi.fn()} />);
    fireEvent.click(screen.getAllByRole("button", { name: /^edit$/i })[0]);
    const tokenInput = (await screen.findByLabelText(/api token/i)) as HTMLInputElement;
    expect(tokenInput.value).toBe("");
    expect(tokenInput.type).toBe("password");
    expect(tokenInput.placeholder).toMatch(/unchanged/i);
  });

  it("editing an entry preserves its existing tokenRef even when a new token value is typed", async () => {
    const onChange = vi.fn();
    render(<CatalogSection spec={baseSpec} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);
    fireEvent.click(screen.getAllByRole("button", { name: /^edit$/i })[1]); // gpt-x
    fireEvent.change(screen.getByLabelText(/output \$\/mtok/i), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(onChange).toHaveBeenCalled());
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    const edited = (next.modelCatalog ?? []).find((e) => e.name === "gpt-x");
    expect(edited?.outputPerMTok).toBe(30);
    expect(edited?.tokenRef).toEqual(baseSpec.modelCatalog![1].tokenRef);
  });

  it("editing an entry preserves non-curated fields (routing) the dialog never surfaces", async () => {
    // routing is settable only via the raw YAML tab; an edit here (e.g. a
    // price tweak) must round-trip it untouched, not erase it.
    const withRouting: SettingsSpec = {
      modelCatalog: [
        { ...baseSpec.modelCatalog![0] },
        { ...baseSpec.modelCatalog![1], routing: { sort: "price" } },
      ],
    };
    const onChange = vi.fn();
    render(<CatalogSection spec={withRouting} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);
    fireEvent.click(screen.getAllByRole("button", { name: /^edit$/i })[1]); // gpt-x
    fireEvent.change(await screen.findByLabelText(/output \$\/mtok/i), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(onChange).toHaveBeenCalled());
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    const edited = (next.modelCatalog ?? []).find((e) => e.name === "gpt-x");
    expect(edited?.outputPerMTok).toBe(30);
    expect(edited?.routing).toEqual({ sort: "price" });
  });

  it("setting default on one entry's dialog clears every other entry's default (exclusivity)", async () => {
    const onChange = vi.fn();
    render(<CatalogSection spec={baseSpec} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);
    fireEvent.click(screen.getAllByRole("button", { name: /^edit$/i })[1]); // gpt-x, currently not default
    fireEvent.click(await screen.findByLabelText(/default model/i));
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(onChange).toHaveBeenCalled());
    const next = onChange.mock.calls[0][0] as SettingsSpec;
    const byName = new Map((next.modelCatalog ?? []).map((e) => [e.name, e.default]));
    expect(byName.get("gpt-x")).toBe(true);
    expect(byName.get("claude-opus")).toBe(false);
  });

  it("delete requires confirmation before onChange is called", () => {
    const onChange = vi.fn();
    render(<CatalogSection spec={baseSpec} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);

    fireEvent.click(screen.getAllByRole("button", { name: /^delete$/i })[1]); // gpt-x
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /confirm delete/i })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(screen.queryByRole("button", { name: /confirm delete/i })).not.toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
  });

  it("confirming delete emits onChange with the entry removed and siblings intact", () => {
    const onChange = vi.fn();
    render(<CatalogSection spec={baseSpec} onChange={onChange} tokens={[]} onTokensChange={vi.fn()} />);

    fireEvent.click(screen.getAllByRole("button", { name: /^delete$/i })[1]); // gpt-x
    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i }));

    expect(onChange).toHaveBeenCalledWith({ modelCatalog: [baseSpec.modelCatalog![0]] });
  });

  it("deleting an entry drops any pending token write staged for it (no orphan Secret)", () => {
    const onTokensChange = vi.fn();
    const pending: TokenWrite[] = [
      { namespace: "agentprimitives-system", name: "model-default-token-gpt-x", key: "token", value: "sk-x" },
      { namespace: "agentprimitives-system", name: "model-default-token-claude-opus", key: "token", value: "sk-keep" },
    ];
    render(<CatalogSection spec={baseSpec} onChange={vi.fn()} tokens={pending} onTokensChange={onTokensChange} />);

    fireEvent.click(screen.getAllByRole("button", { name: /^delete$/i })[1]); // gpt-x
    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i }));

    // gpt-x's staged token is dropped; the unrelated one survives.
    expect(onTokensChange).toHaveBeenCalledWith([
      { namespace: "agentprimitives-system", name: "model-default-token-claude-opus", key: "token", value: "sk-keep" },
    ]);
  });
});
