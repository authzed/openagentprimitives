import { describe, it, expect } from "vitest";
import { interactRequestFromUIAction, appToolResponseToCallResult } from "./mcpUiActionSend";

describe("interactRequestFromUIAction – flattens @mcp-ui/client's nested UIActionResult to the server's flat wire shape", () => {
  it("flattens a tool action's payload.toolName/params to top-level fields", () => {
    const req = interactRequestFromUIAction({ type: "tool", payload: { toolName: "refresh_data", params: { id: 42 } } });
    expect(req).toEqual({
      kind: "mcp_ui_action",
      payload: { type: "tool", toolName: "refresh_data", params: { id: 42 } },
    });
  });

  it("flattens a prompt action", () => {
    const req = interactRequestFromUIAction({ type: "prompt", payload: { prompt: "Summarize the last quarter" } });
    expect(req).toEqual({ kind: "mcp_ui_action", payload: { type: "prompt", prompt: "Summarize the last quarter" } });
  });

  it("flattens a link action (never opened client-side — routed through the turn like every other variant)", () => {
    const req = interactRequestFromUIAction({ type: "link", payload: { url: "https://example.com/report" } });
    expect(req).toEqual({ kind: "mcp_ui_action", payload: { type: "link", url: "https://example.com/report" } });
  });

  it("flattens an intent action", () => {
    const req = interactRequestFromUIAction({ type: "intent", payload: { intent: "open_settings", params: {} } });
    expect(req).toEqual({ kind: "mcp_ui_action", payload: { type: "intent", intent: "open_settings", params: {} } });
  });

  it("flattens a notify action", () => {
    const req = interactRequestFromUIAction({ type: "notify", payload: { message: "widget finished loading" } });
    expect(req).toEqual({ kind: "mcp_ui_action", payload: { type: "notify", message: "widget finished loading" } });
  });

  it("never carries an artifactId — this is the session-scoped branch", () => {
    const req = interactRequestFromUIAction({ type: "notify", payload: { message: "hi" } }) as Record<string, unknown>;
    expect(req.artifactId).toBeUndefined();
  });
});

describe("appToolResponseToCallResult – maps AppToolCallResponse.status to the widget's CallToolResultLike", () => {
  it("ok: decodes a plain (already-decoded-by-r.json()) result string, threading through the tool's own isError", () => {
    expect(appToolResponseToCallResult({ status: "ok", result: "42 widgets refreshed", isError: false })).toEqual({
      content: [{ type: "text", text: "42 widgets refreshed" }],
      isError: false,
    });
    expect(appToolResponseToCallResult({ status: "ok", result: "boom", isError: true })).toEqual({
      content: [{ type: "text", text: "boom" }],
      isError: true,
    });
  });

  it("ok: decodes a double-JSON-encoded result string (defensive path)", () => {
    expect(appToolResponseToCallResult({ status: "ok", result: JSON.stringify("nested quoted text"), isError: false })).toEqual({
      content: [{ type: "text", text: "nested quoted text" }],
      isError: false,
    });
  });

  it("ok: falls back to JSON.stringify for a non-string result", () => {
    expect(appToolResponseToCallResult({ status: "ok", result: { count: 3 }, isError: false })).toEqual({
      content: [{ type: "text", text: '{"count":3}' }],
      isError: false,
    });
  });

  it("ok: an absent result decodes to empty text rather than throwing", () => {
    expect(appToolResponseToCallResult({ status: "ok", isError: false })).toEqual({
      content: [{ type: "text", text: "" }],
      isError: false,
    });
  });

  it("requires_approval: uses the runner's pending message when present, else a default", () => {
    expect(
      appToolResponseToCallResult({ status: "requires_approval", message: "approval requested — pending; ask in chat for the result" }),
    ).toEqual({
      content: [{ type: "text", text: "approval requested — pending; ask in chat for the result" }],
      isError: true,
    });
    expect(appToolResponseToCallResult({ status: "requires_approval" })).toEqual({
      content: [{ type: "text", text: "Approval requested — pending; ask in chat for the result." }],
      isError: true,
    });
  });

  it("denied: uses the server's message when present, else a default", () => {
    expect(appToolResponseToCallResult({ status: "denied", message: "widget scope excludes this tool" })).toEqual({
      content: [{ type: "text", text: "widget scope excludes this tool" }],
      isError: true,
    });
    expect(appToolResponseToCallResult({ status: "denied" })).toEqual({
      content: [{ type: "text", text: "Not authorized to run this action." }],
      isError: true,
    });
  });

  it("rate_limited: uses the server's message when present, else a default", () => {
    expect(appToolResponseToCallResult({ status: "rate_limited", message: "try again in 30s" })).toEqual({
      content: [{ type: "text", text: "try again in 30s" }],
      isError: true,
    });
    expect(appToolResponseToCallResult({ status: "rate_limited" })).toEqual({
      content: [{ type: "text", text: "Rate limit reached; try again shortly." }],
      isError: true,
    });
  });

  it("error: uses the server's message when present, else a default", () => {
    expect(appToolResponseToCallResult({ status: "error", message: "runner unreachable" })).toEqual({
      content: [{ type: "text", text: "runner unreachable" }],
      isError: true,
    });
    expect(appToolResponseToCallResult({ status: "error" })).toEqual({
      content: [{ type: "text", text: "The action could not be completed." }],
      isError: true,
    });
  });

  it("unknown status: falls back to the same default as error", () => {
    expect(appToolResponseToCallResult({ status: "something_new" })).toEqual({
      content: [{ type: "text", text: "The action could not be completed." }],
      isError: true,
    });
  });
});
