import { describe, it, expect } from "vitest";
import { interactRequestFromBundle } from "./annotationSend";

describe("interactRequestFromBundle", () => {
  it("stamps the shell's artifactId (ignores any id on the bundle)", () => {
    const bundle = { artifactId: "", annotations: [{ index: 1, comment: "reword", target: "element" } as any] };
    const req = interactRequestFromBundle(bundle, "artifact-9");
    expect(req.kind).toBe("annotation_batch");
    expect(req.artifactId).toBe("artifact-9"); // from the shell, not the sandbox
    expect(req.payload.annotations.length).toBe(1);
  });
});
