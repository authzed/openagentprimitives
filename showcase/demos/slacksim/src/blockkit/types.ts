// Block Kit type definitions — a faithful subset of Slack's Block Kit JSON,
// scoped to the block/element/object types OAP's `slack` channel kind actually
// emits (see pkg/channels/channelkinds/slack). The block-capture harness dumps
// real JSON in exactly this shape; slacksim renders it verbatim so the demo can
// never drift from what OAP really sends.
//
// Reference: https://api.slack.com/reference/block-kit

/** A text object: either Slack `mrkdwn` or `plain_text`. */
export interface TextObject {
  type: "mrkdwn" | "plain_text";
  text: string;
  /** plain_text only: whether to render :emoji: shortcodes. Defaults true. */
  emoji?: boolean;
  /** mrkdwn only: disable automatic hyperlinking / markup. */
  verbatim?: boolean;
}

export interface ButtonElement {
  type: "button";
  text: TextObject; // plain_text
  action_id?: string;
  value?: string;
  url?: string;
  /** default (unset) | primary (green) | danger (red). */
  style?: "primary" | "danger";
  confirm?: unknown;
}

export interface ImageElement {
  type: "image";
  image_url: string;
  alt_text: string;
}

/** Elements allowed inside a `context` block. */
export type ContextElement = ImageElement | TextObject;

/** A `section` accessory (right-aligned element). OAP uses buttons + images. */
export type SectionAccessory = ButtonElement | ImageElement;

export interface SectionBlock {
  type: "section";
  block_id?: string;
  text?: TextObject;
  /** Up to 10 text objects rendered as a two-column field grid. */
  fields?: TextObject[];
  accessory?: SectionAccessory;
}

export interface HeaderBlock {
  type: "header";
  block_id?: string;
  text: TextObject; // plain_text
}

export interface ContextBlock {
  type: "context";
  block_id?: string;
  elements: ContextElement[];
}

export interface ActionsBlock {
  type: "actions";
  block_id?: string;
  elements: ButtonElement[];
}

export interface DividerBlock {
  type: "divider";
  block_id?: string;
}

export interface ImageBlock {
  type: "image";
  block_id?: string;
  image_url: string;
  alt_text: string;
  title?: TextObject;
}

// ---- rich_text -----------------------------------------------------------
// rich_text is how Slack represents structured, non-mrkdwn message bodies
// (styled runs, lists, quotes, preformatted code). OAP emits these for tool
// output and code.

export interface RichTextStyle {
  bold?: boolean;
  italic?: boolean;
  strike?: boolean;
  code?: boolean;
}

export interface RichTextText {
  type: "text";
  text: string;
  style?: RichTextStyle;
}

export interface RichTextLink {
  type: "link";
  url: string;
  text?: string;
  style?: RichTextStyle;
}

export interface RichTextUser {
  type: "user";
  user_id: string;
  style?: RichTextStyle;
}

export interface RichTextChannel {
  type: "channel";
  channel_id: string;
  style?: RichTextStyle;
}

export interface RichTextEmoji {
  type: "emoji";
  name: string;
  unicode?: string;
}

export type RichTextElement =
  | RichTextText
  | RichTextLink
  | RichTextUser
  | RichTextChannel
  | RichTextEmoji;

export interface RichTextSection {
  type: "rich_text_section";
  elements: RichTextElement[];
}

export interface RichTextList {
  type: "rich_text_list";
  style: "bullet" | "ordered";
  indent?: number;
  border?: number;
  elements: RichTextSection[];
}

export interface RichTextQuote {
  type: "rich_text_quote";
  elements: RichTextElement[];
}

export interface RichTextPreformatted {
  type: "rich_text_preformatted";
  border?: number;
  elements: RichTextElement[];
}

export type RichTextSubBlock =
  | RichTextSection
  | RichTextList
  | RichTextQuote
  | RichTextPreformatted;

export interface RichTextBlock {
  type: "rich_text";
  block_id?: string;
  elements: RichTextSubBlock[];
}

// OAP's slack kind emits two custom block types that standard Block Kit lacks.
// They were discovered by the block-capture harness (OAP's real sender output),
// so the renderer must handle them to show what OAP actually posts.

/** The hand-rolled Slack "container" block (pkg/.../slack/block_container.go). */
export interface ContainerBlock {
  type: "container";
  block_id?: string;
  /** Bold title as a rich_text run (tone emoji + text). */
  rich_text_title?: RichTextBlock;
  title?: TextObject;
  subtitle?: TextObject;
  child_blocks: Block[];
  has_header_divider?: boolean;
}

export interface TaskCard {
  type: "task_card";
  task_id: string;
  title: string;
  status: "pending" | "in_progress" | "complete" | "error";
  details?: string;
}

/** OAP's plan block: a titled checklist of task_cards. */
export interface PlanBlock {
  type: "plan";
  block_id?: string;
  title?: string;
  tasks: TaskCard[];
}

export type Block =
  | SectionBlock
  | HeaderBlock
  | ContextBlock
  | ActionsBlock
  | DividerBlock
  | ImageBlock
  | RichTextBlock
  | ContainerBlock
  | PlanBlock;

/**
 * A legacy secondary attachment. OAP uses these for the colored severity bar
 * (attachments.go) — `color` is a hex string; `blocks` render inside the bar.
 */
export interface Attachment {
  color?: string;
  blocks?: Block[];
  /** Fallback plain text if `blocks` is absent. */
  text?: string;
  fallback?: string;
}
