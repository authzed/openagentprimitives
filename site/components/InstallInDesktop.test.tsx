// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { InstallInDesktop } from "./InstallInDesktop";

describe("InstallInDesktop", () => {
  afterEach(cleanup);

  it("links to an oap:// install URL with the ref url-encoded", () => {
    render(<InstallInDesktop image="ghcr.io/acme/agents/demo-agent:1.0" />);
    const link = screen.getByRole("link", { name: "Install in OAP Desktop" });
    expect(link.getAttribute("href")).toBe(
      "oap://install/agent?v=1&ref=ghcr.io%2Facme%2Fagents%2Fdemo-agent%3A1.0",
    );
  });

  it("always shows the CLI alternative", () => {
    render(<InstallInDesktop image="ghcr.io/acme/agents/demo-agent:1.0" />);
    expect(
      screen.getByText("oap agent install ghcr.io/acme/agents/demo-agent:1.0"),
    ).toBeTruthy();
  });
});
