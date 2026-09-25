import { describe, expect, it } from "vitest";
import { rowId } from "./useResourceRow";

describe("rowId", () => {
  it("joins namespace/name for namespaced rows", () => {
    expect(rowId({ name: "x", namespace: "default", scope: "namespaced", status: "Valid" })).toBe("default/x");
  });
  it("uses the bare name for cluster-scoped rows", () => {
    expect(rowId({ name: "Alice", scope: "cluster", status: "Valid" })).toBe("Alice");
  });
});
