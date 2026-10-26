// Plain Markdown renditions of the docs, for agents and LLM tooling: the
// per-page /docs/<slug>.md, /llms.txt, and /llms-full.txt. Pure, so it is
// tested against every real guide without a bundler.
//
// The source is the guide's MDX, not its rendered HTML. MDX is already
// Markdown apart from a small, closed set of JSX (the components in
// mdx-components.tsx plus a few inline HTML tags), and each has a direct
// Markdown equivalent. lib/markdown.test.ts fails if a guide uses a tag this
// file does not translate, so a new component cannot silently leak JSX into
// the Markdown.
import { COVERAGE_LABELS } from "@/components/Coverage";
import type { Guide, NavSection } from "./nav";
import type { MediaAsset } from "./manifest";

export interface MarkdownContext {
  siteUrl: string;
  asset: (name: string) => MediaAsset | undefined;
}

const CALLOUT_LABEL: Record<string, string> = {
  note: "Note",
  tip: "Tip",
  warning: "Warning",
  security: "Security",
};

const META_RE = /^export const meta = \{[\s\S]*?^\}\s*/m;

function attr(attrs: string, name: string): string | undefined {
  return new RegExp(`\\b${name}="([^"]*)"`).exec(attrs)?.[1];
}

function dedent(block: string): string {
  return block
    .split("\n")
    .map((l) => l.trim())
    .join("\n")
    .trim();
}

// Inline code spans are literal in MDX (`oap audit verify <session>`), so they
// are swapped out before any tag rewriting and restored after.
function withCodeSpansProtected(
  text: string,
  fn: (t: string) => string,
): string {
  const spans: string[] = [];
  const masked = text.replace(/`[^`]+`/g, (m) => {
    spans.push(m);
    return `\uE000${spans.length - 1}\uE001`;
  });
  return fn(masked).replace(/\uE000(\d+)\uE001/g, (_, i) => spans[Number(i)]);
}

function absolute(href: string, siteUrl: string): string {
  return href.startsWith("/") ? siteUrl + href : href;
}

function media(name: string, ctx: MarkdownContext, caption?: string): string {
  const a = ctx.asset(name);
  if (!a) return "";
  const cap = caption ?? a.caption ?? name;
  if (a.kind === "screenshot")
    return a.src ? `![${cap}](${absolute(a.src, ctx.siteUrl)})` : "";
  const file = a.mp4 ?? a.webm;
  return file ? `[Video: ${cap}](${absolute(file, ctx.siteUrl)})` : "";
}

function series(name: string, ctx: MarkdownContext): string {
  const out: string[] = [];
  for (let i = 1; ctx.asset(`${name}-${i}`); i++)
    out.push(media(`${name}-${i}`, ctx));
  return out.join("\n\n");
}

const INLINE_MARK: Record<string, string> = {
  strong: "**",
  em: "*",
  code: "`",
};

// HTML collapses the space in "<strong> plan</strong>"; Markdown does not, and
// "** plan**" is not emphasis. Edge whitespace moves outside the markers.
function inlineMark(_: string, tag: string, body: string): string {
  const [, lead, inner, trail] = /^(\s*)([\s\S]*?)(\s*)$/.exec(body)!;
  const m = INLINE_MARK[tag];
  return `${lead}${m}${inner}${m}${trail}`;
}

function prose(text: string, ctx: MarkdownContext): string {
  return withCodeSpansProtected(text, (t) =>
    t
      .replace(/<div className="doc-lede">([\s\S]*?)<\/div>/g, (_, body) =>
        dedent(body),
      )
      .replace(
        /<Callout\b([^>]*)>([\s\S]*?)<\/Callout>/g,
        (_, attrs: string, body: string) => {
          const kind = attr(attrs, "kind") ?? "note";
          const title = attr(attrs, "title");
          const head = `**${CALLOUT_LABEL[kind] ?? CALLOUT_LABEL.note}${title ? `: ${title}` : ""}**`;
          const lines = [head, "", ...dedent(prose(body, ctx)).split("\n")];
          return lines.map((l) => (l ? `> ${l}` : ">")).join("\n");
        },
      )
      .replace(/<(Clip|Screenshot)\b([^>]*?)\/>/g, (_, _c, attrs: string) =>
        media(attr(attrs, "name") ?? "", ctx, attr(attrs, "caption")),
      )
      .replace(/<ScreenshotSeries\b([^>]*?)\/>/g, (_, attrs: string) =>
        series(attr(attrs, "name") ?? "", ctx),
      )
      .replace(/<InstallInDesktop\b([^>]*?)\/>/g, (_, attrs: string) => {
        const ref = attr(attrs, "ref") ?? "";
        return `[Install in OAP Desktop](oap://install/agent?v=1&ref=${encodeURIComponent(ref)}) or \`oap agent install ${ref}\``;
      })
      .replace(
        /<Coverage level="([^"]*)" \/>/g,
        (_, level: string) => `(coverage: ${COVERAGE_LABELS[level] ?? level})`,
      )
      .replace(
        /<h([1-6])\b[^>]*>([\s\S]*?)<\/h\1>/g,
        (_, n: string, body: string) =>
          `${"#".repeat(Number(n))} ${body.trim()}`,
      )
      .replace(/<ul>\s*/g, "")
      .replace(/\s*<\/ul>/g, "")
      .replace(
        /<li>([\s\S]*?)<\/li>\s*/g,
        (_, body: string) => `- ${body.replace(/\s+/g, " ").trim()}\n`,
      )
      .replace(
        /<a href="([^"]*)">([\s\S]*?)<\/a>/g,
        (_, href: string, body: string) => `[${body}](${href})`,
      )
      .replace(/<(strong|em|code)>([\s\S]*?)<\/\1>/g, inlineMark)
      .replace(/<\/?small>/g, "")
      .replace(
        /\]\((\/[^)\s]*)\)/g,
        (_, href: string) => `](${absolute(href, ctx.siteUrl)})`,
      ),
  );
}

// Renders one guide's MDX as Markdown: the meta block becomes a short header
// (description and canonical URL), and everything outside code fences is
// rewritten through prose(). Fenced code is passed through untouched.
export function guideToMarkdown(
  guide: Guide,
  src: string,
  ctx: MarkdownContext,
): string {
  const body = src.replace(META_RE, "");
  const parts = body.split(/^(```[\s\S]*?^```)$/m);
  const md = parts
    .map((part, i) =>
      i % 2 === 1 ? part : prose(part, ctx).replace(/\n{3,}/g, "\n\n"),
    )
    .join("")
    .trim();
  const url = `${ctx.siteUrl}/docs/${guide.slug}`;
  const head = [guide.description && `> ${guide.description}`, `Source: ${url}`]
    .filter(Boolean)
    .join("\n\n");
  // The body opens with the page's own `# Title`; the header goes under it.
  // A body that is only the title line has nothing after it.
  if (md.startsWith("# ")) {
    const br = md.indexOf("\n");
    const [title, rest] = br < 0 ? [md, ""] : [md.slice(0, br), md.slice(br)];
    return `${title}\n\n${head}${rest}\n`;
  }
  return `# ${guide.title}\n\n${head}\n\n${md}\n`;
}

// /llms.txt, per https://llmstxt.org: an H1, a blockquote summary, then one
// link list per docs section, each link to the page's Markdown rendition.
export function llmsTxt(
  nav: NavSection[],
  site: { name: string; description: string; siteUrl: string },
): string {
  const out = [
    `# ${site.name}`,
    "",
    `> ${site.description}`,
    "",
    `Every page below is the Markdown rendition of a docs page; drop the .md for the HTML. The whole docs set as one file: ${site.siteUrl}/llms-full.txt`,
  ];
  for (const { section, blocks } of nav) {
    out.push("", `## ${section}`, "");
    for (const block of blocks)
      for (const g of block.guides) {
        const label = block.group ? `${block.group}: ${g.title}` : g.title;
        const desc = g.description ? `: ${g.description}` : "";
        out.push(`- [${label}](${site.siteUrl}/docs/${g.slug}.md)${desc}`);
      }
  }
  return out.join("\n") + "\n";
}
