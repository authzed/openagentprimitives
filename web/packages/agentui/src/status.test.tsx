import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { renderNode } from "./renderNode";

afterEach(cleanup);

describe("ap:status", () => {
  it.each([
    ["idle", "Not started"],
    ["running", "Running since"],
    ["paused", "Paused at"],
    ["ended", "Ended at"],
    ["unwatched", "Still running; I stopped watching at"],
  ])("renders state %s with its text and a matching dot", (state, text) => {
    render(renderNode({ component: "ap:status", props: { state, text, since: "2026-09-13T12:52:00Z" } }));
    const el = screen.getByTestId("ap-status");
    expect(el).toHaveAttribute("data-state", state);
    expect(el).toHaveTextContent(text);
    expect(el).toHaveAttribute("role", "status");
  });

  // The text says WHAT is true; `since` says when, and only the browser knows
  // which clock the person reads. A moment localized anywhere else is the
  // wrong time for every viewer outside that zone.
  it("draws the moment in the viewer's own clock, keeping the moment itself on the element", () => {
    const since = "2026-09-13T12:52:00Z";
    render(renderNode({ component: "ap:status", props: { state: "running", text: "Running since", since } }));
    const el = screen.getByTestId("ap-status");
    const time = el.querySelector("time") as HTMLTimeElement;
    expect(time).not.toBeNull();
    expect(time).toHaveAttribute("dateTime", since);
    expect(time.textContent).toMatch(/\d{1,2}:\d{2}/);
    expect(el).toHaveAttribute("title", since);
    // A space, not a run-on: "Running since 08:52", never "Running since08:52".
    expect(el.textContent).toMatch(/Running since \d{1,2}:\d{2}/);
  });

  it("draws no time for a since that is not a moment: there is nothing to localize", () => {
    render(renderNode({ component: "ap:status", props: { state: "running", text: "Running since", since: "12:52" } }));
    const el = screen.getByTestId("ap-status");
    expect(el.querySelector("time")).toBeNull();
    expect(el).toHaveTextContent("Running since");
  });
});
