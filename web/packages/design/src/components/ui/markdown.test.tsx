import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { Markdown } from "./markdown";

afterEach(cleanup);

describe("Markdown", () => {
  it("renders GFM tables", () => {
    render(<Markdown>{`| Name | Qty |\n| --- | --- |\n| apples | 3 |`}</Markdown>);
    expect(screen.getByRole("table")).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Name" })).toBeTruthy();
    expect(screen.getByRole("cell", { name: "apples" })).toBeTruthy();
    expect(screen.getByRole("cell", { name: "3" })).toBeTruthy();
  });

  it("renders fenced code blocks", () => {
    const { container } = render(<Markdown>{"```js\nconst x = 1;\n```"}</Markdown>);
    const pre = container.querySelector("pre");
    expect(pre).not.toBeNull();
    expect(pre?.textContent).toContain("const x = 1;");
  });

  it("renders inline formatting", () => {
    render(<Markdown>{"hello **world**"}</Markdown>);
    expect(screen.getByText("world").tagName.toLowerCase()).toBe("strong");
  });

  it("opens links in a new tab, safely", () => {
    render(<Markdown>{"[site](https://example.com)"}</Markdown>);
    const a = screen.getByRole("link", { name: "site" });
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toContain("noopener");
    expect(a.getAttribute("href")).toBe("https://example.com");
  });

  it("does NOT render raw HTML (injection-safe)", () => {
    const { container } = render(
      <Markdown>{'<img src=x onerror="alert(1)"> plus <b>markup</b>'}</Markdown>,
    );
    // Raw HTML is escaped, not rendered — no injected elements are created.
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
  });

  it("strips a dangerous link protocol", () => {
    // react-markdown's default urlTransform blanks a javascript: href to "",
    // which also drops the implicit link role — so query the anchor directly.
    const { container } = render(<Markdown>{"[x](javascript:alert(1))"}</Markdown>);
    const a = container.querySelector("a");
    expect(a).not.toBeNull();
    expect(a?.getAttribute("href") ?? "").not.toContain("javascript:");
  });
});
