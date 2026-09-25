import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FlowGraph } from "./FlowGraph";

describe("FlowGraph", () => {
  it("renders channel → agent → tool nodes and pulses edges when active", () => {
    const { container } = render(
      <FlowGraph channelKind="slack" agentClass="support-bot" tools={["bash", "github_mcp"]} active={true} />,
    );
    const texts = [...container.querySelectorAll("text")].map((t) => t.textContent);
    expect(texts).toContain("slack");
    expect(texts).toContain("support-bot");
    expect(texts).toContain("bash");
    expect(texts).toContain("github_mcp");
    expect(container.querySelectorAll("path.ap-edge-active").length).toBeGreaterThan(0);
  });

  it("no active class when paused", () => {
    const { container } = render(
      <FlowGraph channelKind="slack" agentClass="support-bot" tools={["bash"]} active={false} />,
    );
    expect(container.querySelectorAll("path.ap-edge-active").length).toBe(0);
  });
});
