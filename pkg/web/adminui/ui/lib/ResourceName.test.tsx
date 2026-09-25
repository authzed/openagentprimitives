import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import {
  ResourceName,
  isHiddenNamespace,
  resourceDisplayName,
  splitResourceId,
} from "./ResourceName";

afterEach(cleanup);

describe("splitResourceId", () => {
  it("splits ns/name on the first slash", () => {
    expect(splitResourceId("ops/support-bot")).toEqual({ ns: "ops", name: "support-bot" });
  });
  it("treats a bare id as a name with no namespace", () => {
    expect(splitResourceId("support-bot")).toEqual({ ns: "", name: "support-bot" });
  });
});

describe("isHiddenNamespace", () => {
  it("hides default and empty, keeps everything else", () => {
    expect(isHiddenNamespace("default")).toBe(true);
    expect(isHiddenNamespace("")).toBe(true);
    expect(isHiddenNamespace("ops")).toBe(false);
  });
});

describe("resourceDisplayName", () => {
  it("drops the default namespace, showing just the name", () => {
    expect(resourceDisplayName("default/s1")).toBe("s1");
  });
  it("keeps a non-default namespace", () => {
    expect(resourceDisplayName("ops/s1")).toBe("ops/s1");
  });
  it("passes a bare name through", () => {
    expect(resourceDisplayName("s1")).toBe("s1");
  });
});

describe("ResourceName", () => {
  it("hides the default namespace, rendering just the name (no muted prefix)", () => {
    const { container } = render(<ResourceName id="default/s1" />);
    expect(container.textContent).toBe("s1");
    expect(container.querySelector(".text-muted-foreground")).toBeNull();
  });

  it("renders a non-default namespace muted, before the name", () => {
    const { container } = render(<ResourceName id="ops/s1" />);
    expect(container.textContent).toBe("ops/s1");
    const nsSpan = container.querySelector(".text-muted-foreground");
    expect(nsSpan?.textContent).toBe("ops/");
  });

  it("accepts explicit ns + name parts", () => {
    render(<ResourceName ns="team" name="billing" />);
    expect(screen.getByText("team/")).toBeTruthy();
    expect(screen.getByText("billing")).toBeTruthy();
  });
});
