import type { Metadata } from "next";
import type { CSSProperties, ReactNode } from "react";
import { JsonLd } from "@/components/JsonLd";
import { getAsset } from "@/lib/manifest";
import { OG_IMAGE, SITE_NAME } from "@/lib/site";
import { storyGraph, type FaqEntry } from "@/lib/structured-data";
import lightInk from "../../../../docs/assets/brand/oap-wordmark-light.svg";
import darkInk from "../../../../docs/assets/brand/oap-wordmark-dark.svg";
import {
  BeforeAfter,
  CopyButton,
  DemoClip,
  ThemeCycleButton,
} from "../StoryClient";
import "../stories.css";

// "Build your own" story 01. The layout is the series template from the story
// design file: hero, at-a-glance, the original, ingredients, the build steps,
// see it work, why OAP, still ahead, FAQ, try it, and the series strip. The
// claims follow examples/messagingbot; keep them in step with the bundle.

const PATH = "/stories/claude-design";
const TITLE = "We built our own Claude Design";
const DESCRIPTION =
  "How we rebuilt the Claude Design workflow on Open Agent Primitives: a brief in Slack, an on-brand page, notes pinned to the design, and versioned revisions.";
const KEYWORDS = [
  "Claude Design alternative",
  "self-hosted Claude Design",
  "open source AI design agent",
  "AI brand designer",
  "AI design annotations",
  "OAP artifacts",
  "agent skills",
];
const PUBLISHED = "2026-10-22";
const UPDATED = "2026-10-26";

const REPO = "https://github.com/authzed/openagentprimitives";
const BUNDLE = `${REPO}/tree/main/examples/messagingbot`;
const INSTALL = "oap agent install examples/messagingbot";
const INSTALL_ALL = [
  "oap agent lint examples/messagingbot",
  "oap agent install examples/messagingbot --namespace default",
  "oap channel create --kind slack",
].join("\n");

export const metadata: Metadata = {
  title: { absolute: `${TITLE} · OAP stories` },
  description: DESCRIPTION,
  keywords: KEYWORDS,
  alternates: { canonical: PATH },
  openGraph: {
    title: TITLE,
    description: DESCRIPTION,
    url: PATH,
    siteName: SITE_NAME,
    type: "article",
    publishedTime: PUBLISHED,
    modifiedTime: UPDATED,
    images: [OG_IMAGE],
  },
  twitter: { card: "summary_large_image", images: [OG_IMAGE.url] },
};

// Answers are plain text so the reader and the FAQPage data say the same
// words; a `backticked` span renders as code.
const FAQ: (FaqEntry & { id: string })[] = [
  {
    id: "faq-alternative",
    q: "Can I build a Claude Design alternative on OAP?",
    a: "Yes. messagingbot reproduces the core Claude Design workflow on OAP: a brief in chat, a design grounded in your brand system, click-to-comment feedback on the rendered page, and versioned revisions. It is built from an AgentClass, a read-only GitHub toolspec, two agent skills, and the artifact system, with no new platform code.",
  },
  {
    id: "faq-code",
    q: "Do I have to write code to run it?",
    a: "No. messagingbot ships as an example agent bundle. Install it with `oap agent install examples/messagingbot`, answer three questions (a read-only GitHub token, your brand repositories, and which skills to load), and bind it to a Slack channel or use the browser chat.",
  },
  {
    id: "faq-brand",
    q: "Where does messagingbot get our brand from?",
    a: "From the GitHub repositories you name at install, such as one holding colour and type tokens and one holding approved messaging. It reads them read-only through the gh CLI, and the toolspec refuses any repository that is not on the approved list.",
  },
  {
    id: "faq-comments",
    q: "How do inline comments on the design work?",
    a: "Open the artifact in the live viewer, click an element, and type a note. Pressing Send bundles your notes up and sends them to the agent with the details of where each was made. OAP first checks that you may view that artifact; the agent then revises the same artifact and records a change note for each comment.",
  },
  {
    id: "faq-safe",
    q: "Is it safe to open HTML a model wrote?",
    a: "Yes. OAP renders every artifact inert: you see the page, but nothing in it can run. messagingbot is also instructed to produce HTML and CSS only.",
  },
  {
    id: "faq-model",
    q: "Which model does it use?",
    a: "Whichever default model your OAP cluster is configured with. The bundle pins no model, so it works with any provider OAP supports, including Anthropic, OpenAI, and OpenRouter.",
  },
  {
    id: "faq-skill",
    q: "Can I add our own brand-voice skill?",
    a: "Yes. Add a SkillSource for the repository that holds the skill, add an entry to the AgentClass spec.skills list, and add its name to the skills install question. The installer can then select it like the bundled skills.",
  },
];

function withCode(text: string): ReactNode {
  return text
    .split(/(`[^`]+`)/)
    .map((part, i) =>
      part.startsWith("`") ? <code key={i}>{part.slice(1, -1)}</code> : part,
    );
}

function OapA({
  className = "icon-a",
  style,
}: {
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <svg className={className} style={style}>
      <use href="#oap-a" />
    </svg>
  );
}

const INGREDIENTS: { has: string; need: string; oap: ReactNode }[] = [
  {
    has: "A conversation",
    need: "Somewhere people already ask for work",
    oap: (
      <>
        A <a href="/docs/channels">channel</a>: a{" "}
        <a href="/docs/slack">Slack</a> thread or the{" "}
        <a href="/docs/browser-chat">browser chat</a>
      </>
    ),
  },
  {
    has: "A canvas",
    need: "A durable, versioned home for rendered output",
    oap: (
      <>
        <a href="/docs/artifacts">Artifacts</a>, with{" "}
        <a href="/docs/artifact-revisions">revisions</a>
      </>
    ),
  },
  {
    has: "Click-to-comment",
    need: "A way to point at an element and route the note back to the agent",
    oap: (
      <>
        <a href="/docs/artifact-annotations">Artifact annotations</a>, the
        viewer&apos;s <code>annotation_batch</code> interaction
      </>
    ),
  },
  {
    has: "Your design system",
    need: "Read access to the brand's source of truth, and nothing more",
    oap: (
      <>
        A read-only <code>gh</code>{" "}
        <a href="/docs/toolspec-validation">toolspec</a> bounded to approved
        repositories
      </>
    ),
  },
  {
    has: "Design taste",
    need: "Instructions on how to design well",
    oap: (
      <>
        <a href="/docs/skills">Agent skills</a>: <code>frontend-design</code>{" "}
        and <code>theme-factory</code>
      </>
    ),
  },
  {
    has: "A safe preview",
    need: "A way to show model-written HTML without trusting it",
    oap: (
      <>
        <a href="/docs/artifact-safety">Inert rendering</a>, so model-written
        HTML can&apos;t run scripts
      </>
    ),
  },
  {
    has: "A history",
    need: "A record of who asked for what and what the agent did",
    oap: (
      <>
        The signed, append-only <a href="/docs/audit-log">audit log</a>
      </>
    ),
  },
];

export default function Page() {
  const clip = getAsset("messagingbot-design");
  return (
    <div className="st">
      <svg
        width="0"
        height="0"
        style={{ position: "absolute" }}
        aria-hidden="true"
      >
        <defs>
          {/* The OAP A, with its cut-out (OAP_TRI.authorized from the landing). */}
          <symbol id="oap-a" viewBox="6 7 36 32">
            <path
              fill="currentColor"
              fillRule="evenodd"
              d="M22.71 8.75C23.28 7.75 24.72 7.75 25.29 8.75L40.78 35.77C41.350 36.76 40.64 38 39.49 38H8.51C7.36 38 6.65 36.76 7.22 35.77L22.71 8.75ZM24.65 21.13C24.36 20.63 23.640 20.63 23.35 21.13L18.57 29.47C18.29 29.97 18.65 30.58 19.22 30.58H28.78C29.35 30.58 29.71 29.97 29.43 29.47L24.65 21.13Z"
            />
          </symbol>
        </defs>
      </svg>

      <header className="nav">
        <div className="rail">
          <a href="/" aria-label="Open Agent Primitives home">
            <img className="wm wm-light" src={lightInk.src} alt="OAP" />
            <img className="wm wm-dark" src={darkInk.src} alt="OAP" />
          </a>
          <span className="crumb">
            <a href="#more">Stories</a> / 01
          </span>
          <nav className="nav-links">
            <a href="/docs/what-is-oap">Docs</a>
            <a href={REPO}>GitHub</a>
            <ThemeCycleButton />
            <a className="btn btn-ink" href="#try">
              Install messagingbot
            </a>
          </nav>
        </div>
      </header>

      <main>
        {/* HERO */}
        <section className="hero">
          <div className="rail">
            <div>
              <p className="kicker">Build your own · Story 01</p>
              <h1>
                We built our own <em>Claude Design</em>
              </h1>
              <p className="subtitle">and it&apos;s fully open source!</p>
              <p className="lede">
                <strong>Claude Design</strong> is Anthropic&apos;s tool for
                designing in conversation. Describe a page, a deck or a
                prototype, and Claude builds it in your brand on a canvas beside
                the chat; then you refine it together, by chatting, commenting
                on the design, or editing it directly, with every version kept.
                We wanted the same tools and capabilities for our own team, but
                completely open. We therefore built{" "}
                <strong>messagingbot</strong>, the design agent our marketing
                team uses, entirely out of pieces OAP already ships.
              </p>
              <div className="cta-row">
                <a className="btn btn-ink" href="#try">
                  Install messagingbot
                </a>
                <a className="btn btn-ghost" href={BUNDLE}>
                  View the bundle on GitHub
                </a>
              </div>
            </div>
            <figure className="clip">
              {clip && (
                <DemoClip
                  asset={clip}
                  still="/media/story-claude-design-hero.jpg"
                  alt="messagingbot's revision 2 open in the OAP artifact viewer: the warehouse hero now reads Every pick, on time. Even at peak., with a Book a demo button."
                />
              )}
              <figcaption>
                <span>
                  messagingbot taking a brief, reading the brand repos, building
                  a page, and revising it from notes pinned to the design.
                </span>
                <span className="sim">Simulated · fictional company</span>
              </figcaption>
              {clip?.transcript && (
                <details>
                  <summary>Transcript</summary>
                  <div
                    className="transcript"
                    tabIndex={0}
                    aria-label="Demo transcript"
                  >
                    {clip.transcript.map((line, i) => (
                      <p key={i}>{line}</p>
                    ))}
                  </div>
                </details>
              )}
            </figure>
          </div>
        </section>

        {/* AT A GLANCE */}
        <section className="glance" aria-label="At a glance">
          <div className="rail">
            <div className="glance-grid">
              <div className="glance-cell">
                <h3>What we built</h3>
                <p>
                  messagingbot, an agent that takes a marketing brief and builds
                  an HTML page from your real design tokens and approved copy.
                  Reviewers pin notes on the page and it revises from them.
                </p>
              </div>
              <div className="glance-cell">
                <h3>What it&apos;s made of</h3>
                <p>
                  An <a href="/docs/agentclass-agentsession">AgentClass</a>, a
                  read-only GitHub <a href="/docs/safe-tools">toolspec</a>, two
                  public <a href="/docs/skills">skills</a>, and the{" "}
                  <a href="/docs/artifacts">artifact system</a> with its{" "}
                  <a href="/docs/artifact-annotations">annotations</a>.
                </p>
              </div>
              <div className="glance-cell">
                <h3>What we didn&apos;t build</h3>
                <p>
                  The canvas, comment system, renderer, sandboxing and safe
                  artifacts are part of OAP. We built on top of these
                  primitives.
                </p>
              </div>
              <div className="glance-cell try">
                <h3>Try it</h3>
                <div className="term">
                  <span className="p">$ </span>
                  {INSTALL}
                  <CopyButton text={INSTALL} />
                </div>
                <a className="go" href={BUNDLE}>
                  Open-source example bundle{" "}
                  <span aria-hidden="true">&rarr;</span>
                </a>
              </div>
            </div>
          </div>
        </section>

        {/* 1. THE THING */}
        <section id="what">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">The original</p>
                <h2 className="h2">What is Claude Design?</h2>
              </div>
              <p className="lede">
                <a href="https://claude.com/product/design">Claude Design</a> is
                Anthropic&apos;s visual design tool. It{" "}
                <a href="https://www.anthropic.com/news/claude-design-anthropic-labs">
                  launched in April 2026
                </a>{" "}
                as an Anthropic Labs research preview, and has since been folded
                into ordinary Claude conversations. You describe what you want
                in chat, and Claude produces it on a canvas beside the
                conversation.
              </p>
            </div>
            <div className="feats">
              <div className="feat">
                <svg
                  viewBox="0 0 44 44"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <rect x="6" y="9" width="32" height="26" rx="3" />
                  <circle cx="14" cy="17" r="3" fill="currentColor" />
                  <circle cx="22" cy="17" r="3" />
                  <circle cx="30" cy="17" r="3" />
                  <path d="M11 26h22M11 30h14" />
                </svg>
                <h3>Brand setup</h3>
                <p>
                  During setup it builds a design system from your codebase and
                  design files, and every project after that uses your colours,
                  type, and components.
                </p>
              </div>
              <div className="feat">
                <svg
                  viewBox="0 0 44 44"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <rect x="6" y="7" width="32" height="24" rx="3" />
                  <path d="M12 15h14M12 20h20" opacity=".5" />
                  <rect x="10" y="12" width="18" height="6" rx="1.5" />
                  <circle
                    cx="29"
                    cy="31"
                    r="5"
                    fill="currentColor"
                    stroke="none"
                  />
                  <path d="M33 35l4 4" />
                </svg>
                <h3>Comments on the design</h3>
                <p>
                  You click an element on the page and leave a comment on it,
                  and Claude works from that comment.
                </p>
              </div>
              <div className="feat">
                <svg
                  viewBox="0 0 44 44"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <rect x="14" y="6" width="24" height="18" rx="2.5" />
                  <rect
                    x="10"
                    y="12"
                    width="24"
                    height="18"
                    rx="2.5"
                    fill="var(--card)"
                  />
                  <rect
                    x="6"
                    y="18"
                    width="24"
                    height="18"
                    rx="2.5"
                    fill="var(--card)"
                  />
                  <path d="M11 27h14M11 31h9" />
                </svg>
                <h3>Versions and handoff</h3>
                <p>
                  Each round of feedback produces a new version, and a finished
                  design can be handed off to Claude Code.
                </p>
              </div>
            </div>
            <p className="after-feats">
              This is quite similar to what our marketing team requested: a
              designer that already knows the brand and can take notes on a
              draft.
            </p>
          </div>
        </section>

        {/* INGREDIENTS (series signature) */}
        <section id="ingredients">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">The ingredients</p>
                <h2 className="h2">
                  What you need to <em>build one</em>
                </h2>
              </div>
              <p className="lede">
                What a design agent needs, and the OAP primitive that already
                provides it.
              </p>
            </div>
            <div
              className="ing"
              role="table"
              aria-label="Claude Design's parts and their OAP equivalents"
            >
              <div className="ing-row th" role="row">
                <span role="columnheader">Claude Design has</span>
                <span role="columnheader">So you need</span>
                <span aria-hidden="true"></span>
                <span role="columnheader" className="oap">
                  In OAP that&apos;s
                </span>
              </div>
              {INGREDIENTS.map((row) => (
                <div className="ing-row" role="row" key={row.has}>
                  <span className="ing-has" role="cell">
                    {row.has}
                  </span>
                  <span className="ing-need" role="cell">
                    {row.need}
                  </span>
                  <span className="ing-arrow" aria-hidden="true"></span>
                  <span className="ing-oap" role="cell">
                    <OapA />
                    <span>{row.oap}</span>
                  </span>
                </div>
              ))}
            </div>
            <p className="ing-foot">
              <OapA />
              Everything in the right column shipped with OAP before
              messagingbot existed.
            </p>
          </div>
        </section>

        {/* 2. HOW WE BUILT IT */}
        <section id="build">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">How we built it</p>
                <h2 className="h2">
                  The five pieces of <em>messagingbot</em>
                </h2>
              </div>
              <p className="lede">
                messagingbot is an agent definition, a read-only GitHub tool,
                two skills, and OAP&apos;s artifact viewer. Below is each piece
                with the config that sets it up.
              </p>
            </div>

            <div className="flow" aria-label="The five pieces">
              <a href="#step-1">
                <span className="node">
                  <svg
                    viewBox="0 0 30 30"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.4"
                  >
                    <path d="M5 7h20v13H13l-5 4v-4H5z" />
                    <path d="M13.5 11.5a1.8 1.8 0 1 1 2.4 1.7c-.6.2-.9.6-.9 1.2" />
                    <circle cx="15" cy="16.6" r=".7" fill="currentColor" />
                  </svg>
                </span>
                <span className="n">01</span>
                <span className="t">Scope the brief</span>
                <span className="s">a system prompt</span>
              </a>
              <a href="#step-2">
                <span className="node">
                  <svg viewBox="0 0 30 30">
                    <use href="#oap-a" x="5" y="5" width="20" height="20" />
                  </svg>
                </span>
                <span className="n">02</span>
                <span className="t">Read the brand</span>
                <span className="s">a read-only toolspec</span>
              </a>
              <a href="#step-3">
                <span className="node">
                  <svg
                    viewBox="0 0 30 30"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.4"
                  >
                    <rect x="9" y="5" width="14" height="18" rx="2" />
                    <rect
                      x="6"
                      y="8"
                      width="14"
                      height="18"
                      rx="2"
                      fill="var(--bg)"
                    />
                    <path d="M9.5 13h7M9.5 16.5h7M9.5 20h4" />
                  </svg>
                </span>
                <span className="n">03</span>
                <span className="t">Bring taste</span>
                <span className="s">a menu of skills</span>
              </a>
              <a href="#step-4">
                <span className="node">
                  <svg
                    viewBox="0 0 30 30"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.4"
                  >
                    <rect x="5" y="6" width="20" height="16" rx="2" />
                    <rect x="8.5" y="10" width="10" height="4" rx="1" />
                    <circle
                      cx="20"
                      cy="21"
                      r="3.2"
                      fill="currentColor"
                      stroke="none"
                    />
                  </svg>
                </span>
                <span className="n">04</span>
                <span className="t">Canvas and comments</span>
                <span className="s">two capabilities</span>
              </a>
              <a href="#step-5">
                <span className="node">
                  <svg
                    viewBox="0 0 30 30"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.4"
                  >
                    <rect x="4" y="6" width="22" height="18" rx="2" />
                    <path d="M8.5 12l3.5 3-3.5 3M14.5 18.5h6" />
                  </svg>
                </span>
                <span className="n">05</span>
                <span className="t">Customize it</span>
                <span className="s">three questions</span>
              </a>
            </div>

            <div className="step" id="step-1">
              <div>
                <span className="step-num">01</span>
                <h3>A prompt that scopes the brief first</h3>
                <div className="prose">
                  <p>
                    Before it designs anything, messagingbot asks for the
                    audience, the key message, the format, the call to action,
                    and which repositories hold the brand. It then posts a plan
                    and waits for answers.
                  </p>
                  <p>
                    Without those answers the page tends to come out generic, so
                    the prompt makes asking them the first step.
                  </p>
                </div>
              </div>
              <div className="vis">
                <p className="code-label">
                  From{" "}
                  <a
                    href={`${REPO}/tree/main/examples/messagingbot/manifests/agentclass.yaml`}
                  >
                    examples/messagingbot/manifests/agentclass.yaml
                  </a>
                </p>
                <pre className="code">{`Read the brief and decide what's missing. A good marketing piece needs,
at minimum: the audience, the goal/outcome, the medium/format, the
single key message, and the call-to-action. If any are unclear, ask 2-4
sharp clarifying questions ... BEFORE designing.

Never invent a colour, font, or claim that isn't grounded in what you read.`}</pre>
              </div>
            </div>

            <div className="step" id="step-2">
              <div>
                <span className="step-num">02</span>
                <h3>The brand, read-only</h3>
                <div className="prose">
                  <p>
                    messagingbot reads the brand with one tool, the GitHub CLI (
                    <code>gh</code>), in a sandbox that can only reach GitHub. A{" "}
                    <a href="/docs/safe-tools">toolspec</a> around it decides
                    which calls go through, and it only allows reads from
                    approved repositories.
                  </p>
                  <p>
                    The approved list (<code>config.readableRepos</code>) is set
                    by whoever installs the agent: just the repositories that
                    hold your design system and messaging. Even on those,
                    messagingbot can only read — the three subcommands above,
                    with <code>gh api</code> limited to GET requests. Any write,
                    or any request for a repository not on the list, is refused
                    before it reaches GitHub, regardless of what the prompt asks
                    for.
                  </p>
                </div>
              </div>
              <div className="vis">
                <figure
                  className="gate"
                  style={{ margin: 0 }}
                  aria-label="Two approved brand repositories pass the gate to GitHub; an unapproved repository is refused at the gate."
                >
                  <svg viewBox="0 0 560 220" role="img">
                    <title>
                      The toolspec gate: approved repositories pass, anything
                      else is refused
                    </title>
                    <g fill="none" strokeWidth="2" strokeLinecap="round">
                      <path
                        d="M176 52 C 230 52, 236 112, 268 112"
                        stroke="var(--state)"
                      />
                      <path d="M176 112 L 268 112" stroke="var(--state)" />
                      <path
                        d="M176 172 C 216 172, 228 160, 240 150"
                        stroke="var(--deny)"
                      />
                      <path d="M318 112 L 430 112" stroke="var(--state)" />
                    </g>
                    <g
                      stroke="var(--deny)"
                      strokeWidth="2.2"
                      strokeLinecap="round"
                    >
                      <path d="M240 142 l12 12 M252 142 l-12 12" />
                    </g>
                    <use
                      href="#oap-a"
                      x="252"
                      y="70"
                      width="84"
                      height="74"
                      style={{ color: "var(--heading)" }}
                    />
                    <g fontSize="12" fill="var(--fg)">
                      <text x="12" y="48">
                        acme/design
                      </text>
                      <text x="12" y="108">
                        acme/messaging
                      </text>
                      <text x="12" y="168" fill="var(--muted)">
                        acme/roadmap
                      </text>
                    </g>
                    <g fontSize="10" fill="var(--muted)">
                      <text x="12" y="63">
                        tokens, components
                      </text>
                      <text x="12" y="123">
                        pillars, voice, copy
                      </text>
                      <text x="12" y="183" fill="var(--deny)">
                        not in readableRepos
                      </text>
                      <text x="294" y="170" textAnchor="middle">
                        toolspec
                      </text>
                      <text x="294" y="184" textAnchor="middle">
                        GET only · approved repos
                      </text>
                    </g>
                    <rect
                      x="432"
                      y="90"
                      width="112"
                      height="44"
                      rx="8"
                      fill="var(--s2)"
                      stroke="var(--line)"
                    />
                    <text
                      x="488"
                      y="117"
                      textAnchor="middle"
                      fontSize="12"
                      fill="var(--fg)"
                    >
                      github.com
                    </text>
                    <text
                      x="252"
                      y="208"
                      fontSize="10.5"
                      fill="var(--deny)"
                      textAnchor="middle"
                    >
                      refused before it reaches GitHub
                    </text>
                  </svg>
                </figure>
                <pre className="code">
                  <span className="k">allowSubcommands</span>
                  {`: ["api", "repo view", "auth status"]`}
                </pre>
              </div>
            </div>

            <div className="step" id="step-3">
              <div>
                <span className="step-num">03</span>
                <h3>Design skills the installer picks</h3>
                <div className="prose">
                  <p>
                    Brand tokens cover colour and type. For layout and visual
                    judgment the agent uses <a href="/docs/skills">skills</a>,
                    which are instructions it loads when they&apos;re relevant.
                    The bundle includes two from Anthropic&apos;s public skills
                    repository, pinned to a commit: <code>frontend-design</code>{" "}
                    for visual direction, and <code>theme-factory</code>, which
                    offers ten preset palettes for teams without a brand system.
                  </p>
                  <p>
                    The bundle declares both and the installer chooses which
                    ones the agent gets. The <code>[?]</code> at the end of the
                    binding target filters <code>spec.skills</code> down to the
                    chosen names, and the install fails if a name isn&apos;t in
                    the list, so a typo can&apos;t leave the agent without a
                    skill you asked for.
                  </p>
                </div>
              </div>
              <div className="vis">
                <pre className="code">
                  {"- "}
                  <span className="k">name</span>
                  {": skills\n  "}
                  <span className="k">type</span>
                  {": resourceList\n  "}
                  <span className="k">prompt</span>
                  {': "Which design skills should messagingbot load?"\n  '}
                  <span className="k">enum</span>
                  {": [frontend-design, theme-factory]\n  "}
                  <span className="k">default</span>
                  {": [frontend-design, theme-factory]\n  "}
                  <span className="k">binding</span>
                  {':\n    - target: "AgentClass/messagingbot#spec.skills[?]"'}
                </pre>
              </div>
            </div>

            <div className="step" id="step-4">
              <div>
                <span className="step-num">04</span>
                <h3>Canvas and comments</h3>
                <div className="prose">
                  <p>
                    <code>artifacts.renderers</code> gives the agent the
                    artifact tools, limited to HTML, so it can render a page,
                    revise it, and share a live view.{" "}
                    <code>session_views.interactions</code> lets a reviewer in
                    that live view pin notes to parts of the page and send them
                    back.
                  </p>
                  <p>
                    Those two settings cover the versions, preview and
                    click-to-comment that this workflow uses from Claude
                    Design&apos;s canvas.
                  </p>
                </div>
              </div>
              <div className="vis">
                <pre className="code">
                  <span className="k">capabilities</span>
                  {":\n  "}
                  <span className="k">artifacts</span>
                  {":\n    "}
                  <span className="k">renderers</span>
                  {": [html]              "}
                  <span className="c"># the canvas</span>
                  {"\n  "}
                  <span className="k">session_views</span>
                  {":\n    "}
                  <span className="k">interactions</span>
                  {": [annotation_batch]  "}
                  <span className="c"># the comments</span>
                </pre>
              </div>
            </div>

            <div className="step" id="step-5">
              <div>
                <span className="step-num">05</span>
                <h3>Customize it</h3>
                <div className="prose">
                  <p>
                    You make messagingbot yours at install, by answering three
                    questions: a read-only GitHub token, the repositories that
                    hold your brand, and which skills to load. The bundle README
                    covers{" "}
                    <a href={`${BUNDLE}#read-only-access-three-rings`}>
                      scoping the token
                    </a>
                    .
                  </p>
                </div>
              </div>
              <div className="vis">
                <pre className="code">
                  <span className="c">$</span>
                  {" oap agent lint    examples/messagingbot\n"}
                  <span className="c">$</span>
                  {
                    " oap agent install examples/messagingbot --namespace default\n"
                  }
                  <span className="c">$</span>
                  {" oap channel create --kind slack   "}
                  <span className="c"># bind it to messagingbot</span>
                </pre>
              </div>
            </div>
          </div>
        </section>

        {/* SEE IT WORK */}
        <section id="pin">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">See it work</p>
                <h2 className="h2">
                  What happens when you <em>pin a note</em>
                </h2>
              </div>
              <p className="lede">
                In the demo, Jordan (on a fictional marketing team) leaves two
                notes on revision 1, and messagingbot addresses both in revision
                2. Use the tabs to compare them.
              </p>
            </div>

            <BeforeAfter
              rev1={{
                src: "/media/story-claude-design-rev1.jpg",
                alt: "Revision 1: headline Smarter robots for a smarter warehouse with note 1 pinned to it, and a Learn more button with note 2 pinned to it.",
              }}
              rev2={{
                src: "/media/story-claude-design-rev2.jpg",
                alt: "Revision 2: headline Every pick, on time. Even at peak. with an orange Book a demo button.",
              }}
              panel1={
                <>
                  <h4>Annotations</h4>
                  <div className="note">
                    <div className="on">
                      <span className="pin">1</span>jordan, on &quot;Smarter
                      robots for a smarter...&quot;
                    </div>
                    <p>
                      Generic. Lead with our peak-season pillar, the outcome,
                      not the AI.
                    </p>
                  </div>
                  <div className="note">
                    <div className="on">
                      <span className="pin">2</span>jordan, on &quot;Learn
                      more&quot;
                    </div>
                    <p>
                      Should be &quot;Book a demo&quot;, as a primary button in
                      Signal Orange.
                    </p>
                  </div>
                  <p className="ba-send">
                    Send bundles both notes into one batch, each with where on
                    the page it was made.
                  </p>
                </>
              }
              panel2={
                <>
                  <h4>Change note</h4>
                  <div className="note done">
                    <div className="on">
                      <span className="pin">1</span>headline
                    </div>
                    <p>&quot;Every pick, on time. Even at peak.&quot;</p>
                    <div className="ans">
                      <b>From</b> acme/messaging pillars.md
                    </div>
                  </div>
                  <div className="note done">
                    <div className="on">
                      <span className="pin">2</span>primary CTA
                    </div>
                    <p>&quot;Book a demo&quot;, primary button</p>
                    <div className="ans">
                      <b>Change</b> primary CTA, Signal Orange
                    </div>
                  </div>
                  <p className="ba-send">
                    Revision 1 is still in the history, unchanged.
                  </p>
                </>
              }
            />
            <div className="ba-foot">
              <span>Shots from the artifact viewer.</span>
              <span className="sim">Simulated · fictional company</span>
            </div>

            <div className="beats">
              <div className="beat">
                <div className="chk">
                  <i>1</i>Pin
                </div>
                <h3>The note is attached to the element</h3>
                <p>
                  When Jordan clicks the headline in the viewer and types a
                  note, the note is attached to that element. Pressing{" "}
                  <strong>Send</strong> bundles every note up and sends them to
                  the agent together, each with the details of where on the page
                  it was made.
                </p>
              </div>
              <div className="beat">
                <div className="chk">
                  <i>2</i>Check
                </div>
                <h3>OAP authorizes the feedback first</h3>
                <p>
                  Before the agent sees any of it, OAP checks that Jordan is
                  allowed to view this exact artifact and to take part in the
                  session. Jordan&apos;s notes reach the agent as{" "}
                  <strong>Jordan&apos;s words</strong>, while anything taken
                  from the page itself is treated as <strong>data</strong>, so
                  text inside a design can&apos;t pose as an instruction.
                </p>
              </div>
              <div className="beat">
                <div className="chk">
                  <i>3</i>Revise
                </div>
                <h3>The agent revises the same artifact</h3>
                <p>
                  Revision 2 includes a change note that responds to each
                  annotation. Revision 1 stays in the history as it was.
                </p>
              </div>
            </div>
          </div>
        </section>

        {/* WHY OAP */}
        <section id="why">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">Why OAP</p>
                <h2 className="h2">
                  Why it was <em>easy on OAP</em>
                </h2>
              </div>
              <p className="lede">
                OAP is a secure foundation for building any agent: identity,
                authorization checked before every action, sandboxed tools, safe
                artifacts and an audit trail all come with the platform. That
                made messagingbot easy to build. We described the agent we
                wanted, and everything below came with it.
              </p>
            </div>
            <div className="why">
              <WhyItem
                hue="var(--h0)"
                title="Runs in your cluster"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <path d="M20 5l13 7.5v15L20 35 7 27.5v-15z" />
                    <path d="M7 12.5L20 20l13-7.5M20 20v15" />
                  </svg>
                }
              >
                messagingbot uses whatever model your cluster is configured with
                (Anthropic, OpenAI or OpenRouter). Briefs and unreleased
                messaging stay on infrastructure you run.
              </WhyItem>
              <WhyItem
                hue="var(--h1)"
                title="Works in Slack and the browser"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <path d="M6 9h20v13H14l-5 4v-4H6z" />
                    <path d="M30 15h4v13h-3v4l-5-4h-9v-2" />
                  </svg>
                }
              >
                The same agent answers in a Slack thread or OAP&apos;s browser
                chat, and the conversation stays linked to the design it
                produced.
              </WhyItem>
              <WhyItem
                hue="var(--h2)"
                title="Reads only approved repositories"
                icon={
                  <svg viewBox="0 0 40 40">
                    <use href="#oap-a" x="5" y="6" width="30" height="27" />
                  </svg>
                }
              >
                The toolspec enforces the repository allowlist outside the
                model. There&apos;s also a read-only token and a sandbox that
                can only reach GitHub, and none of the three rely on the model
                following instructions.
              </WhyItem>
              <WhyItem
                hue="var(--h3)"
                title="Pages can't run scripts"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <rect x="6" y="8" width="28" height="24" rx="3" />
                    <path d="M6 14h28" />
                    <path d="M15 21l10 6M25 21l-10 6" />
                  </svg>
                }
              >
                Artifacts render <a href="/docs/artifact-safety">inert</a>, so
                scripts in a model-written page don&apos;t execute when you open
                it.
              </WhyItem>
              <WhyItem
                hue="var(--h4)"
                title="Feedback is checked"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <circle cx="14" cy="15" r="6" />
                    <path d="M14 12v3l2 1.5" />
                    <path d="M22 22l12 0M22 27h12M22 32h8" />
                    <path d="M6 30c1.5-4 4.5-6 8-6" />
                  </svg>
                }
              >
                An annotation can only target an artifact its sender may view,
                and nothing scraped from the page reaches the agent as an
                instruction.
              </WhyItem>
              <WhyItem
                hue="var(--h0)"
                title="Versions and an audit log"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <rect x="12" y="6" width="22" height="16" rx="2" />
                    <rect
                      x="9"
                      y="11"
                      width="22"
                      height="16"
                      rx="2"
                      fill="var(--bg)"
                    />
                    <rect
                      x="6"
                      y="16"
                      width="22"
                      height="16"
                      rx="2"
                      fill="var(--bg)"
                    />
                    <path d="M10 24h14M10 28h9" />
                  </svg>
                }
              >
                Revisions are immutable and can be{" "}
                <a href="/docs/artifact-annotations">tagged</a> (for example
                &quot;approved hero&quot;). Each session&apos;s transcript is a
                signed, append-only <a href="/docs/audit-log">audit log</a> you
                can verify offline to see what the agent read and what it was
                asked to change.
              </WhyItem>
              <WhyItem
                hue="var(--h1)"
                title="Swappable parts"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <path d="M8 14h22l-5-5M32 26H10l5 5" />
                  </svg>
                }
              >
                Skills and brand repositories are chosen at install and the
                model is a cluster setting, so the same bundle works for a
                different brand by pointing it at different repos.
              </WhyItem>
              <WhyItem
                hue="var(--h2)"
                title="Multiplayer, with the owner's say-so"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                  >
                    <circle cx="14" cy="14" r="5" />
                    <path d="M5 31c0-5 4-8.5 9-8.5s9 3.5 9 8.5" />
                    <circle cx="28" cy="16" r="4" />
                    <path d="M25 23.5c1-.6 2-.9 3-.9 4 0 7 2.8 7 7" />
                    <path d="M29 6.5l2 2 3.5-3.5" />
                  </svg>
                }
              >
                A design review is rarely one person, and{" "}
                <a href="/docs/multiplayer-sessions">multiplayer sessions</a>{" "}
                work out of the box. A colleague in the thread asks to join and
                suggest changes, and the session&apos;s owner approves them
                first. Once approved, they can chat with messagingbot and pin
                notes on the design just like the person who started it.
              </WhyItem>
            </div>
          </div>
        </section>

        {/* STILL AHEAD */}
        <section id="ahead">
          <div className="rail">
            <div className="ahead">
              <div>
                <p className="kicker">Comparison</p>
                <h2 className="h2" style={{ marginBottom: 22 }}>
                  Where Claude Design is still ahead
                </h2>
                <div className="prose">
                  <p>
                    Claude Design does more today: direct editing on the canvas,
                    slides and prototypes, export to tools like Canva, and
                    handoff to Claude Code. Each could be added to messagingbot
                    with parts OAP already has. Our designers just haven&apos;t
                    needed them (yet).
                  </p>
                  <p>
                    messagingbot is open source, so if you need one first,{" "}
                    <a href={BUNDLE}>contributions are welcome</a>!
                  </p>
                </div>
              </div>
              <div className="seams">
                <div className="seams-h">
                  <span>Missing today</span>
                  <span>How it could be added</span>
                </div>
                <div className="seam">
                  <b>More formats</b>
                  <p>
                    Artifact renderers are{" "}
                    <a href="/docs/artifacts">pluggable</a>, so slides or
                    prototypes would be a new renderer or skill.
                  </p>
                </div>
                <div className="seam">
                  <b>Export to other tools</b>
                  <p>
                    Connect the tool&apos;s API as an{" "}
                    <a href="/docs/mcp-servers">MCP server</a> and give the
                    agent a tool to publish with.
                  </p>
                </div>
                <div className="seam">
                  <b>Handoff to code</b>
                  <p>
                    OAP agents can{" "}
                    <a href="/docs/subagents-delegation">
                      delegate to other agents
                    </a>
                    , so a finished design can go to a coding agent like{" "}
                    <a href="/docs/codebot">codebot</a>, which opens the pull
                    request.
                  </p>
                </div>
                <div className="seam">
                  <b>Design-system setup</b>
                  <p>
                    A skill could read your codebase and draft a tokens file as
                    an artifact for someone to review and commit, so the agent
                    never needs write access.
                  </p>
                </div>
              </div>
            </div>
          </div>
        </section>

        {/* FAQ */}
        <section id="faq">
          <div className="rail">
            <div className="head">
              <div>
                <p className="kicker">FAQ</p>
                <h2 className="h2">Frequently asked questions</h2>
              </div>
            </div>
            <div className="faq">
              {[FAQ.slice(0, 4), FAQ.slice(4)].map((col, c) => (
                <div key={c}>
                  {col.map((it) => (
                    <details key={it.id} id={it.id}>
                      <summary>{it.q}</summary>
                      <p>{withCode(it.a)}</p>
                    </details>
                  ))}
                </div>
              ))}
            </div>
          </div>
        </section>

        {/* 3. TRY IT */}
        <section id="try">
          <div className="rail">
            <div className="try-band">
              <div>
                <p className="kicker">Try it</p>
                <h2 className="h2" style={{ marginBottom: 16 }}>
                  Try messagingbot <em>with your brand</em>
                </h2>
                <p className="lede">
                  Install the bundle, answer the three install questions, and
                  bind it to a Slack channel. The install is where you point it
                  at your own design and messaging repos.
                </p>
                <div className="cta-row">
                  <a className="btn btn-ink" href={BUNDLE}>
                    View the bundle on GitHub
                  </a>
                  <a className="btn btn-ghost" href="/docs/what-is-oap">
                    New to OAP?
                  </a>
                </div>
              </div>
              <div className="term">
                <CopyButton text={INSTALL_ALL} />
                <span className="p">$ </span>oap agent lint
                examples/messagingbot
                <br />
                <span className="p">$ </span>oap agent install
                examples/messagingbot --namespace default
                <br />
                <span className="p">$ </span>oap channel create --kind slack{" "}
                <span className="c"># bind it to messagingbot</span>
              </div>
            </div>
            <div className="deeper">
              <a href="/docs/artifacts">
                <b>Artifacts</b>
                <span>
                  The durable, versioned home for everything messagingbot
                  designs.
                </span>
              </a>
              <a href="/docs/artifact-annotations">
                <b>Annotations &amp; tags</b>
                <span>How notes on a rendered artifact reach the agent.</span>
              </a>
              <a href="/docs/artifact-safety">
                <b>Rendered inert</b>
                <span>Why a model-written page can&apos;t run scripts.</span>
              </a>
              <a href="/docs/skills">
                <b>Skills</b>
                <span>How agents load instructions on demand.</span>
              </a>
              <a href="/docs/safe-tools">
                <b>Safe tools</b>
                <span>How a toolspec bounds what a CLI can do.</span>
              </a>
              <a href="/docs/packaging-oap">
                <b>Packaging an agent</b>
                <span>
                  Install questions, bindings, and the <code>.oap</code> format.
                </span>
              </a>
            </div>
          </div>
        </section>

        {/* SERIES */}
        <section id="more">
          <div className="rail">
            <p className="kicker">Build your own</p>
            <div className="series">
              <a className="story here" href={PATH}>
                <span className="no">01 · you are here</span>
                <b>
                  <span className="pre">We built our own</span>Claude Design
                </b>
              </a>
              <div className="story soon">
                <span className="no">02 · next story</span>
                <b>
                  <span className="pre">We built our own</span>...
                </b>
              </div>
              <div className="story soon">
                <span className="no">03</span>
                <b>
                  <span className="pre">We built our own</span>...
                </b>
              </div>
            </div>
            <p className="disclaimer">
              The demo on this page is a scripted simulation of Slack and the
              artifact viewer; its company, people, and repositories are
              fictional.
            </p>
          </div>
        </section>
      </main>

      <footer>
        <div className="rail">
          <span>
            Built by <a href="https://authzed.com/">AuthZed</a>, using{" "}
            <a href="https://github.com/authzed/spicedb">SpiceDB</a>. Open
            source under the{" "}
            <a href={`${REPO}/blob/main/LICENSE`}>Apache 2.0 license</a>.
          </span>
          <span>
            <a href="/docs/what-is-oap">Docs</a> · <a href={REPO}>GitHub</a>
          </span>
        </div>
      </footer>

      <JsonLd
        data={storyGraph(
          {
            title: TITLE,
            description: DESCRIPTION,
            path: PATH,
            keywords: KEYWORDS,
            published: PUBLISHED,
            updated: UPDATED,
            image: OG_IMAGE.url,
          },
          FAQ,
          clip,
        )}
      />
    </div>
  );
}

function WhyItem({
  hue,
  title,
  icon,
  children,
}: {
  hue: string;
  title: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="why-item" style={{ "--hue": hue } as CSSProperties}>
      {icon}
      <div>
        <h3>{title}</h3>
        <p>{children}</p>
      </div>
    </div>
  );
}
