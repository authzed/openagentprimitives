import { describe, expect, it } from "vitest";
import { hooksContaining, questionOutsideHooks, stepBindings, treeContains } from "./tree";
import type { Node } from "./types";

const view: Node = { component: "ap:stack", children: [
  { component: "oap:generative", props: { name: "brief", allowedComponents: ["*"] }, children: [
    { component: "ap:card", children: [{ component: "ap:question", props: { prompt: "Which repo?" } }] },
  ]},
  { component: "ap:text", props: { text: "x" } },
]};

describe("treeContains", () => {
  it("finds a component at any depth", () => { expect(treeContains(view, "ap:question")).toBe(true); });
  it("is false when absent", () => { expect(treeContains(view, "ap:progress")).toBe(false); });
  it("is false for no view", () => { expect(treeContains(undefined, "ap:question")).toBe(false); });
  it("names the hooks holding a component", () => { expect([...hooksContaining(view, "ap:question")]).toEqual(["brief"]); expect(hooksContaining(view, "ap:progress").size).toBe(0); });
});

// questionOutsideHooks is the half of "is the page still asking" that the
// answered set cannot reach: a question the AUTHOR put outside every
// oap:generative is nobody's to answer and nothing marks it answered, so it
// keeps the platform's reply modal shut for as long as it is declared.
describe("questionOutsideHooks", () => {
  it("is false for a question that lives inside a hook", () => { expect(questionOutsideHooks(view)).toBe(false); });

  it("is true for a question the author declared outside every hook", () => {
    const authored: Node = { component: "ap:stack", children: [
      { component: "ap:card", children: [{ component: "ap:question", props: { prompt: "Which repo?" } }] },
    ]};
    expect(questionOutsideHooks(authored)).toBe(true);
  });

  it("is false for a page with no question at all", () => {
    expect(questionOutsideHooks({ component: "ap:stack", children: [{ component: "ap:text", props: { text: "x" } }] })).toBe(false);
    expect(questionOutsideHooks(undefined)).toBe(false);
  });
});

describe("stepBindings", () => {
  const timeline = (steps: unknown[]) => ({ component: "ap:steps", props: { steps } });
  const bound = (name: string, step: string, title?: string) => ({ component: "oap:generative", props: { name, allowedComponents: ["*"], step, ...(title !== undefined ? { title } : {}) } });
  const steps = [
    { id: "assess", label: "Intake", state: "done" },
    { id: "tools", label: "Tools", state: "active" },
    { id: "build", label: "Build" },
  ];

  it("maps each bound hook to its step's label and state, read from the one timeline", () => {
    const view = { component: "ap:stack", children: [timeline(steps), bound("brief", "assess", "Brief"), bound("tools", "tools"), bound("later", "build")] };
    const m = stepBindings(view);
    expect(m.get("brief")).toEqual({ step: "assess", label: "Intake", state: "done", title: "Brief" });
    expect(m.get("tools")).toEqual({ step: "tools", label: "Tools", state: "active" });
    expect(m.get("later")).toEqual({ step: "build", label: "Build", state: "upcoming" });
  });

  it("ignores a hook whose step the timeline does not declare, and hooks with no step", () => {
    const view = { component: "ap:stack", children: [timeline(steps), bound("x", "nope"), { component: "oap:generative", props: { name: "free", allowedComponents: ["*"] } }] };
    expect(stepBindings(view).size).toBe(0);
  });

  it("answers nothing for zero or two timelines, and for no view", () => {
    expect(stepBindings(undefined).size).toBe(0);
    expect(stepBindings({ component: "ap:stack", children: [bound("brief", "assess")] }).size).toBe(0);
    expect(stepBindings({ component: "ap:stack", children: [timeline(steps), timeline(steps), bound("brief", "assess")] }).size).toBe(0);
  });

  it("answers nothing for a timeline whose steps are not an array", () => {
    expect(stepBindings({ component: "ap:stack", children: [{ component: "ap:steps", props: { steps: "nope" } }, bound("brief", "assess")] }).size).toBe(0);
  });

  it("falls back to the id as the label when a step has none", () => {
    const view = { component: "ap:stack", children: [timeline([{ id: "assess", state: "done" }]), bound("brief", "assess")] };
    expect(stepBindings(view).get("brief")).toEqual({ step: "assess", label: "assess", state: "done" });
  });

  it("skips a step whose id is not a string", () => {
    const view = { component: "ap:stack", children: [timeline([{ id: 7, label: "Seven", state: "done" }, { id: "tools", label: "Tools", state: "active" }]), bound("x", "7"), bound("tools", "tools")] };
    const m = stepBindings(view);
    expect(m.has("x")).toBe(false);
    expect(m.get("tools")?.label).toBe("Tools");
  });
});
