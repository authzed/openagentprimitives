import { describe, expect, it } from "vitest";
import { httpUrl } from "./safeUrl";

describe("httpUrl", () => {
  it("passes an https URL through unchanged", () => {
    expect(httpUrl("https://github.com/acme/team-skills")).toBe("https://github.com/acme/team-skills");
  });

  it("passes a plain http URL through unchanged", () => {
    expect(httpUrl("http://example.com/repo")).toBe("http://example.com/repo");
  });

  it("rejects a javascript: URL (XSS vector) → null", () => {
    expect(httpUrl("javascript:alert(1)")).toBeNull();
  });

  it("rejects a data: URL → null", () => {
    expect(httpUrl("data:text/html,<script>alert(1)</script>")).toBeNull();
  });

  it("rejects an unparseable / relative garbage string → null", () => {
    expect(httpUrl("not a url")).toBeNull();
    expect(httpUrl("/relative/path")).toBeNull();
    expect(httpUrl("")).toBeNull();
  });
});
