import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { renderNode } from "./renderNode";

afterEach(cleanup);

describe("ap:progress", () => {
  it("renders now, done and next", () => {
    render(renderNode({ component: "ap:progress", props: { now: "Connecting GitHub", done: ["Understood the brief"], next: ["Set permissions", "Test"], step: "Tools" } }));
    expect(screen.getByTestId("ap-progress-now")).toHaveTextContent("Connecting GitHub");
    expect(screen.getByText("Understood the brief").closest("li")).toHaveAttribute("data-state", "done");
    expect(screen.getByText("Set permissions").closest("li")).toHaveAttribute("data-state", "next");
    expect(screen.getByText("Tools")).toBeInTheDocument();
  });
  it("renders with only now", () => {
    render(renderNode({ component: "ap:progress", props: { now: "Thinking" } }));
    expect(screen.getByTestId("ap-progress-now")).toHaveTextContent("Thinking");
  });

  // `now` is Bindable (components.go), and a binding's value arrives from a
  // live source with no idea the prop was typed `string` in Go — a source
  // answering a bare number is the ordinary case, not a malformed one. See
  // registry.tsx's `s` for the established fix for this exact bug (ap:metric
  // rendering blank for a numeric bound value with no signal anything had
  // been dropped).
  it("coerces a bound numeric `now` instead of rendering it blank", () => {
    render(renderNode({ component: "ap:progress", props: { now: 42 } }));
    expect(screen.getByTestId("ap-progress-now")).toHaveTextContent("42");
  });
});
