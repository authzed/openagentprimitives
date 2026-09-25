import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { SystemPage } from "./SystemPage";

afterEach(cleanup);

describe("SystemPage", () => {
  it("renders the title and message", () => {
    render(<SystemPage status={404} kind="notFound" title="Not found" message="No such page." />);
    expect(screen.getByText("Not found")).toBeTruthy();
    expect(screen.getByText("No such page.")).toBeTruthy();
  });

  it("renders an action link when provided", () => {
    render(
      <SystemPage status={403} kind="forbidden" title="Forbidden" message="No access."
        actions={[{ label: "Sign in", href: "/oidc/login" }]} />,
    );
    const link = screen.getByRole("link", { name: "Sign in" });
    expect(link.getAttribute("href")).toBe("/oidc/login");
  });
});
