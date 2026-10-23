import { describe, expect, it, vi } from "vitest";

// guides.ts is server-only; the guard throws outside a React server build.
vi.mock("server-only", () => ({}));

const { guideSource } = await import("./guides");

describe("guideSource", () => {
  it("reads a guide that exists", async () => {
    await expect(guideSource("what-is-oap")).resolves.toContain(
      "export const meta",
    );
  });

  it.each(["../../package", "../_manifest", "no-such-guide", ""])(
    "%j: refused, since it names no guide",
    async (slug) => {
      await expect(guideSource(slug)).rejects.toThrow(/no guide named/);
    },
  );
});
