import { Fragment, type ReactNode } from "react";
import { Mrkdwn, EMOJI, type MrkdwnContext } from "./mrkdwn";
import type {
  Attachment,
  Block,
  ButtonElement,
  ContextElement,
  RichTextBlock,
  RichTextElement,
  RichTextSection,
  RichTextSubBlock,
  SectionAccessory,
  TextObject,
} from "./types";

// Renders Block Kit JSON to faithful Slack DOM. The JSON shape is exactly what
// Slack accepts (and what OAP's `slack` kind emits), so a captured fixture and a
// live Slack message render identically here.

function Text({
  text,
  ctx,
}: {
  text: TextObject;
  ctx: MrkdwnContext;
}): ReactNode {
  if (text.type === "mrkdwn") return <Mrkdwn text={text.text} ctx={ctx} />;
  // plain_text: emoji shortcodes are still expanded when emoji !== false, but no
  // other markup is parsed. We reuse the mrkdwn emoji pass by treating the raw
  // string as text — plain_text never contains Slack markup in OAP's output.
  return <>{text.text}</>;
}

function Button({ el }: { el: ButtonElement }): ReactNode {
  const variant =
    el.style === "primary"
      ? "sk-btn--primary"
      : el.style === "danger"
        ? "sk-btn--danger"
        : "";
  return (
    <button
      className={`sk-btn ${variant}`.trim()}
      type="button"
      data-action-id={el.action_id}
    >
      {el.text.text}
    </button>
  );
}

function Accessory({ el }: { el: SectionAccessory }): ReactNode {
  if (el.type === "button") return <Button el={el} />;
  return (
    <img className="sk-accessory-image" src={el.image_url} alt={el.alt_text} />
  );
}

function ContextEl({
  el,
  ctx,
}: {
  el: ContextElement;
  ctx: MrkdwnContext;
}): ReactNode {
  if (el.type === "image")
    return (
      <img className="sk-context-image" src={el.image_url} alt={el.alt_text} />
    );
  return (
    <span className="sk-context-text">
      <Text text={el} ctx={ctx} />
    </span>
  );
}

// ---- rich_text -----------------------------------------------------------

function RichRun({
  el,
  ctx,
}: {
  el: RichTextElement;
  ctx: MrkdwnContext;
}): ReactNode {
  if (el.type === "emoji")
    return (
      <span className="sk-emoji">
        {el.unicode ?? EMOJI[el.name] ?? `:${el.name}:`}
      </span>
    );
  if (el.type === "user")
    return <span className="sk-mention">@{ctx.resolveUser(el.user_id)}</span>;
  if (el.type === "channel")
    return (
      <span className="sk-mention sk-mention--channel">
        #{ctx.resolveChannel(el.channel_id)}
      </span>
    );

  const content = el.type === "link" ? (el.text ?? el.url) : el.text;
  const styled = (node: ReactNode): ReactNode => {
    let out = node;
    if (el.style?.code) out = <code className="sk-code-inline">{out}</code>;
    if (el.style?.bold) out = <strong>{out}</strong>;
    if (el.style?.italic) out = <em>{out}</em>;
    if (el.style?.strike) out = <del>{out}</del>;
    return out;
  };
  if (el.type === "link")
    return (
      <a className="sk-link" href={el.url} target="_blank" rel="noreferrer">
        {styled(content)}
      </a>
    );
  return <>{styled(content)}</>;
}

function RichSection({
  section,
  ctx,
}: {
  section: RichTextSection;
  ctx: MrkdwnContext;
}): ReactNode {
  return (
    <>
      {section.elements.map((el, i) => (
        <RichRun el={el} ctx={ctx} key={i} />
      ))}
    </>
  );
}

function RichSubBlock({
  sub,
  ctx,
}: {
  sub: RichTextSubBlock;
  ctx: MrkdwnContext;
}): ReactNode {
  switch (sub.type) {
    case "rich_text_section":
      return (
        <div className="sk-rich-section">
          <RichSection section={sub} ctx={ctx} />
        </div>
      );
    case "rich_text_preformatted":
      return (
        <pre className="sk-code-block">
          <code>
            {sub.elements.map((el, i) => (
              <RichRun el={el} ctx={ctx} key={i} />
            ))}
          </code>
        </pre>
      );
    case "rich_text_quote":
      return (
        <blockquote className="sk-quote">
          {sub.elements.map((el, i) => (
            <RichRun el={el} ctx={ctx} key={i} />
          ))}
        </blockquote>
      );
    case "rich_text_list": {
      const Tag = sub.style === "ordered" ? "ol" : "ul";
      return (
        <Tag
          className={`sk-list sk-list--${sub.style}`}
          style={{ marginLeft: (sub.indent ?? 0) * 20 }}
        >
          {sub.elements.map((section, i) => (
            <li key={i}>
              <RichSection section={section} ctx={ctx} />
            </li>
          ))}
        </Tag>
      );
    }
  }
}

function RichText({
  block,
  ctx,
}: {
  block: RichTextBlock;
  ctx: MrkdwnContext;
}): ReactNode {
  return (
    <div className="sk-rich-text">
      {block.elements.map((sub, i) => (
        <RichSubBlock sub={sub} ctx={ctx} key={i} />
      ))}
    </div>
  );
}

// ---- block dispatch ------------------------------------------------------

function BlockNode({
  block,
  ctx,
}: {
  block: Block;
  ctx: MrkdwnContext;
}): ReactNode {
  switch (block.type) {
    case "header":
      return (
        <h3 className="sk-block-header">
          <Text text={block.text} ctx={ctx} />
        </h3>
      );
    case "section":
      return (
        <div className="sk-section">
          <div className="sk-section-body">
            {block.text && (
              <div className="sk-section-text">
                <Text text={block.text} ctx={ctx} />
              </div>
            )}
            {block.fields && block.fields.length > 0 && (
              <div className="sk-section-fields">
                {block.fields.map((f, i) => (
                  <div className="sk-field" key={i}>
                    <Text text={f} ctx={ctx} />
                  </div>
                ))}
              </div>
            )}
          </div>
          {block.accessory && (
            <div className="sk-section-accessory">
              <Accessory el={block.accessory} />
            </div>
          )}
        </div>
      );
    case "context":
      return (
        <div className="sk-context">
          {block.elements.map((el, i) => (
            <ContextEl el={el} ctx={ctx} key={i} />
          ))}
        </div>
      );
    case "actions":
      return (
        <div className="sk-actions">
          {block.elements.map((el, i) => (
            <Button el={el} key={i} />
          ))}
        </div>
      );
    case "divider":
      return <hr className="sk-divider" />;
    case "image":
      return (
        <div className="sk-image-block">
          {block.title && (
            <div className="sk-image-title">
              <Text text={block.title} ctx={ctx} />
            </div>
          )}
          <img src={block.image_url} alt={block.alt_text} />
        </div>
      );
    case "rich_text":
      return <RichText block={block} ctx={ctx} />;
    case "container":
      return (
        <div className="sk-container">
          {block.rich_text_title && (
            <div className="sk-container-title">
              <RichText block={block.rich_text_title} ctx={ctx} />
            </div>
          )}
          {block.title && (
            <div className="sk-container-title">
              <Text text={block.title} ctx={ctx} />
            </div>
          )}
          {block.subtitle && (
            <div className="sk-container-subtitle">
              <Text text={block.subtitle} ctx={ctx} />
            </div>
          )}
          {block.has_header_divider && <hr className="sk-divider" />}
          <Blocks blocks={block.child_blocks} ctx={ctx} />
        </div>
      );
    case "plan":
      return (
        <div className="sk-plan">
          {block.title && <div className="sk-plan-title">{block.title}</div>}
          <ul className="sk-plan-tasks">
            {block.tasks.map((t) => (
              <li className={`sk-task sk-task--${t.status}`} key={t.task_id}>
                <span className="sk-task-glyph">
                  {TASK_GLYPH[t.status] ?? "•"}
                </span>
                <span className="sk-task-title">{t.title}</span>
              </li>
            ))}
          </ul>
        </div>
      );
  }
}

const TASK_GLYPH: Record<string, string> = {
  complete: "✓",
  in_progress: "◐",
  pending: "○",
  error: "✕",
};

export function Blocks({
  blocks,
  ctx,
}: {
  blocks: Block[];
  ctx: MrkdwnContext;
}): ReactNode {
  return (
    <div className="sk-blocks">
      {blocks.map((b, i) => (
        <Fragment key={b.block_id ?? i}>
          <BlockNode block={b} ctx={ctx} />
        </Fragment>
      ))}
    </div>
  );
}

export function Attachments({
  attachments,
  ctx,
}: {
  attachments: Attachment[];
  ctx: MrkdwnContext;
}): ReactNode {
  return (
    <div className="sk-attachments">
      {attachments.map((att, i) => (
        <div
          className="sk-attachment"
          key={i}
          style={
            att.color
              ? ({ "--sk-attachment-color": att.color } as React.CSSProperties)
              : undefined
          }
        >
          {att.blocks ? (
            <Blocks blocks={att.blocks} ctx={ctx} />
          ) : (
            <div className="sk-section-text">{att.text ?? att.fallback}</div>
          )}
        </div>
      ))}
    </div>
  );
}
