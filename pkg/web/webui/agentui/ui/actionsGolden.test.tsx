import { describe, expect, it } from "vitest";
import type { ActionRequestBody, ActionResponseBody } from "@ap/agentui";
import golden from "./testdata/actions.golden.json";

// testdata/actions.golden.json is the ONE artifact both halves of the
// actions-ENDPOINT seam (J4) read: pkg/web/webui/agentui/actions_golden_internal_test.go
// asserts the REAL actionsHandler's request/response bytes against it; this
// file re-derives the fields through @ap/agentui's ActionRequestBody/
// ActionResponseBody types and asserts them BY NAME, rather than echoing the
// file's own values back at itself.
//
// Asserting by name is what makes this a pin rather than decoration. A
// render-only assertion (does SOMETHING appear on screen) does not catch a
// coordinated rename: a review found exactly that gap on the bootstrap-props
// seam, where a getByRole-only check stayed green through a
// json-tag rename that broke the page for real users. Named field access
// (golden.response.state) fails the instant the Go side and this golden
// rename that key together, because the OLD name this test still asks for
// is simply absent from the regenerated file.
//
// There is deliberately no rendering here. This file's job is narrower: prove
// both languages agree on the WIRE SHAPE of the endpoint, independent of any
// component. The rendering half — a control that POSTs this body and reacts to
// this response — is covered in useActionLifecycle.test.tsx, against the real
// page.

describe("the actions-endpoint request/response wire contract (J4)", () => {
  it("carries the fields ActionRequestBody names, matching what the browser will POST", () => {
    const request = golden.request as ActionRequestBody;
    expect(request.action).toBe("advance");
    expect(request.params?.lead).toBe("l-1");
    expect(request.inputs?.why).toBe("ready");
  });

  it("carries the fields ActionResponseBody names, matching what actionsHandler actually writes", () => {
    const response = golden.response as ActionResponseBody;
    expect(response.state).toBe("submitted");
    expect(response.message).toBe("Advancing the lead.");
  });
});
