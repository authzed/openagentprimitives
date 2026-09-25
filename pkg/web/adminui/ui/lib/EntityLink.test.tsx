import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { EntityLink, ViewLink } from "./EntityLink";

afterEach(() => cleanup());
beforeEach(() => window.history.pushState({}, "", "/admin"));

describe("EntityLink", () => {
  it("renders a real <a> whose href is the detail buildPath (middle-click / open-in-new-tab work)", () => {
    render(<EntityLink entity="agent" id="default/x">go</EntityLink>);
    const a = screen.getByRole("link", { name: "go" });
    expect(a.getAttribute("href")).toBe("/admin/agent/default/x");
  });

  it("plain left-click: preventDefault + client-navigate, updating location without a full reload", () => {
    render(<EntityLink entity="session" id="ns/name" tab="activity">open</EntityLink>);
    const a = screen.getByRole("link", { name: "open" });
    const notPrevented = fireEvent.click(a);
    expect(notPrevented).toBe(false); // default WAS prevented → browser did not follow href
    expect(window.location.pathname).toBe("/admin/session/ns/name");
    expect(window.location.search).toBe("?tab=activity");
  });

  it("modifier (meta) click: does NOT preventDefault or client-navigate — the browser opens the href", () => {
    render(<EntityLink entity="agent" id="default/x">go</EntityLink>);
    const a = screen.getByRole("link", { name: "go" });
    const notPrevented = fireEvent.click(a, { metaKey: true });
    expect(notPrevented).toBe(true); // default preserved → open-in-new-tab works
    expect(window.location.pathname).toBe("/admin"); // unchanged
  });

  it("passes className through to the anchor", () => {
    render(<EntityLink entity="tool" id="t1" className="text-primary">t</EntityLink>);
    expect(screen.getByRole("link", { name: "t" }).className).toContain("text-primary");
  });
});

describe("ViewLink", () => {
  it("renders a view href and left-click navigates to the view", () => {
    render(<ViewLink view="logs">Logs</ViewLink>);
    const a = screen.getByRole("link", { name: "Logs" });
    expect(a.getAttribute("href")).toBe("/admin/logs");
    const notPrevented = fireEvent.click(a);
    expect(notPrevented).toBe(false);
    expect(window.location.pathname).toBe("/admin/logs");
  });
});
