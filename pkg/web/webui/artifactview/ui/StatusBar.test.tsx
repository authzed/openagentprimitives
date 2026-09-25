import * as React from "react";
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { StatusBar, stateText } from "./StatusBar";

afterEach(cleanup);

describe("stateText", () => {
  it("maps pause causes to labels", () => {
    expect(stateText(false, "")).toBe("Working");
    expect(stateText(true, "awaiting_approval")).toBe("Awaiting approval");
    expect(stateText(true, "awaiting_reply")).toBe("Awaiting your reply");
    expect(stateText(true, "failed")).toBe("Failed");
    expect(stateText(true, "complete")).toBe("Done");
    expect(stateText(true, "weird")).toBe("Paused");
  });
});

describe("StatusBar", () => {
  it("renders nothing when status is null", () => {
    const { container } = render(<StatusBar status={null} />);
    expect(container.textContent).toBe("");
  });
  it("renders the state label and current step", () => {
    render(<StatusBar status={{ phase: "Running", paused: false, pauseCause: "",
      plan: [{ label: "Step A", status: "in_progress" }], statusMessage: "halfway" }} />);
    expect(screen.getByText("Working")).toBeTruthy();
    expect(screen.getByText("Step A")).toBeTruthy();
    expect(screen.getByText("halfway")).toBeTruthy();
  });
});
