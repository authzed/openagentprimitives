// @vitest-environment jsdom
import { render, screen } from "@testing-library/react";
import { beforeAll, describe, expect, it } from "vitest";
import { FIGS, GEO_NAMES, GeoFigure } from "./GeoFigure";

beforeAll(() => {
  // jsdom has no SVG geometry; the figures' anim() measures path lengths.
  const proto = SVGElement.prototype as unknown as {
    getTotalLength?: () => number;
  };
  proto.getTotalLength ??= () => 100;
});

describe("FIGS", () => {
  it("exports exactly the names GeoName declares", () => {
    expect(Object.keys(FIGS).sort()).toEqual([...GEO_NAMES].sort());
  });

  it.each(GEO_NAMES)("%s renders a deterministic <svg> string", (name) => {
    const a = FIGS[name].svg();
    expect(a.startsWith("<svg")).toBe(true);
    expect(FIGS[name].svg()).toBe(a);
  });
});

describe("GeoFigure", () => {
  it("renders the figure as a labelled image carrying its hue", () => {
    render(<GeoFigure name="gate" label="Each tool call passes a gate" hue={1} />);
    const el = screen.getByRole("img", { name: "Each tool call passes a gate" });
    expect(el.dataset.geo).toBe("gate");
    expect(el.dataset.hue).toBe("1");
    expect(el.className).toBe("geo");
    expect(el.querySelector("svg")).not.toBeNull();
  });
});
