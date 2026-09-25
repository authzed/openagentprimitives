import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { ModelName, parseModelDisplay } from "./model-name";

afterEach(cleanup);

describe("parseModelDisplay", () => {
  it("splits a direct-provider id on the first slash", () => {
    expect(parseModelDisplay("anthropic/claude-opus-4-8")).toEqual({
      provider: "anthropic",
      name: "claude-opus-4-8",
    });
  });

  it("splits a routed OpenRouter id into via + underlying provider/name", () => {
    expect(parseModelDisplay("openrouter/anthropic/claude-3.5-sonnet")).toEqual({
      via: "openrouter",
      provider: "anthropic",
      name: "claude-3.5-sonnet",
    });
  });

  it("mechanically re-applies the split rule to OpenRouter's own auto router", () => {
    // "openrouter/openrouter/auto" is not a special case: the first split
    // yields provider="openrouter" (routed), then the REST ("openrouter/auto")
    // is split the same way, yielding provider="openrouter", name="auto".
    expect(parseModelDisplay("openrouter/openrouter/auto")).toEqual({
      via: "openrouter",
      provider: "openrouter",
      name: "auto",
    });
  });

  it("treats a routed id with no further slash as name-only", () => {
    expect(parseModelDisplay("openrouter/auto")).toEqual({ via: "openrouter", name: "auto" });
  });

  it("returns a plain name when there is no delimiter", () => {
    expect(parseModelDisplay("mystery")).toEqual({ name: "mystery" });
  });

  it("returns an empty name for an empty string", () => {
    expect(parseModelDisplay("")).toEqual({ name: "" });
  });
});

describe("ModelName", () => {
  it("direct provider shows provider badge + model", () => {
    render(<ModelName model="anthropic/claude-opus-4-8" />);
    expect(screen.getByText("anthropic")).toBeTruthy();
    expect(screen.getByText("claude-opus-4-8")).toBeTruthy();
  });

  it("openrouter routed shows OpenRouter + sub-provider + model", () => {
    render(<ModelName model="openrouter/anthropic/claude-3.5-sonnet" />);
    expect(screen.getByText("openrouter")).toBeTruthy();
    expect(screen.getByText("anthropic")).toBeTruthy();
    expect(screen.getByText("claude-3.5-sonnet")).toBeTruthy();
  });

  it("no delimiter renders plain", () => {
    render(<ModelName model="mystery" />);
    expect(screen.getByText("mystery")).toBeTruthy();
  });

  it("empty model renders nothing", () => {
    const { container } = render(<ModelName model="" />);
    expect(container.firstChild).toBeNull();
  });
});
