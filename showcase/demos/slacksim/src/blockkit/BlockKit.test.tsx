import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { Blocks, Attachments } from "./BlockKit";
import type { Block, Attachment } from "./types";
import type { MrkdwnContext } from "./mrkdwn";

const ctx: MrkdwnContext = {
  resolveUser: (id) => ({ U2: "reviewbot" })[id] ?? id,
  resolveChannel: (id) => id,
};

describe("BlockKit block rendering", () => {
  it("renders a header block as prominent text", () => {
    const blocks: Block[] = [
      { type: "header", text: { type: "plain_text", text: "PR #7 review" } },
    ];
    render(<Blocks blocks={blocks} ctx={ctx} />);
    expect(screen.getByText("PR #7 review")).toBeInTheDocument();
  });

  it("renders a section with mrkdwn text", () => {
    const blocks: Block[] = [
      {
        type: "section",
        text: { type: "mrkdwn", text: "Recommendation: *do not merge*." },
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    expect(container.querySelector("strong")?.textContent).toBe("do not merge");
  });

  it("renders section fields as a two-column grid", () => {
    const blocks: Block[] = [
      {
        type: "section",
        fields: [
          { type: "mrkdwn", text: "*Base*\n2321b6d" },
          { type: "mrkdwn", text: "*Head*\n34fbc53" },
        ],
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    const grid = container.querySelector(".sk-section-fields");
    expect(grid).not.toBeNull();
    expect(grid!.children.length).toBe(2);
  });

  it("renders an actions block with buttons carrying style classes", () => {
    const blocks: Block[] = [
      {
        type: "actions",
        elements: [
          {
            type: "button",
            text: { type: "plain_text", text: "Approve" },
            style: "primary",
          },
          {
            type: "button",
            text: { type: "plain_text", text: "Deny" },
            style: "danger",
          },
          {
            type: "button",
            text: { type: "plain_text", text: "Show settings" },
          },
        ],
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    expect(container.querySelector(".sk-btn--primary")?.textContent).toBe(
      "Approve",
    );
    expect(container.querySelector(".sk-btn--danger")?.textContent).toBe(
      "Deny",
    );
    // Default (unstyled) button still renders as a button.
    expect(
      screen.getByRole("button", { name: "Show settings" }),
    ).toBeInTheDocument();
  });

  it("renders a divider as an <hr>", () => {
    const { container } = render(
      <Blocks blocks={[{ type: "divider" }]} ctx={ctx} />,
    );
    expect(container.querySelector("hr")).not.toBeNull();
  });

  it("renders a context block with muted small text", () => {
    const blocks: Block[] = [
      {
        type: "context",
        elements: [{ type: "mrkdwn", text: "done · $0.2906 · 40s" }],
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    const ctxEl = container.querySelector(".sk-context");
    expect(ctxEl?.textContent).toContain("$0.2906");
  });

  it("renders a rich_text preformatted sub-block as a <pre>", () => {
    const blocks: Block[] = [
      {
        type: "rich_text",
        elements: [
          {
            type: "rich_text_preformatted",
            elements: [{ type: "text", text: "go test ./..." }],
          },
        ],
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    expect(container.querySelector("pre")?.textContent).toContain(
      "go test ./...",
    );
  });

  it("renders a rich_text bullet list", () => {
    const blocks: Block[] = [
      {
        type: "rich_text",
        elements: [
          {
            type: "rich_text_list",
            style: "bullet",
            elements: [
              {
                type: "rich_text_section",
                elements: [{ type: "text", text: "first" }],
              },
              {
                type: "rich_text_section",
                elements: [{ type: "text", text: "second" }],
              },
            ],
          },
        ],
      },
    ];
    const { container } = render(<Blocks blocks={blocks} ctx={ctx} />);
    expect(container.querySelectorAll("ul li").length).toBe(2);
  });
});

describe("BlockKit attachments", () => {
  it("renders the colored left bar from attachment.color", () => {
    const attachments: Attachment[] = [
      {
        color: "#e01e5a",
        blocks: [
          { type: "section", text: { type: "mrkdwn", text: "High severity" } },
        ],
      },
    ];
    const { container } = render(
      <Attachments attachments={attachments} ctx={ctx} />,
    );
    const bar = container.querySelector(".sk-attachment") as HTMLElement | null;
    expect(bar).not.toBeNull();
    // The color drives a CSS custom property consumed by the left border.
    expect(bar!.style.getPropertyValue("--sk-attachment-color")).toBe(
      "#e01e5a",
    );
  });

  it("falls back to plain text when an attachment has no blocks", () => {
    const attachments: Attachment[] = [{ color: "#2eb67d", text: "passing" }];
    render(<Attachments attachments={attachments} ctx={ctx} />);
    expect(screen.getByText("passing")).toBeInTheDocument();
  });
});
