import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { SectionRenderer } from "./SectionRenderer";
import type { Section } from "../../lib/api";

afterEach(cleanup);

describe("SectionRenderer", () => {
  it("fields: renders a linked field as an EntityLink to the target detail route", () => {
    const section: Section = {
      id: "identity",
      label: "Identity",
      kind: "fields",
      fields: [
        { label: "Identity mode", value: "per_session" },
        { label: "Agent identity", value: "support-bot-id", link: { entity: "identity", id: "default/support-bot-id" } },
      ],
    };
    render(<SectionRenderer section={section} />);

    // plain field value shown
    expect(screen.getByText("per_session")).toBeTruthy();
    // linked field is a real anchor to the entity detail route
    const link = (screen.getByText("support-bot-id")).closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/identity/default/support-bot-id");
  });

  it("fields: an unknown entity slug degrades to plain text (no anchor)", () => {
    const section: Section = {
      id: "x",
      label: "X",
      kind: "fields",
      fields: [{ label: "Bogus", value: "nope", link: { entity: "not-an-entity", id: "whatever" } }],
    };
    render(<SectionRenderer section={section} />);
    const node = screen.getByText("nope");
    expect(node.closest("a")).toBeNull();
  });

  it("fields: a 'Kind' label renders its value as a color-coded KindBadge", () => {
    const section: Section = {
      id: "overview",
      label: "Overview",
      kind: "fields",
      fields: [
        { label: "Kind", value: "slack" },
        { label: "Role", value: "input" },
      ],
    };
    render(<SectionRenderer section={section} />);

    // The kind value is wrapped in a KindBadge carrying a stable color swatch.
    const kind = screen.getByText("slack");
    expect(kind.querySelector('span[aria-hidden="true"][style*="background-color"]')).toBeTruthy();
    // A non-kind field stays plain text — no color swatch.
    const role = screen.getByText("input");
    expect(role.querySelector('span[aria-hidden="true"]')).toBeNull();
  });

  it("fields: an http(s) URL value renders as a safe external anchor", () => {
    const section: Section = {
      id: "source",
      label: "Source",
      kind: "fields",
      fields: [{ label: "Repo", value: "https://github.com/acme/skills" }],
    };
    render(<SectionRenderer section={section} />);
    const link = screen.getByText("https://github.com/acme/skills").closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://github.com/acme/skills");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer nofollow");
  });

  it("fields: an explicit href renders value as the anchor TEXT (short SHA → full URL)", () => {
    const section: Section = {
      id: "source",
      label: "Source",
      kind: "fields",
      fields: [
        { label: "Commit", value: "a1b2c3d", href: "https://github.com/acme/skills/commit/a1b2c3d4e5f6" },
      ],
    };
    render(<SectionRenderer section={section} />);
    // The SHORT SHA is the visible link text; the href is the FULL github URL.
    const link = screen.getByText("a1b2c3d").closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://github.com/acme/skills/commit/a1b2c3d4e5f6");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer nofollow");
  });

  it("fields: a javascript: href degrades to plain text (never a clickable script)", () => {
    const section: Section = {
      id: "x",
      label: "X",
      kind: "fields",
      // eslint-disable-next-line no-script-url
      fields: [{ label: "Commit", value: "a1b2c3d", href: "javascript:alert(1)" }],
    };
    render(<SectionRenderer section={section} />);
    expect(screen.getByText("a1b2c3d").closest("a")).toBeNull();
  });

  it("fields: a javascript: value stays plain text (never a clickable href)", () => {
    const section: Section = {
      id: "x",
      label: "X",
      kind: "fields",
      // eslint-disable-next-line no-script-url
      fields: [{ label: "Danger", value: "javascript:alert(1)" }],
    };
    render(<SectionRenderer section={section} />);
    const node = screen.getByText("javascript:alert(1)");
    expect(node.closest("a")).toBeNull();
  });

  it("fields: an 'oap agent pull …' value renders as a copy-to-clipboard command, not plain text", () => {
    const pullCmd = "oap agent pull ghcr.io/acme/support-bot@sha256:deadbeef -o support-bot.oap";
    const section: Section = {
      id: "oap",
      label: "OAP Bundle",
      kind: "fields",
      fields: [
        { label: "Source", value: "ghcr.io/acme/support-bot" },
        { label: "Pull command", value: pullCmd },
      ],
    };
    render(<SectionRenderer section={section} />);

    // The command renders in a monospace box carrying the FULL text…
    expect(screen.getByText(pullCmd)).toBeTruthy();
    // …plus a copy affordance (not just an inert span like "Source" above).
    expect(screen.getByRole("button", { name: "Copy command" })).toBeTruthy();
    expect(screen.getByText("ghcr.io/acme/support-bot").closest("button")).toBeNull();
  });

  it("text: renders the block content verbatim", () => {
    const section: Section = { id: "prompt", label: "Prompt", kind: "text", text: "You are a helpful assistant." };
    render(<SectionRenderer section={section} />);
    expect(screen.getByText("You are a helpful assistant.")).toBeTruthy();
  });

  it("list: renders items with title, subtitle, an EntityLink, and badge chips", () => {
    const section: Section = {
      id: "tools",
      label: "Tools",
      kind: "list",
      items: [
        {
          title: "github",
          subtitle: "mcpserver → gh",
          link: { entity: "tool", id: "default/gh" },
          badges: [{ key: "kind", value: "mcpserver" }],
        },
        { title: "read_files", badges: [{ key: "kind", value: "toolspec" }] },
      ],
    };
    render(<SectionRenderer section={section} />);

    const link = (screen.getByText("github")).closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/tool/default/gh");
    expect(screen.getByText("mcpserver → gh")).toBeTruthy();
    expect(screen.getByText("mcpserver")).toBeTruthy();
    // a linkless item still renders its title + badge
    expect(screen.getByText("read_files")).toBeTruthy();
    expect(screen.getByText("toolspec")).toBeTruthy();
  });

  // A list section's `text` is a notice about the LIST — a backend that could
  // read only part of it. Both halves must render: the rows alone present a
  // short list as a complete one, the notice alone hides what IS known.
  it("list: a partial-read notice renders ABOVE the rows it qualifies", () => {
    const section: Section = {
      id: "directoryidentities",
      label: "Directory identities",
      kind: "list",
      text: "This list is INCOMPLETE — some sources could not be read: GitHub (github_org#member): boom",
      items: [{ title: "slack_user:U0FKE", subtitle: "asserted by Slack · user" }],
    };
    const { container } = render(<SectionRenderer section={section} />);

    const notice = screen.getByText(/INCOMPLETE/);
    const row = screen.getByText("slack_user:U0FKE");
    expect(notice).toBeTruthy();
    expect(row).toBeTruthy();
    // ABOVE is the claim in this test's name, so assert document order rather
    // than mere co-presence: a notice rendered BELOW the rows it qualifies is
    // read after the list it was meant to caveat, which is most of the way back
    // to not having it.
    expect(
      notice.compareDocumentPosition(row) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(container.textContent?.indexOf("INCOMPLETE")).toBeLessThan(
      container.textContent!.indexOf("slack_user:U0FKE"),
    );
  });

  // The worst case: nothing could be read at all. "Nothing here." would be the
  // wrong answer — it is a claim about the data, not about the read — so a
  // notice replaces it rather than sitting beside it.
  it("list: a notice with no rows replaces the empty placeholder", () => {
    const section: Section = {
      id: "directoryidentities",
      label: "Directory identities",
      kind: "list",
      text: "This list is INCOMPLETE — some sources could not be read: GitHub (github_org#member): boom",
      items: [],
    };
    render(<SectionRenderer section={section} />);

    expect(screen.getByText(/INCOMPLETE/)).toBeTruthy();
    expect(screen.queryByText("Nothing here.")).toBeNull();
  });

  // And the mirror: no notice, no rows is still the plain empty state.
  it("list: no notice and no rows is the ordinary empty state", () => {
    const section: Section = { id: "tools", label: "Tools", kind: "list", items: [] };
    render(<SectionRenderer section={section} />);

    expect(screen.getByText("Nothing here.")).toBeTruthy();
  });

  // A synced repository row: the resolved name is the anchor text, the full
  // URL is the target, and the raw forge id stays readable in the subtitle.
  it("list: an item href makes the title an external link while the raw id stays visible", () => {
    const section: Section = {
      id: "directoryscopes",
      label: "Scopes",
      kind: "list",
      items: [
        {
          title: "demo-org/widgets",
          subtitle: "github_repo:1005857813 · synced by GitHub",
          href: "https://github.com/demo-org/widgets",
          badges: [{ key: "source", value: "GitHub" }],
        },
      ],
    };
    render(<SectionRenderer section={section} />);

    const link = screen.getByText("demo-org/widgets").closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://github.com/demo-org/widgets");
    expect(link.getAttribute("rel")).toContain("noopener");
    expect(screen.getByText("github_repo:1005857813 · synced by GitHub")).toBeTruthy();
  });

  // An entity link is an in-console route and a href is an outbound one; a row
  // carrying both must navigate inside the console, matching FieldValue's own
  // precedence so the two never disagree about what a link means.
  it("list: an entity link wins over an external href", () => {
    const section: Section = {
      id: "tools",
      label: "Tools",
      kind: "list",
      items: [{ title: "github", link: { entity: "tool", id: "default/gh" }, href: "https://example.com/elsewhere" }],
    };
    render(<SectionRenderer section={section} />);

    const link = screen.getByText("github").closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/tool/default/gh");
  });

  // The same scheme guard the field path has. A decoded object id is not a
  // value this console authored, so a hostile scheme reaching the href field
  // must render as inert text rather than as a live link.
  it("list: a javascript: href never becomes a clickable link", () => {
    const section: Section = {
      id: "directoryscopes",
      label: "Scopes",
      kind: "list",
      // eslint-disable-next-line no-script-url
      items: [{ title: "demo-org/widgets", href: "javascript:alert(1)" }],
    };
    render(<SectionRenderer section={section} />);

    expect(screen.getByText("demo-org/widgets").closest("a")).toBeNull();
  });

  // An unlabelled scope row — the Slack/1Password steady state, and the
  // fallback when a label bridge could not be read. Plain text, never an
  // auto-linked one: a list title is a name, not a URL.
  it("list: a title with no href stays plain text", () => {
    const section: Section = {
      id: "directoryscopes",
      label: "Scopes",
      kind: "list",
      items: [{ title: "slack_channel:C0123", subtitle: "synced by Slack" }],
    };
    render(<SectionRenderer section={section} />);

    expect(screen.getByText("slack_channel:C0123").closest("a")).toBeNull();
  });
});
