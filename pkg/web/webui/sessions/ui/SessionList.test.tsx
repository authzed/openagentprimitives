import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

import { SessionList } from "./SessionList";
import type { SessionRow } from "./types";

afterEach(cleanup);

// SessionShell.test.tsx drives this component through the golden, which is
// what pins the wire. These rows cover the branches that golden cannot reach
// — it carries exactly two rows, both live, and a plural notice count — so
// the singular copy, the ended marker and the empty state would otherwise
// ship unexercised. Fixture names are fabrications, never read from
// `examples/`.
function row(overrides: Partial<SessionRow> = {}): SessionRow {
  return {
    ns: "demo-ns",
    name: "alpha",
    class: "demo-agent",
    title: "Demo Agent",
    phase: "Active",
    awaitingHuman: false,
    ended: false,
    ...overrides,
  };
}

describe("SessionList — rows", () => {
  it("addresses each session by URL, encoding the separator so a namespace is never mistaken for a path segment", () => {
    render(<SessionList sessions={[row(), row({ name: "beta" })]} />);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Falpha",
    );
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Fbeta",
    );
  });

  // The row shows what the agent IS, never the session's Kubernetes object
  // name — the same rule the shell's chat arm follows.
  it("shows the agent's title, not the session's object name", () => {
    const el = render(<SessionList sessions={[row({ name: "pm-agent-a1b2c3" })]} />);
    expect(el.container.textContent).toContain("Demo Agent");
    expect(el.container.textContent).not.toContain("pm-agent-a1b2c3");
  });

  it("renders each row's human phase copy verbatim, never a second vocabulary", () => {
    render(<SessionList sessions={[row({ phase: "Waiting for you", awaitingHuman: true })]} />);
    expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/alpha")).toHaveTextContent("Waiting for you");
  });

  // The ended branch has no golden row, so this is its only exercise. Ended
  // rows are hidden until the toggle reveals them, so the test goes through
  // the toggle the way a viewer would.
  it("renders an ended session without throwing, still addressable and still labelled", () => {
    render(<SessionList sessions={[row({ ended: true, phase: "Finished" })]} />);
    fireEvent.click(screen.getByTestId("session-shell-toggle-ended"));
    const el = screen.getByTestId("session-shell-session-row-demo-ns/alpha");
    expect(el).toHaveAttribute("href", "/sessions?session=demo-ns%2Falpha");
    expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/alpha")).toHaveTextContent("Finished");
  });

  // startedAt is optional on the wire. A row without one must not invent a
  // recency ("just now" for a session whose start time is simply unknown is a
  // fabrication), and a row with one must show it — which is also what makes
  // the golden's startedAt a field the browser actually READS.
  it("shows a recency label for a row that has a start time, and none for a row that does not", () => {
    render(
      <SessionList
        sessions={[
          row({ name: "recent", startedAt: new Date(Date.now() - 5 * 60_000).toISOString() }),
          row({ name: "unknown" }),
        ]}
      />,
    );
    expect(screen.getByTestId("session-shell-session-row-age-demo-ns/recent")).toHaveTextContent("5m");
    expect(screen.queryByTestId("session-shell-session-row-age-demo-ns/unknown")).toBeNull();
  });

  it("marks only the selected row as the current page", () => {
    render(<SessionList sessions={[row(), row({ name: "beta" })]} selectedNs="demo-ns" selectedName="beta" />);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).not.toHaveAttribute("aria-current");
  });
});

describe("SessionList — an incomplete list says so", () => {
  // An empty list that is really a partially-failed list would be
  // indistinguishable from "you have no sessions" — the silent-failure shape
  // this repo forbids. The golden pins the plural branch; the singular one
  // has no other exercise.
  it("states a single unloadable session in the singular", () => {
    render(<SessionList sessions={[row()]} notices={{ unavailable: 1, truncated: false, bootstrapUnavailable: false }} />);
    expect(screen.getByTestId("session-shell-list-notices")).toHaveTextContent(
      "1 session could not be loaded and is not shown.",
    );
  });

  it("states several in the plural", () => {
    render(<SessionList sessions={[row()]} notices={{ unavailable: 3, truncated: false, bootstrapUnavailable: false }} />);
    expect(screen.getByTestId("session-shell-list-notices")).toHaveTextContent(
      "3 sessions could not be loaded and are not shown.",
    );
  });

  it("states truncation as its own line, alongside an unavailable count", () => {
    render(<SessionList sessions={[row()]} notices={{ unavailable: 2, truncated: true, bootstrapUnavailable: false }} />);
    const notices = within(screen.getByTestId("session-shell-list-notices"));
    expect(notices.getByText("2 sessions could not be loaded and are not shown.")).toBeInTheDocument();
    expect(notices.getByText("Showing your most recent sessions only.")).toBeInTheDocument();
  });

  it("renders no notice region at all when the list is complete", () => {
    render(<SessionList sessions={[row()]} notices={{ unavailable: 0, truncated: false, bootstrapUnavailable: false }} />);
    expect(screen.queryByTestId("session-shell-list-notices")).toBeNull();
  });

  // bootstrapUnavailable narrows a DIFFERENT set from the two above: not the
  // session list, but the agents New chat can offer. These two cases are what
  // make the Go golden's `bootstrapUnavailable` key load-bearing rather than
  // decorative — without a TS side that reads it, the golden would pin only
  // that Go emits the key, never that the browser does anything with it.
  it("states an unevaluated start-set arm as its own line", () => {
    render(
      <SessionList
        sessions={[row()]}
        notices={{ unavailable: 2, truncated: false, bootstrapUnavailable: true }}
      />,
    );
    const notices = within(screen.getByTestId("session-shell-list-notices"));
    expect(notices.getByText("2 sessions could not be loaded and are not shown.")).toBeInTheDocument();
    expect(notices.getByText("Some agents may be missing from New chat. Try again in a moment.")).toBeInTheDocument();
  });

  it("states it even when the session list itself is complete", () => {
    // The session list is whole here — only the start set is short. Asserting
    // this case separately is what proves the line is not merely riding along
    // with an unavailable count.
    render(
      <SessionList
        sessions={[row()]}
        notices={{ unavailable: 0, truncated: false, bootstrapUnavailable: true }}
      />,
    );
    expect(screen.getByTestId("session-shell-list-notices")).toHaveTextContent(
      "Some agents may be missing from New chat. Try again in a moment.",
    );
  });

  it("renders the empty state, and still says what is missing, when every session failed to load", () => {
    render(<SessionList sessions={[]} notices={{ unavailable: 2, truncated: false, bootstrapUnavailable: false }} />);
    expect(screen.getByTestId("session-shell-empty-list")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-list-notices")).toHaveTextContent(
      "2 sessions could not be loaded and are not shown.",
    );
  });
});

describe("SessionList — entry point, not archive", () => {
  const live = row({ name: "chat-agent-8bf8779c", title: "chat-agent" });
  const endedA = row({ name: "chat-agent-11aa22bb", title: "chat-agent", ended: true, phase: "Finished" });
  const endedB = row({ name: "pirate-3c3c3c3c", title: "pirate", ended: true, phase: "Finished" });

  it("hides ended sessions by default and says how many it hid", () => {
    render(<SessionList sessions={[live, endedA, endedB]} />);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/chat-agent-8bf8779c")).toBeInTheDocument();
    expect(screen.queryByTestId("session-shell-session-row-demo-ns/chat-agent-11aa22bb")).toBeNull();
    expect(screen.getByTestId("session-shell-toggle-ended")).toHaveTextContent("Show 2 ended sessions");
  });

  it("reveals them on toggle and offers to hide them again", () => {
    render(<SessionList sessions={[live, endedA, endedB]} />);
    fireEvent.click(screen.getByTestId("session-shell-toggle-ended"));
    expect(screen.getByTestId("session-shell-session-row-demo-ns/pirate-3c3c3c3c")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-toggle-ended")).toHaveTextContent("Hide ended sessions");
  });

  // A deep link to an ended session must land on a list that contains it.
  it("always shows the selected session, even when ended and hidden", () => {
    render(<SessionList sessions={[live, endedA, endedB]} selectedNs="demo-ns" selectedName="pirate-3c3c3c3c" />);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/pirate-3c3c3c3c")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-toggle-ended")).toHaveTextContent("Show 1 ended session");
  });

  it("shows no toggle when nothing is ended", () => {
    render(<SessionList sessions={[live]} />);
    expect(screen.queryByTestId("session-shell-toggle-ended")).toBeNull();
  });

  it("tags rows with the name suffix only when two visible rows share a title", () => {
    render(<SessionList sessions={[live, endedA, endedB]} />);
    // Only one chat-agent is visible: no tag.
    expect(screen.queryByTestId("session-shell-session-row-tag-demo-ns/chat-agent-8bf8779c")).toBeNull();
    fireEvent.click(screen.getByTestId("session-shell-toggle-ended"));
    // Now two chat-agents are visible: both tagged, the lone pirate is not.
    expect(screen.getByTestId("session-shell-session-row-tag-demo-ns/chat-agent-8bf8779c")).toHaveTextContent("8bf8779c");
    expect(screen.getByTestId("session-shell-session-row-tag-demo-ns/chat-agent-11aa22bb")).toHaveTextContent("11aa22bb");
    expect(screen.queryByTestId("session-shell-session-row-tag-demo-ns/pirate-3c3c3c3c")).toBeNull();
  });
});
