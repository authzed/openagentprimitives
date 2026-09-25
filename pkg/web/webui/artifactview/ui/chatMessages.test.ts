import { describe, it, expect } from "vitest";
import { appendMessage, type ChatMessage } from "./chatMessages";

const m = (role: "agent" | "user", text: string): ChatMessage => ({ role, text, author: "", at: "" });

describe("appendMessage", () => {
  it("appends in arrival order", () => {
    let l: ChatMessage[] = [];
    l = appendMessage(l, m("user", "reworded?"));
    l = appendMessage(l, m("agent", "done"));
    expect(l.map((x) => x.text)).toEqual(["reworded?", "done"]);
  });
  it("bounds history to the last 200", () => {
    let l: ChatMessage[] = [];
    for (let i = 0; i < 250; i++) l = appendMessage(l, m("agent", String(i)));
    expect(l.length).toBe(200);
    expect(l[0].text).toBe("50");
  });
});
