import { describe, it, expect } from "vitest";
import { parseLiveMessage, wsURLFrom } from "./useLiveSocket";

describe("parseLiveMessage", () => {
  it("parses a valid snapshot frame", () => {
    const msg = parseLiveMessage(JSON.stringify({ type: "snapshot", revisions: [] }));
    expect(msg?.type).toBe("snapshot");
  });
  it("returns null on malformed JSON", () => {
    expect(parseLiveMessage("not json")).toBeNull();
  });
  it("returns null when type is missing", () => {
    expect(parseLiveMessage(JSON.stringify({ revisions: [] }))).toBeNull();
  });
});

describe("wsURLFrom", () => {
  it("uses wss for https and carries the search params", () => {
    expect(wsURLFrom({ protocol: "https:", host: "x.example", search: "?d=a&sig=b" }))
      .toBe("wss://x.example/artifact-view/ws?d=a&sig=b");
  });
  it("uses ws for http", () => {
    expect(wsURLFrom({ protocol: "http:", host: "localhost:8080", search: "" }))
      .toBe("ws://localhost:8080/artifact-view/ws");
  });
});
