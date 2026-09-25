import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { InstallAgentForm } from "./InstallAgentForm";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// jsonResponse builds a fetch Response the way admind's handlers do: a JSON
// body at the given status.
function jsonResponse(body: unknown, status: number) {
  return new Response(JSON.stringify(body), { status });
}

describe("InstallAgentForm", () => {
  it("file source: submit is disabled until a file is chosen", () => {
    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    expect((screen.getByRole("button", { name: "Install" }) as HTMLButtonElement).disabled).toBe(true);

    const file = new File(["fake-bundle-bytes"], "agent.oap");
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [file] } });

    expect((screen.getByRole("button", { name: "Install" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("a 400 missing-question response renders the exact typed field (secret → masked input), and re-submitting with the answer installs", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi
      .fn()
      // First submit: no answers yet — the bundle's one required secret question comes back.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [{ name: "githubToken", type: "secret", prompt: "GitHub PAT" }],
          },
          400,
        ),
      )
      // Second submit: the answer is included — installs cleanly.
      .mockResolvedValueOnce(
        jsonResponse({ name: "product-manager", appliedKinds: ["AgentClass"], secretsCreated: 1 }, 200),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // The missing question renders as a MASKED (password) input, labeled with its prompt.
    const secretInput = await screen.findByLabelText("GitHub PAT *");
    expect(secretInput.getAttribute("type")).toBe("password");

    fireEvent.change(secretInput, { target: { value: "fake-token-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    expect(onInstalled).toHaveBeenCalledWith(
      { name: "product-manager", appliedKinds: ["AgentClass"], secretsCreated: 1 },
      "default",
    );

    // The second POST is multipart (file source) and carries the collected answer
    // in its "values" field (the same JSON-encoded shape the JSON/ref path sends inline).
    const secondCallForm = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(JSON.parse(secondCallForm.get("values") as string)).toEqual({ githubToken: "fake-token-value" });
  });

  it("qualifies identical root, child, and grandchild question names and shows dependency context", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "missing required question(s)",
        questions: [
          { name: "region", type: "string", prompt: "Region" },
          { agentPath: "reviewer", name: "region", type: "string", prompt: "Region" },
          { agentPath: "reviewer > helper", name: "region", type: "string", prompt: "Region" },
        ],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const inputs = await screen.findAllByLabelText("Region *");
    expect(inputs).toHaveLength(3);
    expect(screen.getByText("Root agent")).toBeTruthy();
    expect(screen.getByText("Dependency reviewer")).toBeTruthy();
    expect(screen.getByText("Dependency reviewer > helper")).toBeTruthy();
    fireEvent.change(inputs[0], { target: { value: "root-region" } });
    fireEvent.change(inputs[1], { target: { value: "child-region" } });
    fireEvent.change(inputs[2], { target: { value: "grandchild-region" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    const sent = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(JSON.parse(sent.get("values") as string)).toEqual({
      region: "root-region",
      "agents.reviewer.region": "child-region",
      "agents.reviewer.agents.helper.region": "grandchild-region",
    });
  });

  it("completes a channel-only decision with its setup token, resumes the install, and returns the graph result", async () => {
    const onInstalled = vi.fn();
    const graphResult = {
      name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0,
      agents: [{ agentPath: "reviewer", name: "test-root-reviewer", appliedKinds: ["AgentClass"], secretsCreated: 1 }],
    };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "required channel setup is incomplete", questions: [],
        channels: [{ agentPath: "reviewer", name: "test-root-reviewer-chat", kind: "fake", role: "both", purpose: "Review replies", status: "ask", setupToken: "opaque-token", questions: [] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root-reviewer-chat", namespace: "default", kind: "fake", agentClass: "test-root-reviewer", staged: true }, 200))
      .mockResolvedValueOnce(jsonResponse(graphResult, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    expect(await screen.findByText("Review replies")).toBeTruthy();
    expect(screen.getByText("Dependency reviewer")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-reviewer-chat" }));
    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    expect(fetchMock.mock.calls[1][0]).toBe("/admin/api/agents/channel-setup");
    expect(JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string)).toEqual({ setupToken: "opaque-token", answers: {} });

    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));
    await waitFor(() => expect(onInstalled).toHaveBeenCalledWith(graphResult, "default"));
  });

  it("keeps mixed questions, conflicts, warnings, and channels visible and submits all decisions", async () => {
    const onInstalled = vi.fn();
    const decisions = {
      error: "missing required question(s)",
      questions: [{ agentPath: "reviewer", name: "region", type: "string", prompt: "Region" }],
      conflicts: [{ agentPath: "reviewer", kind: "ConfigMap", namespace: "default", name: "test-root-reviewer-config" }],
      warnings: ["reviewer capacity was adjusted"],
      channels: [{ agentPath: "reviewer", name: "test-root-reviewer-chat", kind: "fake", role: "both", status: "ask", setupToken: "mixed-token", questions: [] }],
    };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse(decisions, 400))
      // Editing any answer invalidates adoption consent because a binding may
      // target metadata.name. Replan once, then approve the freshly observed
      // conflict while the warning and channel decision remain present.
      .mockResolvedValueOnce(jsonResponse(decisions, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root-reviewer-chat", namespace: "default", kind: "fake", agentClass: "test-root-reviewer", staged: true }, 200))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    expect(await screen.findByText("reviewer capacity was adjusted")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Region *"), { target: { value: "us-test" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));
    fireEvent.click(await screen.findByLabelText("ConfigMap default/test-root-reviewer-config"));
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-reviewer-chat" }));
    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Adopt and resume install" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    const sent = (fetchMock.mock.calls[3][1] as RequestInit).body as FormData;
    expect(JSON.parse(sent.get("values") as string)).toEqual({ "agents.reviewer.region": "us-test" });
    expect(JSON.parse(sent.get("adopt") as string)).toEqual(["ConfigMap/test-root-reviewer-config"]);
  });

  it("preserves an exact-token staged channel and guidance while unrelated questions replan", async () => {
    const onInstalled = vi.fn();
    const channel = { name: "test-root-slack", kind: "slack", role: "both", status: "ask", setupToken: "stable-token", questions: [{ name: "conversation", type: "string", prompt: "Conversation" }] };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "missing required question(s)", questions: [{ name: "region", type: "string", prompt: "Region" }], channels: [channel],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({
        name: channel.name, namespace: "default", kind: channel.kind, agentClass: "test-root", staged: true,
        nextSteps: ["Invite the bot after install."], warnings: ["Grant chat:write."],
      }, 200))
      .mockResolvedValueOnce(jsonResponse({
        error: "missing required question(s)", questions: [{ name: "tier", type: "string", prompt: "Tier" }], channels: [channel],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    const conversation = await screen.findByLabelText("Conversation *") as HTMLInputElement;
    fireEvent.change(conversation, { target: { value: "C123" } });
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-slack" }));
    await screen.findByText("Setup staged");

    fireEvent.change(screen.getByLabelText("Region *"), { target: { value: "us-test" } });
    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));

    await screen.findByLabelText("Tier *");
    const restoredConversation = screen.getByLabelText("Conversation *") as HTMLInputElement;
    expect(restoredConversation.disabled).toBe(true);
    expect(restoredConversation.value).toBe("C123");
    expect(screen.getByText("Setup staged")).toBeTruthy();
    expect(screen.getByText("Invite the bot after install.")).toBeTruthy();
    expect(screen.getByText("Grant chat:write.")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Tier *"), { target: { value: "enterprise" } });
    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));
    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    expect(onInstalled.mock.calls[0][0].warnings).toEqual([
      "test-root-slack: Invite the bot after install.",
      "test-root-slack: Grant chat:write.",
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it("keeps exact-token staging across a conflict replan but clears adoption consent", async () => {
    const onInstalled = vi.fn();
    const channel = { name: "test-root-chat", kind: "fake", role: "both", status: "ask", setupToken: "stable-conflict-token", questions: [] };
    const conflict = { kind: "ConfigMap", namespace: "default", name: "test-root-config" };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "decisions required", questions: [{ name: "region", type: "string", prompt: "Region" }], conflicts: [conflict], channels: [channel],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({
        name: channel.name, namespace: "default", kind: channel.kind, agentClass: "test-root", staged: true,
        nextSteps: ["Finish the external setup."],
      }, 200))
      .mockResolvedValueOnce(jsonResponse({ error: "decisions required", questions: [], conflicts: [conflict], channels: [channel] }, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    const adopt = await screen.findByLabelText("ConfigMap default/test-root-config") as HTMLInputElement;
    fireEvent.click(adopt);
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-chat" }));
    await screen.findByText("Setup staged");

    fireEvent.change(screen.getByLabelText("Region *"), { target: { value: "us-test" } });
    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));

    const replannedAdopt = await screen.findByLabelText("ConfigMap default/test-root-config") as HTMLInputElement;
    expect(replannedAdopt.checked).toBe(false);
    expect(screen.getByText("Setup staged")).toBeTruthy();
    expect(screen.getByText("Finish the external setup.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up test-root-chat" })).toBeNull();
    fireEvent.click(replannedAdopt);
    fireEvent.click(screen.getByRole("button", { name: "Adopt and resume install" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    const sent = (fetchMock.mock.calls[3][1] as RequestInit).body as FormData;
    expect(JSON.parse(sent.get("adopt") as string)).toEqual(["ConfigMap/test-root-config"]);
  });

  it("starts a handoff in a separate browser context, retains the token, and resumes through channel setup", async () => {
    const onInstalled = vi.fn();
    const opened = vi.spyOn(window, "open").mockReturnValue({} as Window);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "required channel setup is incomplete", questions: [],
        channels: [{ name: "test-root-github", kind: "github", role: "input", status: "handoff", setupToken: "handoff-token", questions: [{ name: "organization", type: "string", prompt: "Organization" }] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ setupToken: "handoff-token", explain: "Create the app", url: "https://provider.example/start?state=opaque" }, 200))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root-github", namespace: "default", kind: "github", agentClass: "test-root", staged: true }, 200))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    fireEvent.change(await screen.findByLabelText("Organization *"), { target: { value: "test-org" } });
    fireEvent.click(screen.getByRole("button", { name: "Start browser setup for test-root-github" }));

    await waitFor(() => expect(opened).toHaveBeenCalledWith("https://provider.example/start?state=opaque", "_blank", "noopener,noreferrer"));
    fireEvent.click(screen.getByRole("button", { name: "Resume test-root-github" }));
    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    expect(JSON.parse((fetchMock.mock.calls[2][1] as RequestInit).body as string)).toEqual({ setupToken: "handoff-token", answers: { organization: "test-org" } });
    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));
    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
  });

  it("follows a server-directed setup transition without provider-specific UI logic", async () => {
    const opened = vi.spyOn(window, "open").mockReturnValue({} as Window);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "required channel setup is incomplete", questions: [],
        channels: [{ name: "test-root-chat", kind: "custom", role: "both", status: "handoff", setupToken: "transition-token", questions: [{ name: "route", type: "enum", prompt: "Route", enum: ["existing", "create"] }] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ setupToken: "transition-token", nextAction: "setup", explain: "Use the existing application" }, 200))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root-chat", namespace: "default", kind: "custom", agentClass: "test-root", staged: true }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    fireEvent.change(await screen.findByLabelText("Route *"), { target: { value: "existing" } });
    fireEvent.click(screen.getByRole("button", { name: "Start browser setup for test-root-chat" }));

    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    expect(opened).not.toHaveBeenCalled();
    expect(fetchMock.mock.calls[1][0]).toBe("/admin/api/agents/channel-handoff");
    expect(fetchMock.mock.calls[2][0]).toBe("/admin/api/agents/channel-setup");
  });

  it("locks staged fields and carries setup guidance and warnings into completion", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "required channel setup is incomplete", questions: [],
        channels: [{ name: "test-root-slack", kind: "slack", role: "both", status: "ask", setupToken: "slack-token", questions: [{ name: "conversation", type: "string", prompt: "Conversation" }] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({
        name: "test-root-slack", namespace: "default", kind: "slack", agentClass: "test-root", staged: true,
        nextSteps: ["Invite the bot to the conversation."], warnings: ["The bot still needs chat:write."],
      }, 200))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root", appliedKinds: ["AgentClass"], secretsCreated: 1, warnings: ["capacity notice"] }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));
    const field = await screen.findByLabelText("Conversation *") as HTMLInputElement;
    fireEvent.change(field, { target: { value: "C123" } });
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-slack" }));

    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    expect(field.disabled).toBe(true);
    fireEvent.change(field, { target: { value: "C999" } });
    expect(field.value).toBe("C123");
    expect(screen.getByText("Invite the bot to the conversation.")).toBeTruthy();
    expect(screen.getByText("The bot still needs chat:write.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Resume install" }));
    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    expect(onInstalled.mock.calls[0][0].warnings).toEqual([
      "capacity notice",
      "test-root-slack: Invite the bot to the conversation.",
      "test-root-slack: The bot still needs chat:write.",
    ]);
    expect(JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string).answers).toEqual({ conversation: "C123" });
  });

  it("answer replanning discards a pending channel token and typed credential before accepting a fresh decision", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        error: "decisions required", questions: [{ name: "region", type: "string", prompt: "Region" }],
        channels: [{ name: "test-root-chat", kind: "fake", role: "both", status: "ask", setupToken: "old-token", questions: [{ name: "apiKey", type: "secret", prompt: "API key" }] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({
        error: "required channel setup is incomplete", questions: [],
        channels: [{ name: "test-root-chat", kind: "fake", role: "both", status: "ask", setupToken: "fresh-token", questions: [{ name: "apiKey", type: "secret", prompt: "API key" }] }],
      }, 400))
      .mockResolvedValueOnce(jsonResponse({ name: "test-root-chat", namespace: "default", kind: "fake", agentClass: "test-root", staged: true }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), { target: { files: [new File(["bytes"], "agent.oap")] } });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const firstSecret = await screen.findByLabelText("API key *") as HTMLInputElement;
    fireEvent.change(firstSecret, { target: { value: "must-not-cross-targets" } });
    fireEvent.change(screen.getByLabelText("Region *"), { target: { value: "us-test" } });

    expect(screen.queryByText("test-root-chat")).toBeNull();
    expect(screen.queryByLabelText("API key *")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    const freshSecret = await screen.findByLabelText("API key *") as HTMLInputElement;
    expect(freshSecret.value).toBe("");
    fireEvent.change(freshSecret, { target: { value: "fresh-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Set up test-root-chat" }));

    await waitFor(() => expect(screen.getByText("Setup staged")).toBeTruthy());
    expect(JSON.parse((fetchMock.mock.calls[2][1] as RequestInit).body as string)).toEqual({
      setupToken: "fresh-token",
      answers: { apiKey: "fresh-value" },
    });
  });

  it("ref source: switching source reveals the ref field; a 403 surfaces 'not authorized'", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "forbidden" }, 403)));

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Registry ref" }));

    expect(screen.queryByLabelText("Bundle file")).toBeNull();
    fireEvent.change(screen.getByLabelText("Registry ref"), {
      target: { value: "ghcr.io/acme/support-bot:1.0.0" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    await waitFor(() => expect(screen.getByText("Not authorized to install agents.")).toBeTruthy());
  });

  it("a 400 carrying skillClones renders the 'will clone' notice alongside the questions", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [{ name: "githubToken", type: "secret", prompt: "GitHub PAT" }],
            skillClones: [
              { skillSource: "example-skill", repoURL: "https://github.com/fakeorg/skills", ref: "main" },
            ],
          },
          400,
        ),
      ),
    );

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // The clone notice names the repo + ref + SkillSource, before the operator re-submits.
    await waitFor(() =>
      expect(screen.getByText(/Installing this agent will clone/)).toBeTruthy(),
    );
    expect(screen.getByText(/https:\/\/github.com\/fakeorg\/skills/)).toBeTruthy();
    expect(screen.getByText(/example-skill/)).toBeTruthy();
  });

  it("a question carrying a default pre-fills the field AND submits it untouched — a capacity clamp question never loops", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi
      .fn()
      // First submit: an oversized SpiceboxClass produces a capacity question
      // whose Default is the suggested clamp value — the operator never types
      // into this field at all.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [
              {
                name: "capacity.oversized-sandbox.memory",
                type: "string",
                prompt: "SpiceboxClass oversized-sandbox requests memory=4Gi but only 1Gi is available — lower it to fit?",
                default: "768Mi",
              },
            ],
            warnings: ["SpiceboxClass oversized-sandbox: lowering memory from 4Gi to 768Mi to fit largest node oap-desktop"],
          },
          400,
        ),
      )
      .mockResolvedValueOnce(
        jsonResponse({ name: "oversized-agent", appliedKinds: ["AgentClass", "SpiceboxClass"], secretsCreated: 0 }, 200),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // The suggested clamp shows both as the field's pre-filled value AND as
    // the notice explaining it — the operator can see why before submitting.
    const memoryInput = (await screen.findByLabelText(
      "SpiceboxClass oversized-sandbox requests memory=4Gi but only 1Gi is available — lower it to fit? *",
    )) as HTMLInputElement;
    expect(memoryInput.value).toBe("768Mi");
    expect(screen.getByText(/lowering memory from 4Gi to 768Mi/)).toBeTruthy();

    // Submitting WITHOUT touching the field must still send the pre-filled
    // value — this is the regression this test guards: display-only pre-fill
    // (rendering q.default but never writing it into `values`) would submit
    // nothing for this key, and admind would 400 the exact same question again.
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    const secondCallForm = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(JSON.parse(secondCallForm.get("values") as string)).toEqual({
      "capacity.oversized-sandbox.memory": "768Mi",
    });
  });

  it("renders bool as a checkbox, enum as a select, and int as a number input", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [
              { name: "verbose", type: "bool", prompt: "Verbose logging?" },
              { name: "tier", type: "enum", prompt: "Tier", enum: ["small", "large"] },
              { name: "maxTurns", type: "int", prompt: "Max turns" },
            ],
          },
          400,
        ),
      ),
    );

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    expect((await screen.findByLabelText("Verbose logging? *")).getAttribute("type")).toBe("checkbox");
    expect(screen.getByLabelText("Tier *").tagName).toBe("SELECT");
    expect(screen.getByLabelText("Max turns *").getAttribute("type")).toBe("number");
  });

  // enumLabels is what the operator READS for each enum entry, paired with
  // `enum` positionally; the VALUE submitted is still the enum entry, because
  // that is the stable answer key the backend and `--answer` both address.
  //
  // oap.Manifest.ValidateQuestions accepts the field, so a bundle author can
  // declare labels, see them honored in `oap agent install`, and — before this
  // — lose them silently here. That is the same "silently ignored is worse
  // than absent" hazard the validators close for binding, secret and
  // validation. The row deliberately mixes a real label, a blank one and a
  // missing one, because the fallback is per-entry and not all-or-nothing.
  it("renders an enum's enumLabels as the option text while still submitting the raw value", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [
              {
                name: "slackapp",
                type: "enum",
                prompt: "Have you already created a Slack app?",
                enum: ["false", "true", "provision", "later"],
                // Four shapes, because the pairing rule is per-entry and each
                // one is a different branch of enumOptionLabel / Go's
                // EnumLabelFor: a plain label, a BLANK one (whitespace is no
                // label, so it falls back), a PADDED one (returned verbatim —
                // the trim decides, it does not rewrite), and a MISSING one
                // (the list is shorter than enum).
                enumLabels: ["Show me the manifest — I'll create it myself", "   ", "  Have oap provision it  "],
              },
            ],
          },
          400,
        ),
      )
      .mockResolvedValueOnce(jsonResponse({ name: "demo-agent", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const select = (await screen.findByLabelText("Have you already created a Slack app? *")) as HTMLSelectElement;
    const options = [...select.options].filter((o) => o.value !== "");
    expect(options.map((o) => o.textContent)).toEqual([
      "Show me the manifest — I'll create it myself",
      "true",
      // Verbatim, padding intact: Go's EnumLabelFor trims only to decide
      // whether a label counts and returns the declared string unchanged, so
      // a renderer that trimmed here would be quietly saying something else.
      "  Have oap provision it  ",
      "later",
    ]);
    expect(options.map((o) => o.value)).toEqual(["false", "true", "provision", "later"]);

    // The label is what the operator picks by; the VALUE is what is sent.
    fireEvent.change(select, { target: { value: "false" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    const submitted = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(JSON.parse(submitted.get("values") as string)).toEqual({ slackapp: "false" });
  });

  it("a 409 conflicts response renders a tickable row per conflict (Secret starts unticked) and re-submits only the ticked keys", async () => {
    const onInstalled = vi.fn();
    const fetchMock = vi
      .fn()
      // First submit: the cluster already holds an AgentClass and a Secret this
      // install did not create.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [
              { kind: "AgentClass", namespace: "demo", name: "demo-agent" },
              { kind: "Secret", namespace: "demo", name: "demo-token", secret: true },
            ],
          },
          409,
        ),
      )
      // Second submit: only the ticked AgentClass is adopted — installs cleanly.
      .mockResolvedValueOnce(
        jsonResponse({ name: "demo-agent", appliedKinds: ["AgentClass"], secretsCreated: 0 }, 200),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={onInstalled} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // Both conflicts are listed; the Secret starts UNTICKED and names the risk.
    const classRow = (await screen.findByLabelText(/AgentClass demo\/demo-agent/)) as HTMLInputElement;
    expect(classRow.checked).toBe(false);
    const secretRow = screen.getByLabelText(/Secret demo\/demo-token/) as HTMLInputElement;
    expect(secretRow.checked).toBe(false);
    expect(screen.getByText(/overwrites this Secret's data/i)).toBeTruthy();

    fireEvent.click(classRow);
    fireEvent.click(screen.getByRole("button", { name: /adopt and install/i }));

    await waitFor(() => expect(onInstalled).toHaveBeenCalledTimes(1));
    expect(onInstalled).toHaveBeenCalledWith(
      { name: "demo-agent", appliedKinds: ["AgentClass"], secretsCreated: 0 },
      "default",
    );

    // Only the ticked key rides the re-submit — never every conflict.
    const secondCallForm = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(JSON.parse(secondCallForm.get("adopt") as string)).toEqual(["AgentClass/demo-agent"]);
  });

  // Regression for the Critical finding: adoptKeys must NOT survive a change
  // to the install target (file/ref/namespace/source). The wire key is
  // Kind+"/"+Name with no namespace (install.Conflict.Key()), so retargeting
  // to a different namespace can collide on the exact same key — an
  // un-cleared tick would then either silently authorize a seizure the
  // operator never saw a row for on the NEW target, or, worse, render a
  // Secret checkbox pre-ticked if that same key is a Secret this time.
  it("retargeting after ticking a conflict clears the tick — no stale adopt rides the next submit, and a same-key Secret never renders pre-ticked", async () => {
    const fetchMock = vi
      .fn()
      // First submit, namespace "demo": one AgentClass conflict.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "demo", name: "demo-agent" }],
          },
          409,
        ),
      )
      // Second submit, after retargeting to namespace "other": the SAME
      // Kind/Name key comes back, but this time as a Secret.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "other", name: "demo-agent", secret: true }],
          },
          409,
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const classRow = (await screen.findByLabelText(/AgentClass demo\/demo-agent/)) as HTMLInputElement;
    fireEvent.click(classRow);
    expect(classRow.checked).toBe(true);

    // The operator picked the wrong namespace and retargets — this must
    // clear both the stale conflict row and the stale tick immediately,
    // not just on the next submit.
    fireEvent.change(screen.getByLabelText("Namespace"), { target: { value: "other" } });
    expect(screen.queryByLabelText(/AgentClass demo\/demo-agent/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // The new target's conflict list happens to share the same Kind/Name key,
    // now as a Secret — it must render UNTICKED, never pre-ticked from state
    // left over by the previous target.
    const secretRow = (await screen.findByLabelText(/AgentClass other\/demo-agent/)) as HTMLInputElement;
    expect(secretRow.checked).toBe(false);

    // And the POST that produced this second 409 must not have carried the
    // stale key either — an absent "adopt" field, not the old tick.
    const secondCallForm = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(secondCallForm.get("adopt")).toBeNull();
  });

  // Same class as the namespace regression above, for the install-name
  // override: install.Install renames EVERY bundled CR and synthesized
  // Secret via `instance.Rename(renameSet, opts.Name+"-")` before conflict
  // detection runs, so editing `name` changes essentially every object
  // identity the install touches. name="foo" over a CR named "bar-widget"
  // and name="foo-bar" over a CR named "widget" both produce the same
  // rendered name "foo-bar-widget" — a tick made under one name must not
  // authorize a seizure under another.
  it("changing the install name after ticking a conflict clears the tick — no stale adopt rides the next submit", async () => {
    const fetchMock = vi
      .fn()
      // First submit, name unset: one AgentClass conflict.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "demo", name: "foo-bar-widget" }],
          },
          409,
        ),
      )
      // Second submit, after the operator sets an install name override: a
      // DIFFERENT underlying bundled object, renamed under the new prefix,
      // happens to produce the exact SAME Kind/Name key — this time as a
      // Secret.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "demo", name: "foo-bar-widget", secret: true }],
          },
          409,
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const classRow = (await screen.findByLabelText(/AgentClass demo\/foo-bar-widget/)) as HTMLInputElement;
    fireEvent.click(classRow);
    expect(classRow.checked).toBe(true);

    // The operator sets an install name override — every renamed object's
    // identity changes, so this must clear both the stale row and the tick
    // immediately, not just on the next submit.
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "foo-bar" } });
    expect(screen.queryByLabelText(/AgentClass demo\/foo-bar-widget/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    // The new target's conflict list happens to share the same Kind/Name
    // key, now as a Secret — it must render UNTICKED, never pre-ticked from
    // state left over by the previous name.
    const secretRow = (await screen.findByLabelText(/AgentClass demo\/foo-bar-widget/)) as HTMLInputElement;
    expect(secretRow.checked).toBe(false);

    // And the POST that produced this second 409 must not have carried the
    // stale key either — an absent "adopt" field, not the old tick.
    const secondCallForm = (fetchMock.mock.calls[1][1] as RequestInit).body as FormData;
    expect(secondCallForm.get("adopt")).toBeNull();
  });

  // Same class again, for an install-QUESTION ANSWER: a bundle Binding can
  // target metadata.name just as validly as any other path (oap.ParseTarget
  // has no denylist on segment names, and oap.Apply runs before conflict
  // detection), so editing an answer can retarget the same way editing the
  // Name field does. The UI cannot know which bindings a given bundle
  // declares, so it must reset unconditionally on every answer edit.
  it("changing an install-question answer after ticking a conflict clears the tick — no stale adopt rides the next submit", async () => {
    const fetchMock = vi
      .fn()
      // First submit: the bundle needs an answer (the UI has no visibility
      // into what this question's Binding targets).
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "missing required question(s)",
            questions: [{ name: "instanceName", type: "string", prompt: "Instance name" }],
          },
          400,
        ),
      )
      // Second submit, with an answer given: it resolves to a pre-existing
      // AgentClass.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "demo", name: "widget-a" }],
          },
          409,
        ),
      )
      // Third submit, after the operator EDITS the answer: a DIFFERENT
      // underlying object happens to produce the exact SAME Kind/Name key —
      // this time as a Secret.
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "AgentClass", namespace: "demo", name: "widget-a", secret: true }],
          },
          409,
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    const answerInput = await screen.findByLabelText("Instance name *");
    fireEvent.change(answerInput, { target: { value: "widget-a" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    const classRow = (await screen.findByLabelText(/AgentClass demo\/widget-a/)) as HTMLInputElement;
    fireEvent.click(classRow);
    expect(classRow.checked).toBe(true);

    // The operator edits the answer — the questions block and the conflicts
    // block are both on screen at once here, and this edit must clear both
    // the stale row and the tick immediately, exactly like editing a target
    // field does.
    fireEvent.change(answerInput, { target: { value: "widget-b" } });
    expect(screen.queryByLabelText(/AgentClass demo\/widget-a/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Submit answers" }));

    // The new derivation's conflict list happens to share the same Kind/Name
    // key, now as a Secret — it must render UNTICKED, never pre-ticked from
    // state left over by the previous answer.
    const secretRow = (await screen.findByLabelText(/AgentClass demo\/widget-a/)) as HTMLInputElement;
    expect(secretRow.checked).toBe(false);

    // And the POST that produced this third 409 must not have carried the
    // stale key either — an absent "adopt" field, not the old tick.
    const thirdCallForm = (fetchMock.mock.calls[2][1] as RequestInit).body as FormData;
    expect(thirdCallForm.get("adopt")).toBeNull();
  });

  // Defensive test of the documented wire contract: `namespace` is
  // `omitempty` on the Go side and may be absent for a cluster-scoped
  // conflict. This path may be unreachable server-side today (cluster-scoped
  // conflicts are filtered into a plain error before reaching the wire array),
  // but the client must still render correctly if it ever is.
  it("a conflict with namespace omitted renders a bare Kind/Name label", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          {
            error: "install would overwrite pre-existing object(s)",
            conflicts: [{ kind: "ClusterRole", name: "demo-cluster-role" }],
          },
          409,
        ),
      ),
    );

    render(<InstallAgentForm apiBase="/admin/api" onInstalled={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    expect(await screen.findByLabelText("ClusterRole/demo-cluster-role")).toBeTruthy();
  });
});
