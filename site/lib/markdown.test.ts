import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { getAsset, type MediaAsset } from "./manifest";
import { guideToMarkdown, llmsTxt, type MarkdownContext } from "./markdown";
import { buildNav, normalizeGuide, sortGuides } from "./nav";

const SITE = "https://example.test";
const ASSETS: Record<string, MediaAsset> = {
  shot: { kind: "screenshot", src: "/media/shot.png", caption: "A shot" },
  clip: {
    kind: "clip",
    mp4: "/media/clip.mp4",
    webm: "/media/clip.webm",
    caption: "A clip",
  },
  "set-1": { kind: "screenshot", src: "/media/set-1.png", caption: "One" },
  "set-2": { kind: "screenshot", src: "/media/set-2.png", caption: "Two" },
};
const ctx: MarkdownContext = { siteUrl: SITE, asset: (n) => ASSETS[n] };
const guide = normalizeGuide("demo", {
  title: "Demo",
  description: "A demo page.",
});

const render = (body: string) =>
  guideToMarkdown(
    guide,
    `export const meta = {\n  title: 'Demo',\n}\n\n# Demo\n\n${body}`,
    ctx,
  );

describe("guideToMarkdown", () => {
  it("replaces the meta block with the description and canonical URL under the title", () => {
    const md = render("Body.");
    expect(md).not.toContain("export const meta");
    expect(md).toBe(
      `# Demo\n\n> A demo page.\n\nSource: ${SITE}/docs/demo\n\nBody.\n`,
    );
  });

  it("a body that is only the title line: title once, header under it", () => {
    const md = guideToMarkdown(
      guide,
      "export const meta = {\n  title: 'Demo',\n}\n\n# Demo",
      ctx,
    );
    expect(md).toBe(`# Demo\n\n> A demo page.\n\nSource: ${SITE}/docs/demo\n`);
  });

  it.each([
    [
      "lede div: unwrapped and dedented",
      '<div className="doc-lede">\n  First line\n  second line.\n</div>',
      "First line\nsecond line.",
    ],
    [
      "callout: a titled blockquote",
      '<Callout kind="security" title="Heads up">\n  Body **text**.\n</Callout>',
      "> **Security: Heads up**\n>\n> Body **text**.",
    ],
    [
      "screenshot: image with the manifest caption and an absolute URL",
      '<Screenshot name="shot" />',
      `![A shot](${SITE}/media/shot.png)`,
    ],
    [
      "clip: a link to the mp4",
      '<Clip name="clip" caption="Override" />',
      `[Video: Override](${SITE}/media/clip.mp4)`,
    ],
    [
      "screenshot series: every numbered still",
      '<ScreenshotSeries name="set" />',
      `![One](${SITE}/media/set-1.png)\n\n![Two](${SITE}/media/set-2.png)`,
    ],
    [
      "heading tag with coverage badge: a Markdown heading",
      '<h3 id="asi01">ASI01 — Hijack <Coverage level="partial" /></h3>',
      "### ASI01 — Hijack (coverage: Partial)",
    ],
    [
      "html list with links: Markdown list with absolute links",
      '<ul>\n  <li><a href="/docs/x#y">X</a>: one\n    two.</li>\n  <li><em>Three</em></li>\n</ul>',
      `- [X](${SITE}/docs/x#y): one two.\n- *Three*`,
    ],
    [
      "markdown docs link: made absolute",
      "See [this](/docs/other).",
      `See [this](${SITE}/docs/other).`,
    ],
    [
      "inline tags with edge spaces: space moved outside the markers",
      "a\n<strong> plan</strong> on <code> acme/w </code>.",
      "a\n **plan** on  `acme/w` .",
    ],
    [
      "inline code: tags inside it are left literal",
      "Run `oap audit verify <session>` and <strong>check</strong>.",
      "Run `oap audit verify <session>` and **check**.",
    ],
  ])("%s", (_, input, want) => {
    expect(render(input)).toContain(want);
  });

  it("leaves fenced code untouched", () => {
    const fence = '```yaml\nkind: <Thing>\nhref: "/docs/x"\n```';
    expect(render(fence)).toContain(fence);
  });

  it("drops a media reference missing from the manifest instead of emitting JSX", () => {
    expect(render('<Screenshot name="nope" />')).not.toContain("<");
  });
});

// Every real guide must come out as Markdown with no JSX left over: a new
// component or tag the transform does not know fails here, not in an agent's
// context window.
describe("every guide in content/docs", () => {
  const dir = path.join(__dirname, "../content/docs");
  const files = readdirSync(dir).filter((f) => f.endsWith(".mdx"));
  const realCtx: MarkdownContext = { siteUrl: SITE, asset: getAsset };

  it.each(files)("%s renders without leftover JSX", (file) => {
    const slug = file.slice(0, -4);
    const md = guideToMarkdown(
      normalizeGuide(slug, { title: slug }),
      readFileSync(path.join(dir, file), "utf8"),
      realCtx,
    );
    const outsideCode = md
      .replace(/^```[\s\S]*?^```/gm, "")
      .replace(/`[^`]+`/g, "");
    expect(outsideCode).not.toMatch(/<\/?[A-Za-z][\w-]*[\s/>]/);
    expect(outsideCode).not.toContain("className=");
    expect(outsideCode).not.toMatch(/\]\(\/[^/]/);
  });
});

describe("llmsTxt", () => {
  it("lists every guide under its section, linking the Markdown rendition", () => {
    const nav = buildNav(
      sortGuides([
        normalizeGuide("intro", {
          title: "Intro",
          section: "Guides",
          group: "Get started",
          order: 1,
          description: "Start here.",
        }),
        normalizeGuide("cli", { title: "CLI", section: "Reference", order: 2 }),
      ]),
    );
    const txt = llmsTxt(nav, {
      name: "Site",
      description: "What it is.",
      siteUrl: SITE,
    });
    expect(txt).toMatch(/^# Site\n\n> What it is\.\n/);
    expect(txt).toContain(
      `## Guides\n\n- [Get started: Intro](${SITE}/docs/intro.md): Start here.`,
    );
    expect(txt).toContain(`## Reference\n\n- [CLI](${SITE}/docs/cli.md)\n`);
  });
});
