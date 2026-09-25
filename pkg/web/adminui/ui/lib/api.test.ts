import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError, OapInstallConflictsError, OapInstallQuestionsError, getCluster, getConfigDetail,
  getConfigResource, oapChannelHandoff, oapChannelSetup, oapInstallByFile, oapInstallByRef,
  type ClusterInfo, type ResourceDetail,
} from "./api";

afterEach(() => vi.restoreAllMocks());

describe("getCluster", () => {
  it("GETs /cluster and returns the parsed ClusterInfo", async () => {
    const info: ClusterInfo = {
      name: "ap-prod",
      type: "gke",
      consoleURL: "https://console.cloud.google.com/kubernetes/clusters/details/us-central1-a/ap-prod?project=demo",
      distribution: "",
    };
    const spy = vi.fn(async () => new Response(JSON.stringify(info), { status: 200 }));
    vi.stubGlobal("fetch", spy);

    const got = await getCluster("/admin/api");

    expect(spy).toHaveBeenCalledWith("/admin/api/cluster");
    expect(got.type).toBe("gke");
    expect(got.name).toBe("ap-prod");
    expect(got.consoleURL).toContain("console.cloud.google.com");
  });

  it("rejects with the server error message on a non-2xx response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "boom" }), { status: 500 })),
    );
    await expect(getCluster("/admin/api")).rejects.toThrow("boom");
  });
});

describe("getConfigDetail", () => {
  it("GETs /config/{resource}/{id} and returns the parsed ResourceDetail", async () => {
    const detail: ResourceDetail = {
      name: "support-bot",
      namespace: "default",
      scope: "namespaced",
      status: "Valid",
      description: "The support agent.",
      manageCmd: "kubectl edit agentclass support-bot -n default",
      sections: [
        { id: "prompt", label: "Prompt", kind: "text", text: "You are helpful." },
        {
          id: "tools",
          label: "Tools",
          kind: "list",
          items: [{ title: "github", subtitle: "mcpserver → gh", link: { entity: "tool", id: "default/gh" } }],
        },
        {
          id: "identity",
          label: "Identity",
          kind: "fields",
          fields: [{ label: "Agent identity", value: "bot", link: { entity: "identity", id: "default/bot" } }],
        },
      ],
    };
    const spy = vi.fn(async () => new Response(JSON.stringify(detail), { status: 200 }));
    vi.stubGlobal("fetch", spy);

    const got = await getConfigDetail("/admin/api", "agents", "default/support-bot");

    expect(spy).toHaveBeenCalledWith("/admin/api/config/agents/default/support-bot");
    expect(got.name).toBe("support-bot");
    expect(got.sections).toHaveLength(3);
    expect(got.sections?.[0].kind).toBe("text");
    expect(got.sections?.[1].items?.[0].link?.id).toBe("default/gh");
  });

  it("rejects with the server error message on a non-2xx response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "not found" }), { status: 404 })),
    );
    await expect(getConfigDetail("/admin/api", "agents", "default/missing")).rejects.toThrow("not found");
  });
});

describe("getConfigResource", () => {
  // Defense-in-depth against an admind projector that marshals its empty rows
  // slice as JSON `null` (a `var rows []ResourceRow` never appended to) rather
  // than `[]` — every ResourceSection consumer iterates the result directly,
  // and a bare `null` throws client-side ("Symbol.iterator on null").
  it("normalizes a null response body to an empty array", async () => {
    const spy = vi.fn(async () => new Response("null", { status: 200 }));
    vi.stubGlobal("fetch", spy);

    const got = await getConfigResource("/admin/api", "skills");

    expect(spy).toHaveBeenCalledWith("/admin/api/config/skills");
    expect(got).toEqual([]);
  });
});

describe("oapInstallByRef / oapInstallByFile", () => {

  it("classifies a questionless channel-only 400 as an actionable aggregate decision", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      error: "required channel setup is incomplete",
      questions: [],
      channels: [{ agentPath: "reviewer", name: "private-reviewer-chat", kind: "fake", role: "both", status: "ask", setupToken: "opaque-token", questions: [] }],
      warnings: ["channel warning"],
    }), { status: 400 })));

    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "default" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OapInstallQuestionsError);
    const decision = err as OapInstallQuestionsError;
    expect(decision.questions).toEqual([]);
    expect(decision.channels[0].setupToken).toBe("opaque-token");
    expect(decision.warnings).toEqual(["channel warning"]);
  });

  it("preserves simultaneous path-qualified questions, conflicts, warnings, and channels", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      error: "missing required question(s)",
      questions: [{ agentPath: "reviewer > helper", name: "region", type: "string", prompt: "Region" }],
      conflicts: [{ agentPath: "reviewer", kind: "ConfigMap", namespace: "default", name: "private-config" }],
      channels: [{ agentPath: "reviewer", name: "private-chat", kind: "fake", role: "both", status: "ask", setupToken: "opaque-token" }],
      warnings: ["capacity warning"],
    }), { status: 400 })));

    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "default" }).catch((e: unknown) => e) as OapInstallQuestionsError;
    expect(err.questions[0].agentPath).toBe("reviewer > helper");
    expect(err.conflicts[0].agentPath).toBe("reviewer");
    expect(err.channels[0].agentPath).toBe("reviewer");
    expect(err.warnings).toEqual(["capacity warning"]);
  });

  it("posts channel setup and handoff without putting the opaque token in the URL", async () => {
    const spy = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ name: "private-chat", namespace: "default", kind: "fake", agentClass: "private", staged: true }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ setupToken: "opaque-token", explain: "Continue", url: "https://provider.example/start", formFields: { manifest: "safe-public-form" } }), { status: 200 }));
    vi.stubGlobal("fetch", spy);

    await oapChannelSetup("/admin/api", "opaque-token", { interval: "@daily" });
    await oapChannelHandoff("/admin/api", "opaque-token", { organization: "test-org" });

    expect(spy.mock.calls[0][0]).toBe("/admin/api/agents/channel-setup");
    expect(JSON.parse((spy.mock.calls[0][1] as RequestInit).body as string)).toEqual({ setupToken: "opaque-token", answers: { interval: "@daily" } });
    expect(spy.mock.calls[1][0]).toBe("/admin/api/agents/channel-handoff");
    expect(JSON.parse((spy.mock.calls[1][1] as RequestInit).body as string)).toEqual({ setupToken: "opaque-token", answers: { organization: "test-org" } });
  });
  it("oapInstallByRef: POSTs a JSON body and resolves the install result on 200", async () => {
    const spy = vi.fn(async () =>
      new Response(JSON.stringify({ name: "support-bot", appliedKinds: ["AgentClass"], secretsCreated: 1 }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", spy);

    const result = await oapInstallByRef("/admin/api", {
      ref: "ghcr.io/acme/support-bot:1.0.0",
      namespace: "default",
      values: { githubToken: "fake-token-value" },
    });

    expect(spy).toHaveBeenCalledWith(
      "/admin/api/agents/oap-install",
      expect.objectContaining({ method: "POST", headers: { "Content-Type": "application/json" } }),
    );
    const sentBody = JSON.parse((spy.mock.calls[0][1] as RequestInit).body as string);
    expect(sentBody).toEqual({
      ref: "ghcr.io/acme/support-bot:1.0.0",
      namespace: "default",
      values: { githubToken: "fake-token-value" },
    });
    expect(result.name).toBe("support-bot");
    expect(result.secretsCreated).toBe(1);
  });

  it("oapInstallByFile: POSTs multipart/form-data carrying the file + namespace + values", async () => {
    const spy = vi.fn(async () =>
      new Response(JSON.stringify({ name: "support-bot", appliedKinds: ["AgentClass"], secretsCreated: 0 }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", spy);
    const file = new File(["fake-bundle-bytes"], "agent.oap");

    await oapInstallByFile("/admin/api", file, { namespace: "default", name: "my-bot", values: { repos: "a,b" } });

    expect(spy).toHaveBeenCalledWith("/admin/api/agents/oap-install", expect.objectContaining({ method: "POST" }));
    const sentForm = (spy.mock.calls[0][1] as RequestInit).body as FormData;
    expect(sentForm.get("file")).toBeInstanceOf(File);
    expect(sentForm.get("namespace")).toBe("default");
    expect(sentForm.get("name")).toBe("my-bot");
    expect(JSON.parse(sentForm.get("values") as string)).toEqual({ repos: "a,b" });
  });

  it("400 with a non-empty questions list throws OapInstallQuestionsError carrying the typed schema", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            error: "missing required question(s)",
            questions: [{ name: "githubToken", type: "secret", prompt: "GitHub PAT" }],
          }),
          { status: 400 },
        ),
      ),
    );

    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "default" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OapInstallQuestionsError);
    const qErr = err as OapInstallQuestionsError;
    expect(qErr.questions).toEqual([{ name: "githubToken", type: "secret", prompt: "GitHub PAT" }]);
  });

  it("403 throws a plain ApiError (not a questions error)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "forbidden" }), { status: 403 })),
    );
    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "default" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).not.toBeInstanceOf(OapInstallQuestionsError);
    expect((err as ApiError).status).toBe(403);
  });

  it("a plain 400 (no questions) throws ApiError with the server's message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "disallowed resource kind: Pod" }), { status: 400 })),
    );
    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "default" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).not.toBeInstanceOf(OapInstallQuestionsError);
    expect((err as Error).message).toBe("disallowed resource kind: Pod");
  });

  it("409 with a non-empty conflicts list throws OapInstallConflictsError carrying the conflicts", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            error: "install would overwrite pre-existing object(s)",
            conflicts: [
              { kind: "AgentClass", namespace: "demo", name: "demo-agent" },
              { kind: "Secret", namespace: "demo", name: "demo-token", secret: true },
            ],
          }),
          { status: 409 },
        ),
      ),
    );

    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "demo" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OapInstallConflictsError);
    const ce = err as OapInstallConflictsError;
    expect(ce.conflicts).toHaveLength(2);
    expect(ce.conflicts[1].secret).toBe(true);
  });

  // Analogue of "a plain 400 (no questions) throws ApiError" above: a 409
  // whose `conflicts` is empty (or absent — both take the same `body.conflicts
  // && body.conflicts.length > 0` false branch in postOapInstall) must not be
  // misclassified as OapInstallConflictsError; it has nothing for the form's
  // confirm step to render, so it must surface as a plain, visible error
  // instead of a step with zero rows.
  it("409 with an empty conflicts array throws a plain ApiError (not OapInstallConflictsError)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({ error: "install would overwrite pre-existing object(s)", conflicts: [] }),
          { status: 409 },
        ),
      ),
    );
    const err = await oapInstallByRef("/admin/api", { ref: "x", namespace: "demo" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).not.toBeInstanceOf(OapInstallConflictsError);
    expect((err as ApiError).status).toBe(409);
  });

  it("oapInstallByFile: sends the adopt list as a JSON form field", async () => {
    const spy = vi.fn(async () =>
      new Response(JSON.stringify({ name: "demo-agent", appliedKinds: [], secretsCreated: 0 }), { status: 200 }),
    );
    vi.stubGlobal("fetch", spy);
    const file = new File(["x"], "demo.oap");

    await oapInstallByFile("/admin/api", file, { namespace: "demo", adopt: ["AgentClass/demo-agent"] });

    const sentForm = (spy.mock.calls[0][1] as RequestInit).body as FormData;
    expect(JSON.parse(sentForm.get("adopt") as string)).toEqual(["AgentClass/demo-agent"]);
  });
});
