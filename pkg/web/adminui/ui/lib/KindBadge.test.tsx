import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { KindBadge, KindDot } from "./KindBadge";

afterEach(cleanup);

// dotColor reads the inline background-color of the color swatch inside a
// rendered KindBadge/KindDot (jsdom normalizes the hex to an rgb() string).
function dotColor(root: HTMLElement): string | undefined {
  const dot = root.querySelector('span[aria-hidden="true"][style*="background-color"]');
  return (dot as HTMLElement | null)?.style.backgroundColor || undefined;
}

describe("KindBadge", () => {
  it("renders the kind label prefixed with a color swatch", () => {
    const { container } = render(<KindBadge kind="slack" />);
    expect(screen.getByText("slack")).toBeTruthy();
    expect(dotColor(container)).toBeTruthy();
  });

  it("is stable: the same value → the same color, independent of any surrounding set", () => {
    const a = render(<KindBadge kind="oauth" />).container;
    const b = render(<KindBadge kind="oauth" />).container;
    expect(dotColor(a)).toBeTruthy();
    expect(dotColor(a)).toBe(dotColor(b));
  });

  it("distinct kinds get distinct colors (no collision for these values)", () => {
    const slack = render(<KindBadge kind="slack" />).container;
    const mcp = render(<KindBadge kind="mcpserver" />).container;
    expect(dotColor(slack)).not.toBe(dotColor(mcp));
  });

  it("renders nothing for an empty or absent kind", () => {
    const { container } = render(<KindBadge kind="" />);
    expect(container.firstChild).toBeNull();
    const { container: c2 } = render(<KindBadge />);
    expect(c2.firstChild).toBeNull();
  });

  it("KindDot renders a bare stable swatch and nothing for an empty value", () => {
    const { container } = render(<KindDot kind="static" />);
    expect(dotColor(container)).toBeTruthy();
    const { container: c2 } = render(<KindDot kind="" />);
    expect(c2.firstChild).toBeNull();
  });
});
