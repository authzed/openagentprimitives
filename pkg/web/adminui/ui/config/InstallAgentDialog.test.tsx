import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { InstallAgentDialog } from "./InstallAgentDialog";

beforeEach(() => window.history.pushState({}, "", "/admin/agents"));
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("InstallAgentDialog", () => {
  it("trigger opens the install form; a successful install shows the result and 'View agent' navigates to its detail page", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({ name: "product-manager", appliedKinds: ["AgentClass"], secretsCreated: 1 }),
          { status: 200 },
        ),
      ),
    );
    const onInstalled = vi.fn();

    render(<InstallAgentDialog apiBase="/admin/api" onInstalled={onInstalled} />);
    expect(screen.queryByLabelText("Bundle file")).toBeNull(); // closed by default

    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));
    expect(await screen.findByLabelText("Bundle file")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    await waitFor(() => expect(screen.getByText(/Installed/)).toBeTruthy());
    expect(screen.getByText("product-manager")).toBeTruthy();
    expect(onInstalled).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "View agent" }));
    expect(window.location.pathname).toBe("/admin/agent/default/product-manager");
  });

  // Result.adopted's own doc comment (pkg/platform/oap/install) says callers MUST
  // surface it: adoption overwrites an object this install did not create,
  // and it is recorded nowhere on the object itself. This pins that the
  // success view actually renders it rather than silently dropping the field
  // the way an earlier revision did.
  it("a result carrying adopted objects names them and says they were seized", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            name: "product-manager",
            appliedKinds: ["AgentClass"],
            secretsCreated: 0,
            adopted: ["AgentClass/product-manager", "Secret/demo-token"],
          }),
          { status: 200 },
        ),
      ),
    );

    render(<InstallAgentDialog apiBase="/admin/api" />);
    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));
    fireEvent.change(await screen.findByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    await waitFor(() => expect(screen.getByText(/Installed/)).toBeTruthy());
    expect(screen.getByText("AgentClass/product-manager")).toBeTruthy();
    expect(screen.getByText("Secret/demo-token")).toBeTruthy();
    expect(screen.getByText(/seized/)).toBeTruthy();
  });

  it("shows every installed dependency from a composed graph result", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({
          name: "test-coordinator",
          appliedKinds: ["AgentClass"],
          secretsCreated: 0,
          agents: [{
            agentPath: "reviewer",
            name: "test-coordinator-reviewer",
            appliedKinds: ["AgentClass"],
            secretsCreated: 1,
            agents: [{
              agentPath: "reviewer > helper",
              name: "test-coordinator-reviewer-helper",
              appliedKinds: ["AgentClass"],
              secretsCreated: 0,
            }],
          }],
        }), { status: 200 }),
      ),
    );

    render(<InstallAgentDialog apiBase="/admin/api" />);
    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));
    fireEvent.change(await screen.findByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    expect(await screen.findByText("test-coordinator-reviewer")).toBeTruthy();
    expect(screen.getByText("test-coordinator-reviewer-helper")).toBeTruthy();
    expect(screen.getByText("Dependency reviewer")).toBeTruthy();
    expect(screen.getByText("Dependency reviewer > helper")).toBeTruthy();
  });

  it("reopening after a completed install starts a fresh form, not the stale success screen", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ name: "bot", appliedKinds: ["AgentClass"], secretsCreated: 0 }), {
          status: 200,
        }),
      ),
    );

    render(<InstallAgentDialog apiBase="/admin/api" />);
    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));
    fireEvent.change(await screen.findByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    await waitFor(() => expect(screen.getByText(/Installed/)).toBeTruthy());

    // Two "Close" buttons exist (the footer's + Radix's built-in X dismiss,
    // labeled via its sr-only span) — the footer one is the FIRST in DOM order.
    fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]);
    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));

    expect(await screen.findByLabelText("Bundle file")).toBeTruthy();
    expect(screen.queryByText(/Installed/)).toBeNull();
  });
});
