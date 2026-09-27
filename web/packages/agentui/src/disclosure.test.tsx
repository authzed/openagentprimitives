import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Disclosure } from "./disclosure";
import { renderNode } from "./renderNode";

afterEach(cleanup);

const details = () => screen.getByTestId("fold") as HTMLDetailsElement;

describe("Disclosure", () => {
  it("starts open unless collapsed, and shows its title in the summary", () => {
    render(
      <Disclosure title="Brief" collapsed={false} testId="fold">
        body
      </Disclosure>,
    );
    expect(details()).toHaveAttribute("open");
    expect(screen.getByText("Brief").tagName).toBe("SUMMARY");
  });

  it("the person's toggle wins over the prop until the prop changes", () => {
    const { rerender } = render(
      <Disclosure title="Brief" collapsed testId="fold">
        body
      </Disclosure>,
    );
    expect(details()).not.toHaveAttribute("open");
    fireEvent.click(screen.getByText("Brief"));
    expect(details()).toHaveAttribute("open");
    // The same prop again: the person's choice stands.
    rerender(
      <Disclosure title="Brief" collapsed testId="fold">
        body
      </Disclosure>,
    );
    expect(details()).toHaveAttribute("open");
    // The prop changed: it applies again.
    rerender(
      <Disclosure title="Brief" collapsed={false} testId="fold">
        body
      </Disclosure>,
    );
    expect(details()).toHaveAttribute("open");
    rerender(
      <Disclosure title="Brief" collapsed testId="fold">
        body
      </Disclosure>,
    );
    expect(details()).not.toHaveAttribute("open");
  });

  it("takes the full width whether closed or open, so toggling never shifts the column", () => {
    const { container, rerender } = render(
      <Disclosure title="Brief" collapsed testId="d">
        <p>body</p>
      </Disclosure>,
    );
    const closed = container.querySelector('[data-testid="d"]')!.className;
    rerender(
      <Disclosure title="Brief" collapsed={false} testId="d">
        <p>body</p>
      </Disclosure>,
    );
    const open = container.querySelector('[data-testid="d"]')!.className;
    expect(closed).toContain("w-full");
    expect(closed).toBe(open);
    expect(closed).not.toMatch(/\bborder\b|border-border/);
  });
});

describe("ap:collapsible", () => {
  it("renders its children inside a fold titled by the prop, open by default", () => {
    render(
      renderNode({
        component: "ap:collapsible",
        props: { title: "Tools" },
        children: [{ component: "ap:text", props: { text: "inside" } }],
      }),
    );
    const fold = screen.getByTestId("ap-collapsible") as HTMLDetailsElement;
    expect(fold).toHaveAttribute("open");
    expect(fold.querySelector("summary")).toHaveTextContent("Tools");
    expect(screen.getByText("inside")).toBeInTheDocument();
  });

  it("starts closed when collapsed", () => {
    render(
      renderNode({
        component: "ap:collapsible",
        props: { title: "Tools", collapsed: true },
      }),
    );
    expect(screen.getByTestId("ap-collapsible")).not.toHaveAttribute("open");
  });
});
