import { describe, expect, it } from "vitest";
import { decodeSubject } from "./decodeSubject";

// base64url("alice@example.com") — the canonical form of a platform user subject.
const ALICE = "YWxpY2VAZXhhbXBsZS5jb20";

describe("decodeSubject", () => {
  it("decodes user:<base64url(email)> back to the email", () => {
    expect(decodeSubject(`user:${ALICE}`)).toBe("alice@example.com");
  });

  it("decodes a group:<base64url(email)> subject too", () => {
    // base64url("bob@example.com")
    expect(decodeSubject("group:Ym9iQGV4YW1wbGUuY29t")).toBe("bob@example.com");
  });

  it("returns the raw subject when the encoded part is not an email", () => {
    // base64url("alice") decodes cleanly but has no "@" → keep the raw subject.
    expect(decodeSubject("user:YWxpY2U")).toBe("user:YWxpY2U");
  });

  it("returns the raw subject for a plain (non-encoded) group name", () => {
    expect(decodeSubject("group:platform-ops#member")).toBe("group:platform-ops#member");
  });

  it("returns the raw subject for a service identity", () => {
    expect(decodeSubject("service:hubspot-bot")).toBe("service:hubspot-bot");
  });

  it("returns the raw subject on garbage / non-base64url input", () => {
    expect(decodeSubject("user:!!!not base64!!!")).toBe("user:!!!not base64!!!");
  });

  it("passes an empty subject through unchanged", () => {
    expect(decodeSubject("")).toBe("");
  });
});
