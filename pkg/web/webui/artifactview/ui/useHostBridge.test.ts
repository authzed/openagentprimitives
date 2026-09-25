import { describe, it, expect, vi } from "vitest";
import { hostBridge } from "./useHostBridge";

describe("hostBridge", () => {
  it("first framing sets iframe src to the host URL; no postMessage", () => {
    const post = vi.fn();
    const iframe = { getAttribute: () => null, setAttribute: vi.fn(), contentWindow: { postMessage: post } };
    const b = hostBridge(iframe as any, "https://sandbox.example");
    b.showRevision({ hostUrl: "https://sandbox.example/artifact-host?ct=T0", contentUrl: "x" });
    expect(iframe.setAttribute).toHaveBeenCalledWith("src", "https://sandbox.example/artifact-host?ct=T0");
    expect(post).not.toHaveBeenCalled();
  });

  it("once framed, a revision swaps via origin-targeted postMessage; src unchanged", () => {
    const post = vi.fn();
    const iframe = { getAttribute: () => "https://sandbox.example/artifact-host?ct=T0", setAttribute: vi.fn(), contentWindow: { postMessage: post } };
    const b = hostBridge(iframe as any, "https://sandbox.example");
    b.showRevision({ hostUrl: "https://sandbox.example/artifact-host?ct=T1", contentUrl: "https://sandbox.example/content?ct=T1" });
    expect(iframe.setAttribute).not.toHaveBeenCalled();
    expect(post).toHaveBeenCalledWith({ ap: "host", cmd: "swap", url: "https://sandbox.example/content?ct=T1" }, "https://sandbox.example");
  });
});
