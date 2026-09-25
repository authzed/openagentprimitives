import * as React from "react";
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, within } from "@testing-library/react";
import { InteractionCard } from "./InteractionCard";
import type { InteractionAppliedInner, InteractionRequestInner } from "./types";

afterEach(cleanup);

const baseRequest: InteractionRequestInner = {
  agentSessionRef: { namespace: "default", name: "sess-1" },
  category: "credential_link",
  requestRef: "req-1",
  lead: "Connect your account",
  audience: { scope: "requester" },
};

describe("InteractionCard", () => {
  it("renders Lead/Body/Fields", () => {
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          body: "Link the following credentials to continue.",
          fields: [{ label: "Provider", value: "github" }],
        }}
        onDecision={vi.fn()}
      />,
    );
    expect(screen.getByText("Connect your account")).toBeTruthy();
    expect(screen.getByText("Link the following credentials to continue.")).toBeTruthy();
    // The label is now an eyebrow above its value, so it carries no trailing
    // colon. Still asserted by exact text, and the value is still checked
    // separately below — the pair is what proves the field rendered.
    expect(screen.getByText("Provider")).toBeTruthy();
    expect(screen.getByText("github")).toBeTruthy();
  });

  it("renders a link action as an anchor with the given href, not a button", () => {
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          actions: [{ id: "open", label: "Connect", kind: "link", url: "https://link.example.com/x" }],
        }}
        onDecision={vi.fn()}
      />,
    );
    const link = screen.getByRole("link", { name: /Connect/ });
    expect(link.getAttribute("href")).toBe("https://link.example.com/x");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(screen.queryByRole("button", { name: /Connect/ })).toBeNull();
  });

  it("renders a decision action as a button that calls onDecision with requestRef/category/actionId", () => {
    const onDecision = vi.fn();
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          category: "identity_choice",
          actions: [{ id: "agent", label: "Let the agent continue", kind: "decision" }],
        }}
        onDecision={onDecision}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Let the agent continue" }));
    expect(onDecision).toHaveBeenCalledWith("req-1", "identity_choice", "agent");
  });

  it("disables decision buttons after the first click so a slow round-trip can't double-fire", () => {
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          actions: [
            { id: "agent", label: "Agent", kind: "decision" },
            { id: "cancel", label: "Cancel", kind: "decision" },
          ],
        }}
        onDecision={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Agent" }));
    expect(screen.getByRole("button", { name: "Agent" })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveProperty("disabled", true);
  });

  it("renders the excerpt inside a <pre><code> block as literal text, never as markup (untrusted content contract)", () => {
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          excerpt: { label: "Preview", content: "<script>alert('x')</script>" },
        }}
        onDecision={vi.fn()}
      />,
    );
    // The raw string is present as literal text content of a <pre><code>…
    const pre = document.querySelector("pre");
    expect(pre).not.toBeNull();
    expect(pre!.querySelector("code")).not.toBeNull();
    expect(pre!.textContent).toContain("<script>alert('x')</script>");
    // …and it was NOT parsed into an actual <script> element anywhere in the DOM.
    expect(document.querySelector("script[src], script:not([type])")).toBeNull();
  });

  it("shows the resolved outcome instead of actions once `applied` is set, by requestRef", () => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "credential_link",
      requestRef: "req-1",
      outcome: "resolved",
      outcomeText: "GitHub connected",
    };
    render(
      <InteractionCard
        request={{
          ...baseRequest,
          actions: [{ id: "open", label: "Connect", kind: "link", url: "https://link.example.com/x" }],
        }}
        applied={applied}
        onDecision={vi.fn()}
      />,
    );
    expect(screen.getByText("GitHub connected")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Connect/ })).toBeNull();
  });

  // FU-3: identity_choice's resolved outcome shows a friendly label instead
  // of the raw action id that rides the wire in outcomeText (that raw value
  // stays load-bearing for the runner's decision-resume subscriber — see
  // interactionOutcomeText's doc comment — so the mapping is applied here at
  // render time only, never by changing the payload).
  it.each([
    ["agent", "Running as the agent"],
    ["userPassthrough", "Running as you"],
    ["cancel", "Cancelled"],
  ])("shows the friendly label for identity_choice outcomeText=%s", (outcomeText, wantLabel) => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "identity_choice",
      requestRef: "req-1",
      outcome: outcomeText === "cancel" ? "denied" : "approved",
      outcomeText,
    };
    render(
      <InteractionCard request={{ ...baseRequest, category: "identity_choice" }} applied={applied} onDecision={vi.fn()} />,
    );
    expect(screen.getByText(wantLabel)).toBeTruthy();
    expect(screen.queryByText(outcomeText)).toBeNull();
  });

  it("falls back to the raw outcomeText for an unrecognized identity_choice action id", () => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "identity_choice",
      requestRef: "req-1",
      outcome: "approved",
      outcomeText: "not-a-real-action",
    };
    render(
      <InteractionCard request={{ ...baseRequest, category: "identity_choice" }} applied={applied} onDecision={vi.fn()} />,
    );
    expect(screen.getByText("not-a-real-action")).toBeTruthy();
  });

  // Both tool-approval decision handlers resolve with an Outcome and NO
  // OutcomeText, so the constant is all a renderer has to work from. These are
  // the same words channelevents.OutcomeHeadline gives Slack and the chat TUI —
  // the web chat must not be the one surface that shows the raw wire value.
  it.each([
    ["approved", "Approved"],
    ["denied", "Denied"],
    ["expired", "Expired"],
    ["resolved", "Resolved"],
  ])("shows the human headline for outcome=%s carrying no outcomeText", (outcome, wantHeadline) => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "tool_approval",
      requestRef: "req-1",
      outcome,
    };
    render(<InteractionCard request={baseRequest} applied={applied} onDecision={vi.fn()} />);
    expect(screen.getByText(wantHeadline)).toBeTruthy();
    expect(screen.queryByText(outcome)).toBeNull();
  });

  // A renderer must never be the reason a resolution goes silent: an outcome
  // this build has not learned yet is still better shown than swallowed.
  it("passes an unrecognized outcome constant through rather than swallowing it", () => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "tool_approval",
      requestRef: "req-1",
      outcome: "superseded",
    };
    render(<InteractionCard request={baseRequest} applied={applied} onDecision={vi.fn()} />);
    expect(screen.getByText("superseded")).toBeTruthy();
  });

  // Go's OutcomeLabel treats whitespace-only OutcomeText as absent
  // (strings.TrimSpace); `||` alone would render a blank line here.
  it("treats a whitespace-only outcomeText as absent and shows the headline", () => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: baseRequest.agentSessionRef,
      category: "tool_approval",
      requestRef: "req-1",
      outcome: "approved",
      outcomeText: "   ",
    };
    render(<InteractionCard request={baseRequest} applied={applied} onDecision={vi.fn()} />);
    expect(screen.getByText("Approved")).toBeTruthy();
  });

  it("stamps the card with data-request-ref for correlation", () => {
    render(<InteractionCard request={baseRequest} onDecision={vi.fn()} />);
    expect(screen.getByTestId("interaction-card").getAttribute("data-request-ref")).toBe("req-1");
  });
});

// A plan-gate card's What is MULTI-LINE by construction: one line per permission
// in the phase's ceiling, then a line per resource it asks to reach. HTML folds
// newlines into spaces, so the default inline span rendered the whole ceiling
// and every resource run together on one line — legible with three handles,
// unreadable with a real one, and the resource list is the part that matters.
//
// Slack renders these as mrkdwn lines and the TUI as plain text, so webchat was
// the one surface where the same approval read differently.
describe("InteractionCard multi-line fields", () => {
  const planWhat = [
    "perm:fetch:git_repo",
    "perm:push:git_repo",
    "",
    "Also asks to reach:",
    "  git_repo — https://github.com/acme/app",
    "Every resource above is named, so this approval covers them.",
  ].join("\n");

  it("preserves the line structure of a multi-line field value", () => {
    const { container } = render(
      <InteractionCard
        request={{ ...baseRequest, category: "plan_phase", fields: [{ label: "What", value: planWhat }] }}
        onDecision={vi.fn()}
      />,
    );

    // Queried off the element rather than through getByText: the default
    // normalizer collapses whitespace on BOTH sides, so a text query passes
    // whether or not the newlines survive — it cannot see this bug at all.
    const valueEl = Array.from(container.querySelectorAll("span")).find((s) =>
      s.textContent?.includes("Also asks to reach:"),
    );
    expect(valueEl).toBeTruthy();
    expect(valueEl!.textContent).toContain("\n");
    // The rendering half: the newlines are in the DOM either way, and without
    // a pre-wrap the browser folds them into spaces.
    expect(valueEl!.className).toContain("whitespace-pre-wrap");
    expect(container.textContent).toContain("https://github.com/acme/app");
  });

  it("leaves a single-line field on the same row as its label", () => {
    render(
      <InteractionCard
        request={{ ...baseRequest, fields: [{ label: "Provider", value: "github" }] }}
        onDecision={vi.fn()}
      />,
    );
    // Single-line values keep the inline layout every other category relies on;
    // making every field a block would reflow all of them.
    expect(screen.getByText("github").className).not.toContain("block");
  });
});

// The browser is the surface with a design system and the most room, and it was
// the worst place to read an approval: a plan-gate What is a list of phases,
// and it arrived as one newline-joined paragraph rendered in flat grey.
describe("InteractionCard structured fields", () => {
  const structured = {
    agentSessionRef: { namespace: "default", name: "s" },
    category: "plan_phase",
    requestRef: "req-structured",
    lead: "⚠️ Plan approval",
    audience: { scope: "approvers" },
    fields: [
      {
        label: "What",
        value: "Phase 1:\n  Fetch git_repo\nPhase 2:\n  Push git_repo",
        items: [
          { text: "Phase 1", items: [{ text: "Fetch git_repo" }] },
          {
            text: "Phase 2",
            items: [
              { text: "Push git repo", tone: "external" as const, detail: "leaves this session" },
              { text: "reaches git_repo", detail: "https://github.com/demo-org/demo-repo" },
            ],
          },
        ],
      },
    ],
    actions: [{ id: "approve", label: "Approve", kind: "decision", style: "danger" }],
  };

  it("renders each phase as its own row rather than one paragraph", () => {
    render(<InteractionCard request={structured as never} onDecision={() => {}} />);

    expect(screen.getByText("Phase 1")).toBeTruthy();
    expect(screen.getByText("Phase 2")).toBeTruthy();
    expect(screen.getByText("https://github.com/demo-org/demo-repo")).toBeTruthy();

    // The flattened prose must NOT also be printed — showing both would say
    // everything twice, which is how a card stops being read.
    expect(screen.queryByText(/Phase 1:\s+Fetch git_repo/)).toBeNull();
  });

  it("keeps the irreversible step legible without relying on colour", () => {
    render(<InteractionCard request={structured as never} onDecision={() => {}} />);

    // Tone drives emphasis, but the warning is also TEXT: a screenshot, a
    // monochrome display or a copy-paste must still carry the meaning.
    expect(screen.getByText(/leaves this session/)).toBeTruthy();
  });

  it("falls back to the flat value when a field carries no structure", () => {
    const flat = { ...structured, fields: [{ label: "What", value: "perm:read:tracker_issue" }] };
    render(<InteractionCard request={flat as never} onDecision={() => {}} />);

    expect(screen.getByText("perm:read:tracker_issue")).toBeTruthy();
  });
});

describe("InteractionCard emphasis is derived, not decorative", () => {
  const withItems = (items: unknown): InteractionRequestInner =>
    ({
      ...baseRequest,
      category: "plan_phase",
      lead: "Approve this plan",
      fields: [{ label: "What", value: "", items }],
      actions: [{ id: "approve", label: "Approve", kind: "decision", style: "primary" }],
    }) as never;

  it("marks a card whose reach leaves the session, including when the mark is on a NESTED line", () => {
    // The plan gate nests permission lines one level under a phase, so a
    // top-level-only check would miss every external mark it ever sends.
    render(
      <InteractionCard
        request={withItems([{ text: "Phase 1", items: [{ text: "push to a repository", tone: "external" }] }])}
        onDecision={vi.fn()}
      />,
    );
    expect(screen.getByTestId("interaction-card").getAttribute("data-external")).toBe("true");
  });

  it("leaves a card with no external reach unmarked", () => {
    render(
      <InteractionCard
        request={withItems([{ text: "Phase 1", items: [{ text: "read an issue" }] }])}
        onDecision={vi.fn()}
      />,
    );
    expect(screen.getByTestId("interaction-card").getAttribute("data-external")).toBeNull();
  });

  it("numbers the sections but not a standalone statement about the whole card", () => {
    // A top-level line WITH children is a phase; one WITHOUT is the plan gate's
    // coverage line — "does saying yes finish this?" — which is about the card
    // as a whole and would be misread as a third phase if it were numbered.
    render(
      <InteractionCard
        request={withItems([
          { text: "Phase 1", items: [{ text: "read an issue" }] },
          { text: "Phase 2", items: [{ text: "push a fix" }] },
          { text: "Every resource above is named, so this approval covers them.", tone: "muted" },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    // Exactly two numbered chips for two phases — the coverage line gets none.
    expect(within(card).getByText("1")).toBeTruthy();
    expect(within(card).getByText("2")).toBeTruthy();
    expect(within(card).queryByText("3")).toBeNull();
    expect(within(card).getByText(/this approval covers them/)).toBeTruthy();
  });

  it("encodes the tier with BOTH a glyph and a colour", () => {
    render(
      <InteractionCard
        request={withItems([
          {
            text: "Phase 1",
            items: [
              { text: "Read the repository", tone: "readonly" },
              { text: "Push commits", tone: "external" },
            ],
          },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    // Colour alone fails a monochrome display and a red-green deficiency, so
    // each tier also renders its own glyph element — presence of the glyph
    // IS the second, colour-independent encoding.
    expect(within(card).getByTestId("tier-readonly")).toBeTruthy();
    expect(within(card).getByTestId("tier-external")).toBeTruthy();
  });

  it("shows the raw handle on hover, never inline", () => {
    render(
      <InteractionCard
        request={withItems([
          { text: "Phase 1", items: [{ text: "Push commits", tone: "external", hint: "perm:push:git_repo" }] },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).getByTitle("perm:push:git_repo")).toBeTruthy();
    expect(card.textContent).not.toContain("perm:push:git_repo");
  });

  it("omits the hover affordance when a line carries no hint", () => {
    render(
      <InteractionCard
        request={withItems([{ text: "Phase 1", items: [{ text: "Read the repository", tone: "readonly" }] }])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).queryByTitle(/.+/)).toBeNull();
  });
});

describe("InteractionCard resource-instance display", () => {
  const withItems = (items: unknown): InteractionRequestInner =>
    ({
      ...baseRequest,
      category: "plan_phase",
      lead: "Approve this plan",
      fields: [{ label: "What", value: "", items }],
      actions: [{ id: "approve", label: "Approve", kind: "decision", style: "primary" }],
    }) as never;

  // Invariant 1, made unbreakable rather than merely documented: whatever the
  // publisher sent as href, the rendered anchor's visible text must be that
  // SAME string — never `detail`, never `text`, never anything independently
  // computed, so a renderer bug can never make the two diverge.
  it("renders the resource line's link with text byte-identical to its href", () => {
    render(
      <InteractionCard
        request={withItems([
          {
            text: "Phase 1",
            items: [
              {
                text: "reaches demo-org/demo-repo",
                detail: "https://github.com/demo-org/demo-repo",
                icon: "repository",
                href: "https://github.com/demo-org/demo-repo",
              },
            ],
          },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    const anchor = within(card).getByRole("link", { name: "https://github.com/demo-org/demo-repo" });
    expect(anchor.getAttribute("href")).toBe(anchor.textContent);
    expect(anchor.getAttribute("target")).toBe("_blank");
    expect(anchor.getAttribute("rel")).toContain("noopener");
    expect(anchor.getAttribute("rel")).toContain("noreferrer");
  });

  // The derived label and the canonical value are both required, always
  // together — a channel that renders only one line renders the canonical
  // one, but this surface has room for both and must show both whenever a
  // label is present.
  it("shows the canonical line whenever a label is present", () => {
    render(
      <InteractionCard
        request={withItems([
          {
            text: "Phase 1",
            items: [
              {
                text: "reaches demo-org/demo-repo",
                detail: "https://github.com/demo-org/demo-repo",
                href: "https://github.com/demo-org/demo-repo",
              },
            ],
          },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).getByText("reaches demo-org/demo-repo")).toBeTruthy();
    expect(within(card).getByText("https://github.com/demo-org/demo-repo")).toBeTruthy();
  });

  // A resource line with no href (undeclared display, or an ineligible raw
  // value) renders Detail as plain text — no anchor at all.
  it("renders Detail as plain text when no href is present", () => {
    render(
      <InteractionCard
        request={withItems([
          { text: "Phase 1", items: [{ text: "reaches git_repo", detail: "(no target named yet)" }] },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).getByText("(no target named yet)")).toBeTruthy();
    expect(within(card).queryByRole("link", { name: /no target named yet/ })).toBeNull();
  });

  // An unrecognized icon name must render no icon element — never a
  // fallback image, never a guess. The rest of the line still renders.
  it("renders no icon for an unrecognized icon name", () => {
    render(
      <InteractionCard
        request={withItems([
          {
            text: "Phase 1",
            items: [{ text: "reaches demo-org/demo-repo", detail: "x", icon: "not-a-real-icon" }],
          },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).queryByTestId("resource-icon-not-a-real-icon")).toBeNull();
    expect(within(card).getByText("demo-org/demo-repo", { exact: false })).toBeTruthy();
  });

  // A known icon renders its own dedicated glyph element, found by test as
  // `resource-icon-<name>` — mirrors the tier glyphs' `tier-<tone>` pattern.
  it("renders a known icon's own glyph element", () => {
    render(
      <InteractionCard
        request={withItems([
          {
            text: "Phase 1",
            items: [{ text: "reaches demo-org/demo-repo", detail: "x", icon: "repository" }],
          },
        ])}
        onDecision={vi.fn()}
      />,
    );
    const card = screen.getByTestId("interaction-card");
    expect(within(card).getByTestId("resource-icon-repository")).toBeTruthy();
  });
});

// A resolved plan-gate card keeps its whole permission tree — that is the
// record of what was authorized, and deleting it from the transcript would
// make an approval unauditable after the fact. But an approved card is no
// longer a decision surface, and leaving two phases of permissions expanded
// pushes every later message down the transcript. So the tree collapses
// behind a summary that still says what is inside it.
describe("InteractionCard resolved-state collapsing", () => {
  const phaseField = {
    label: "What",
    value: "",
    items: [
      {
        text: "Phase 1",
        items: [
          { text: "Clone or fetch from the remote repository", tone: "readwrite" },
          { text: "reaches demo-org/demo-repo", detail: "https://github.com/demo-org/demo-repo" },
        ],
      },
      { text: "Phase 2", items: [{ text: "Push commits to the remote repository", tone: "external" }] },
    ],
  };

  it("collapses the phase tree behind a summary once the card is approved", () => {
    const applied: InteractionAppliedInner = {
      agentSessionRef: { namespace: "default", name: "sess-1" },
      requestRef: "req-1",
      category: "plan_gate",
      outcome: "approved",
    };
    const { container } = render(
      <InteractionCard
        request={{ ...baseRequest, category: "plan_gate", fields: [phaseField] }}
        applied={applied}
        onDecision={vi.fn()}
      />,
    );
    const details = container.querySelector("details");
    expect(details).toBeTruthy();
    expect((details as HTMLDetailsElement).open).toBe(false);
    // The summary names what is inside, so a reader knows whether to open it.
    expect(within(details as HTMLElement).getByText(/What/i)).toBeTruthy();
    // The content is still PRESENT — collapsed, never dropped.
    expect(within(details as HTMLElement).getByText("Phase 1")).toBeTruthy();
    expect(
      within(details as HTMLElement).getByText("Push commits to the remote repository"),
    ).toBeTruthy();
  });

  it("leaves the phase tree open and uncollapsed while a decision is still pending", () => {
    const { container } = render(
      <InteractionCard
        request={{ ...baseRequest, category: "plan_gate", fields: [phaseField] }}
        onDecision={vi.fn()}
      />,
    );
    expect(container.querySelector("details")).toBeNull();
    expect(screen.getByText("Phase 1")).toBeTruthy();
  });
});
