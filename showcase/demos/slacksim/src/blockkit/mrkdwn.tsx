import { Fragment, type ReactNode } from "react";

// A faithful-enough renderer for Slack `mrkdwn`. Slack mrkdwn is NOT Markdown:
// styles are single-char delimiters (`*bold*`, `_italic_`, `~strike~`), links
// and mentions are angle-bracket entities (`<url|label>`, `<@U123>`), and the
// underscore-italic rule respects word boundaries — so an identifier like
// `company_fit_score` must stay literal, not become italic. That boundary rule
// is the main reason this is hand-written rather than delegated to a Markdown
// library.
//
// Reference: https://api.slack.com/reference/surfaces/formatting

export interface MrkdwnContext {
  /** Resolve a user id (the `U123` in `<@U123>`) to a display name. */
  resolveUser: (id: string) => string;
  /** Resolve a channel id (the `C123` in `<#C123>`) to a channel name. */
  resolveChannel: (id: string) => string;
}

const WORD = /[A-Za-z0-9]/;

// A deliberately small shortcode set — the emoji that actually appear in OAP's
// Slack output (status ticks, severity marks, interaction tone circles) plus a
// few common reactions. Exported so the rich_text renderer resolves emoji
// elements (which carry a name, not a glyph) the same way.
export const EMOJI: Record<string, string> = {
  white_check_mark: "✅",
  heavy_check_mark: "✔️",
  x: "❌",
  no_entry: "⛔",
  no_entry_sign: "🚫",
  octagonal_sign: "🛑",
  red_circle: "🔴",
  large_blue_circle: "🔵",
  large_orange_circle: "🟠",
  large_yellow_circle: "🟡",
  large_green_circle: "🟢",
  large_purple_circle: "🟣",
  warning: "⚠️",
  rocket: "🚀",
  tada: "🎉",
  fire: "🔥",
  eyes: "👀",
  "+1": "👍",
  "-1": "👎",
  wave: "👋",
  robot_face: "🤖",
  bust_in_silhouette: "👤",
  busts_in_silhouette: "👥",
  page_facing_up: "📄",
  memo: "📝",
  key: "🔑",
  gear: "⚙️",
  mag: "🔍",
  lock: "🔒",
  unlock: "🔓",
  bell: "🔔",
  white_circle: "⚪",
  hourglass_flowing_sand: "⏳",
};

function decodeEntities(s: string): string {
  return s.replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
}

function renderEntity(
  inner: string,
  ctx: MrkdwnContext,
  key: number,
): ReactNode {
  // User mention: <@U123> or <@U123|label>
  if (inner.startsWith("@")) {
    const [id, label] = inner.slice(1).split("|");
    const name = label ?? ctx.resolveUser(id);
    return (
      <span className="sk-mention" key={key}>
        @{name}
      </span>
    );
  }
  // Channel mention: <#C123> or <#C123|label>
  if (inner.startsWith("#")) {
    const [id, label] = inner.slice(1).split("|");
    const name = label ?? ctx.resolveChannel(id);
    return (
      <span className="sk-mention sk-mention--channel" key={key}>
        #{name}
      </span>
    );
  }
  // Broadcast / special: <!here>, <!channel>, <!everyone>, <!subteam^S1|@team>
  if (inner.startsWith("!")) {
    const [kind, label] = inner.slice(1).split("|");
    const name = label
      ? label.replace(/^@/, "")
      : kind.startsWith("subteam^")
        ? "team"
        : kind;
    return (
      <span className="sk-mention sk-mention--broadcast" key={key}>
        @{name}
      </span>
    );
  }
  // Link: <url> or <url|label>
  const [url, label] = inner.split("|");
  return (
    <a
      className="sk-link"
      href={url}
      key={key}
      target="_blank"
      rel="noreferrer"
    >
      {label ?? url}
    </a>
  );
}

interface StyleMatch {
  inner: string;
  end: number;
}

// Attempt to match a style span (`*`, `_`, `~`) opening at index `i`, honoring
// Slack's word-boundary rule so identifiers with underscores stay literal.
function tryStyle(
  text: string,
  i: number,
  prevChar: string | undefined,
): StyleMatch | null {
  const d = text[i];
  if (d !== "*" && d !== "_" && d !== "~") return null;
  // Opening boundary: the delimiter must not sit directly after a word char.
  // This is what keeps `company_fit_score` from italicizing on the first `_`.
  if (prevChar && WORD.test(prevChar)) return null;
  // Content must start with a non-space char.
  if (i + 1 >= text.length || /\s/.test(text[i + 1])) return null;
  for (let j = i + 1; j < text.length; j++) {
    const c = text[j];
    if (c === "\n") return null; // a style span does not cross a line break
    if (c === d && !/\s/.test(text[j - 1])) {
      const next = text[j + 1];
      // Closing boundary: next char must not be a word char.
      if (!next || !WORD.test(next))
        return { inner: text.slice(i + 1, j), end: j + 1 };
    }
  }
  return null;
}

function parseInline(
  text: string,
  ctx: MrkdwnContext,
  keyBase: string,
): ReactNode[] {
  const out: ReactNode[] = [];
  let buf = "";
  let key = 0;
  const nextKey = () => `${keyBase}-${key++}`;
  const flush = () => {
    if (buf) {
      out.push(<Fragment key={nextKey()}>{decodeEntities(buf)}</Fragment>);
      buf = "";
    }
  };

  let i = 0;
  while (i < text.length) {
    const ch = text[i];

    if (ch === "\n") {
      flush();
      out.push(<br key={nextKey()} />);
      i++;
      continue;
    }

    // Inline code: `...` (single backtick). Content is not further parsed.
    if (ch === "`") {
      const end = text.indexOf("`", i + 1);
      if (end > i) {
        flush();
        out.push(
          <code className="sk-code-inline" key={nextKey()}>
            {decodeEntities(text.slice(i + 1, end))}
          </code>,
        );
        i = end + 1;
        continue;
      }
    }

    // Entity: <...>
    if (ch === "<") {
      const end = text.indexOf(">", i + 1);
      if (end > i) {
        flush();
        out.push(renderEntity(text.slice(i + 1, end), ctx, key++));
        i = end + 1;
        continue;
      }
    }

    // Emoji shortcode: :name:
    if (ch === ":") {
      const m = /^:([a-z0-9_+'-]+):/i.exec(text.slice(i));
      if (m) {
        const glyph = EMOJI[m[1].toLowerCase()];
        if (glyph) {
          flush();
          out.push(
            <span
              className="sk-emoji"
              key={nextKey()}
              role="img"
              aria-label={m[1]}
            >
              {glyph}
            </span>,
          );
          i += m[0].length;
          continue;
        }
        // Unknown shortcode: emit literally and consume, so `8:30` etc. are safe
        // (they never match the :name: pattern and fall through to the buffer).
        buf += m[0];
        i += m[0].length;
        continue;
      }
    }

    // Styles: *bold* _italic_ ~strike~
    const style = tryStyle(text, i, i > 0 ? text[i - 1] : undefined);
    if (style) {
      flush();
      const Tag = ch === "*" ? "strong" : ch === "_" ? "em" : "del";
      out.push(
        <Tag key={nextKey()}>{parseInline(style.inner, ctx, nextKey())}</Tag>,
      );
      i = style.end;
      continue;
    }

    buf += ch;
    i++;
  }
  flush();
  return out;
}

function parseMrkdwn(text: string, ctx: MrkdwnContext): ReactNode[] {
  const nodes: ReactNode[] = [];
  const fenceRe = /```(?:[^\n`]*)?\n?([\s\S]*?)```/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let idx = 0;
  while ((m = fenceRe.exec(text)) !== null) {
    if (m.index > last) {
      nodes.push(...parseInline(text.slice(last, m.index), ctx, `p${idx++}`));
    }
    nodes.push(
      <pre className="sk-code-block" key={`pre${idx++}`}>
        <code>{decodeEntities(m[1].replace(/\n$/, ""))}</code>
      </pre>,
    );
    last = fenceRe.lastIndex;
  }
  if (last < text.length) {
    nodes.push(...parseInline(text.slice(last), ctx, `p${idx++}`));
  }
  return nodes;
}

export function Mrkdwn({
  text,
  ctx,
}: {
  text: string;
  ctx: MrkdwnContext;
}): ReactNode {
  return <>{parseMrkdwn(text, ctx)}</>;
}
