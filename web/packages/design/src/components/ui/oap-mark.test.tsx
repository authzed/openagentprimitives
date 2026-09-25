import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { OapLogomark, OapMark, OapMarkRelay } from "./oap-mark";

describe("OapMark", () => {
  // currentColor is the whole point: one asset for every surface and theme.
  it("paints with currentColor and is decorative by default", () => {
    const { container } = render(<OapMark className="h-4" />);
    const svg = container.querySelector("svg")!;
    expect(svg.getAttribute("fill")).toBe("currentColor");
    expect(svg.getAttribute("aria-hidden")).toBe("true");
    expect(svg.querySelector("title")).toBeNull();
    expect(svg.className.baseVal).toContain("h-4");
  });

  it("becomes a labelled image when given a title", () => {
    const { container } = render(<OapMark title="Open Agent Primitives" />);
    const svg = container.querySelector("svg")!;
    expect(svg.getAttribute("role")).toBe("img");
    expect(svg.getAttribute("aria-hidden")).toBeNull();
    expect(svg.querySelector("title")?.textContent).toBe("Open Agent Primitives");
  });

  it("renders the three letterform primitives in the logomark", () => {
    const { container } = render(<OapLogomark />);
    expect(container.querySelectorAll("path")).toHaveLength(3);
  });
});

describe("OapMarkRelay", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    // @ts-expect-error jsdom has no matchMedia; tests install one per case.
    delete window.matchMedia;
  });

  it("is the plain mark when idle: band hidden, no frame loop", () => {
    const raf = vi.spyOn(window, "requestAnimationFrame");
    const { container } = render(<OapMarkRelay className="h-5" />);
    const svg = container.querySelector("svg")!;
    expect(svg.getAttribute("fill")).toBe("currentColor");
    expect(svg.getAttribute("data-relay")).toBe("off");
    expect(svg.querySelector("g")!.style.opacity).toBe("0");
    expect(svg.querySelector<SVGPathElement>("[data-ink]")!.style.opacity).toBe("1");
    expect(raf).not.toHaveBeenCalled();
  });

  it("runs the band in the chart series while active, and parks it again when not", () => {
    let frame: FrameRequestCallback | null = null;
    vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => {
      frame = cb;
      return 1;
    });
    const cancel = vi.spyOn(window, "cancelAnimationFrame").mockImplementation(() => {});
    const { container, rerender } = render(<OapMarkRelay active />);
    const svg = container.querySelector("svg")!;
    expect(svg.getAttribute("data-relay")).toBe("on");
    expect(svg.querySelector("g")!.style.opacity).toBe("1");
    expect(svg.querySelector<SVGPathElement>("[data-ink]")!.style.opacity).toBe("0.35");
    expect(frame).not.toBeNull();
    // Five sixths of a lap in: the head has crossed two corners (Y2 hands over
    // every second corner) and is clear of the handover split, so the whole
    // band is on the second hue.
    frame!(performance.now() + (2400 * 5) / 6);
    const a = svg.querySelector<SVGPathElement>('[data-seg="a"]')!;
    expect(a.style.opacity).toBe("1");
    expect(a.style.stroke).toBe("hsl(var(--chart-2))");
    expect(a.style.strokeDasharray).toBe("12 88");
    rerender(<OapMarkRelay active={false} />);
    expect(cancel).toHaveBeenCalled();
    expect(svg.getAttribute("data-relay")).toBe("off");
    expect(svg.querySelector("g")!.style.opacity).toBe("0");
    expect(svg.querySelector<SVGPathElement>("[data-ink]")!.style.opacity).toBe("1");
  });

  it("never starts under prefers-reduced-motion", () => {
    window.matchMedia = vi.fn().mockReturnValue({ matches: true }) as unknown as typeof window.matchMedia;
    const raf = vi.spyOn(window, "requestAnimationFrame");
    const { container } = render(<OapMarkRelay active />);
    expect(container.querySelector("svg")!.getAttribute("data-relay")).toBe("off");
    expect(raf).not.toHaveBeenCalled();
  });
});
