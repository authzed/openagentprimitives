import type { Metadata } from "next";
import type { ReactNode } from "react";
import { JsonLd } from "@/components/JsonLd";
import { SiteFooter } from "@/components/SiteFooter";
import { SiteNav } from "@/components/SiteNav";
import { getAsset } from "@/lib/manifest";
import { OG_IMAGE, SITE_NAME } from "@/lib/site";
import { storyGraph, type FaqEntry } from "@/lib/structured-data";
import { BeforeAfter, CopyButton, DemoClip } from "../StoryClient";
import "../../(landing)/landing.css";
import "../stories.css";

// A "Build your own" story: a familiar product as the anchor, then the same
// thing rebuilt from OAP's primitives, for a visitor who knows the product but
// not OAP. Order: hero, at a glance, the original, ingredients, the build, see
// it work, why OAP, still ahead, FAQ, try it. The claims follow
// examples/messagingbot; keep them in step with the bundle.

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
const UPDATED = "2026-11-06";

const REPO = "https://github.com/authzed/openagentprimitives";
const BUNDLE = `${REPO}/tree/main/examples/messagingbot`;

// Step 2 of "Try it"; step 1 (installing OAP) links to its own guide.
// `oap agent install` takes a local bundle, so it runs from a repo clone.
const INSTALL: { note?: string; cmd?: string }[] = [
  { note: "# from a clone of the repo" },
  { cmd: "oap agent install examples/messagingbot --namespace default" },
  { cmd: "oap channel create --kind slack" },
];
const INSTALL_TEXT = INSTALL.map((l) => l.note ?? l.cmd).join("\n");

// The toolspec's own refusal message, verbatim from
// examples/messagingbot/manifests/toolspec.yaml (the repository ring).
const REFUSAL =
  "This repository is not one messagingbot may read (spec.config.readableRepos, set by the operator at install). Ask the user which approved repository holds what you need.";

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
    a: "Yes. messagingbot reproduces the core Claude Design workflow on OAP: a brief in chat, a design grounded in your brand system, click-to-comment feedback on the rendered page, and versioned revisions. It is built from an agent definition, a read-only GitHub tool, two agent skills, and OAP's artifact system, with no new platform code.",
  },
  {
    id: "faq-code",
    q: "Do I have to write code to run it?",
    a: "No. messagingbot ships as an example agent bundle in the OAP repository. Install it with `oap agent install examples/messagingbot`, answer three questions (a GitHub token, your brand repositories, and which skills to load), and bind it to a Slack channel or use the browser chat.",
  },
  {
    id: "faq-brand",
    q: "Where does messagingbot get our brand from?",
    a: "From the GitHub repositories you name at install, such as one holding colour and type tokens and one holding approved messaging. It reads them through the gh CLI, and its toolspec refuses writes and any repository that is not on the approved list.",
  },
  {
    id: "faq-safe",
    q: "Is it safe to open HTML a model wrote?",
    a: "Yes. OAP renders every artifact inert: you see the page, but no script in it runs. messagingbot is also instructed to produce HTML and CSS only.",
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

function OapA({ className = "icon-a" }: { className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
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
    oap: <a href="/docs/artifact-annotations">Artifact annotations</a>,
  },
  {
    has: "Your design system",
    need: "Read access to the brand's source of truth, and nothing more",
    oap: (
      <>
        A <a href="/docs/toolspec-validation">toolspec</a> that limits the
        GitHub CLI to reads of approved repositories
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
    oap: <a href="/docs/artifact-safety">Inert rendering</a>,
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
    <div className="lp st">
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

      <SiteNav
        cta={{
          href: "#try",
          label: (
            <span>
              Install<span className="st-nav-long"> messagingbot</span>
            </span>
          ),
        }}
      >
        <a className="lp-nav-keep" href="/docs/what-is-oap">
          Docs
        </a>
        <a href="#build">How it&apos;s built</a>
        <a href="#pin">See it work</a>
      </SiteNav>

      <main>
        {/* HERO */}
        <section className="hero">
          <div className="rail">
            <div>
              <h1>
                We built our own <em>Claude Design</em>
              </h1>
              <p className="subtitle">
                We built it on OAP, open-source building blocks for running AI
                agents on your own infra and with the models you choose. The
                agent never decides what it&apos;s allowed to do; OAP does.
              </p>
              <p className="lede">
                <strong>Claude Design</strong> is Anthropic&apos;s tool for
                designing in conversation: describe a page, Claude builds it in
                your brand, and you refine it by commenting on the design.{" "}
                <strong>messagingbot</strong> is our version.
              </p>
              <div className="cta-row">
                <a className="lp-btn lp-btn--primary" href="#try">
                  Install messagingbot
                </a>
                <a className="lp-btn lp-btn--ghost" href={BUNDLE}>
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
                  messagingbot taking a brief, reading the brand, building a
                  page, and revising it from notes pinned to the design.
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
          <div className="glance-grid">
            <div className="glance-cell">
              <p className="glance-h">What we built</p>
              <p>
                messagingbot, an agent that takes a marketing brief and builds
                an HTML page from your real design tokens and approved copy.
                Reviewers pin notes on the page and it revises from them.
              </p>
            </div>
            <div className="glance-cell">
              <p className="glance-h">What it&apos;s made of</p>
              <p>
                Five pieces of configuration: a{" "}
                <a href="/docs/agentclass-agentsession">prompt</a>, a{" "}
                <a href="/docs/safe-tools">read-only GitHub tool</a>, two{" "}
                <a href="/docs/skills">design skills</a>, OAP&apos;s{" "}
                <a href="/docs/artifacts">canvas</a> with{" "}
                <a href="/docs/artifact-annotations">comments</a>, and three
                install questions.
              </p>
            </div>
            <div className="glance-cell">
              <p className="glance-h">What we didn&apos;t build</p>
              <p>
                The canvas, the comment system, the renderer, the sandbox and
                the permission checks. Those are OAP; messagingbot is
                configuration on top.
              </p>
            </div>
            <div className="glance-cell deny">
              <p className="glance-h">What it can&apos;t do</p>
              <p>
                Slip your unreleased roadmap into public copy. It reads only the
                brand sources you approve; a request for anything else is
                refused, whatever the prompt says.
              </p>
              <a className="go" href="#step-2">
                See the refusal <span aria-hidden="true">&rarr;</span>
              </a>
            </div>
          </div>
        </section>

        {/* THE ORIGINAL */}
        <section id="what">
          <div className="rail">
            <div className="head">
              <h2 className="h2">What is Claude Design?</h2>
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
                  aria-hidden="true"
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
                  aria-hidden="true"
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
                  aria-hidden="true"
                >
                  <rect x="14" y="6" width="24" height="18" rx="2.5" />
                  <rect
                    x="10"
                    y="12"
                    width="24"
                    height="18"
                    rx="2.5"
                    fill="var(--lp-bg)"
                  />
                  <rect
                    x="6"
                    y="18"
                    width="24"
                    height="18"
                    rx="2.5"
                    fill="var(--lp-bg)"
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
              That is close to what our marketing team asked for: a designer
              that already knows the brand and can take notes on a draft.
            </p>
          </div>
        </section>

        {/* INGREDIENTS */}
        <section id="ingredients">
          <div className="rail">
            <div className="head">
              <h2 className="h2">What you need to build one</h2>
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

        {/* HOW WE BUILT IT */}
        <section id="build">
          <div className="rail">
            <div className="head">
              <h2 className="h2">The five pieces of messagingbot</h2>
              <p className="lede">
                messagingbot adds no new platform code. It is a small bundle
                that tells OAP how this agent should behave, what it may touch,
                and what to ask whoever installs it. Each piece below sits next
                to the configuration that sets it up.
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
                    aria-hidden="true"
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
                  <svg viewBox="0 0 30 30" aria-hidden="true">
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
                    aria-hidden="true"
                  >
                    <rect x="9" y="5" width="14" height="18" rx="2" />
                    <rect
                      x="6"
                      y="8"
                      width="14"
                      height="18"
                      rx="2"
                      fill="var(--lp-bg)"
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
                    aria-hidden="true"
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
                    aria-hidden="true"
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
                  <a href={`${BUNDLE}/manifests/agentclass.yaml`}>
                    manifests/agentclass.yaml
                  </a>
                </p>
                <pre className="code wrap">{`Read the brief and decide what's missing. A good marketing piece needs, at minimum: the audience, the goal/outcome, the medium/format, the single key message, and the call-to-action. If any are unclear, ask 2-4 sharp clarifying questions ... BEFORE designing.

Never invent a colour, font, or claim that isn't grounded in what you read.`}</pre>
              </div>
            </div>

            <div className="step step-peak" id="step-2">
              <div>
                <span className="step-num">02</span>
                <h3>The brand, read-only</h3>
                <div className="prose">
                  <p>
                    messagingbot reads the brand with one tool, the GitHub CLI.
                    A <a href="/docs/safe-tools">toolspec</a> around it decides
                    which calls go through: read requests only, and only on the
                    repositories whoever installed the agent approved, such as
                    the ones holding your design tokens and messaging.
                  </p>
                  <p>
                    Anything else is{" "}
                    <strong>refused before it reaches GitHub</strong>, and the
                    agent is told why. The check runs outside the model, so no
                    prompt can talk its way past it.
                  </p>
                </div>
              </div>
              <div className="vis">
                <figure className="gate">
                  <svg
                    viewBox="0 0 560 230"
                    role="img"
                    aria-label="Two approved brand repositories pass the toolspec gate to GitHub; an unapproved repository is refused at the gate."
                  >
                    <g fill="none" strokeWidth="2" strokeLinecap="round">
                      <path
                        d="M176 52 C 230 52, 236 112, 268 112"
                        stroke="var(--lp-muted)"
                      />
                      <path d="M176 112 L 268 112" stroke="var(--lp-muted)" />
                      <path
                        d="M176 172 C 216 172, 228 160, 240 150"
                        stroke="var(--lp-deny)"
                        strokeWidth="2.6"
                      />
                      <path d="M318 112 L 430 112" stroke="var(--lp-muted)" />
                    </g>
                    <g
                      stroke="var(--lp-deny)"
                      strokeWidth="2.8"
                      strokeLinecap="round"
                    >
                      <path d="M238 140 l14 14 M252 140 l-14 14" />
                    </g>
                    <use
                      href="#oap-a"
                      x="252"
                      y="70"
                      width="84"
                      height="74"
                      style={{ color: "var(--lp-heading)" }}
                    />
                    <g fontSize="14" fill="var(--lp-fg)">
                      <text x="12" y="48">
                        acme/design
                      </text>
                      <text x="12" y="108">
                        acme/messaging
                      </text>
                      <text x="12" y="168">
                        acme/roadmap
                      </text>
                    </g>
                    <g fontSize="12" fill="var(--lp-muted)">
                      <text x="12" y="65">
                        tokens, components
                      </text>
                      <text x="12" y="125">
                        pillars, voice, copy
                      </text>
                      <text x="12" y="185" fill="var(--lp-deny)">
                        not approved
                      </text>
                      <text x="294" y="170" textAnchor="middle">
                        toolspec
                      </text>
                      <text x="294" y="186" textAnchor="middle">
                        reads · approved repos
                      </text>
                    </g>
                    <rect
                      x="432"
                      y="90"
                      width="112"
                      height="44"
                      rx="6"
                      fill="var(--lp-surface-2)"
                      stroke="var(--lp-line)"
                    />
                    <text
                      x="488"
                      y="117"
                      textAnchor="middle"
                      fontSize="14"
                      fill="var(--lp-fg)"
                    >
                      github.com
                    </text>
                    <text
                      x="245"
                      y="218"
                      fontSize="14"
                      fill="var(--lp-deny)"
                      textAnchor="middle"
                    >
                      refused before it reaches GitHub
                    </text>
                  </svg>
                  {/* The same gate as a list, for widths where the diagram's
                      labels would shrink below reading size. */}
                  <ul className="gate-list">
                    <li>
                      <span>acme/design</span>
                      <b>read</b>
                    </li>
                    <li>
                      <span>acme/messaging</span>
                      <b>read</b>
                    </li>
                    <li className="no">
                      <span>acme/roadmap</span>
                      <b>refused before it reaches GitHub</b>
                    </li>
                  </ul>
                </figure>
                <figure className="refusal">
                  <figcaption>
                    What messagingbot gets back when it asks for acme/roadmap
                  </figcaption>
                  <div className="refusal-call">
                    <span className="p">$ </span>gh api
                    repos/acme/roadmap/readme
                  </div>
                  <p className="refusal-msg">{REFUSAL}</p>
                  <p className="refusal-src">
                    The message is the toolspec&apos;s own, from{" "}
                    <a href={`${BUNDLE}/manifests/toolspec.yaml`}>
                      manifests/toolspec.yaml
                    </a>
                    .
                  </p>
                </figure>
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
                    instructions it loads when they&apos;re relevant. The bundle
                    includes two from Anthropic&apos;s public skills repository,
                    pinned to a commit: <code>frontend-design</code> for visual
                    direction, and <code>theme-factory</code>, which offers ten
                    preset palettes for teams without a brand system.
                  </p>
                  <p>
                    Whoever installs it picks which ones the agent gets, and a
                    misspelled name fails the install instead of quietly leaving
                    a skill out.
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
                  {":\n    - target:\n        "}
                  {'"AgentClass/messagingbot#spec.skills[?]"'}
                </pre>
              </div>
            </div>

            <div className="step" id="step-4">
              <div>
                <span className="step-num">04</span>
                <h3>Canvas and comments</h3>
                <div className="prose">
                  <p>
                    Two settings give messagingbot its canvas. The first lets it
                    render HTML pages, revise them, and share a live view. The
                    second lets a reviewer in that live view pin notes to parts
                    of the page and send them back.
                  </p>
                  <p>
                    Together they cover the versions, preview and
                    click-to-comment this workflow uses from Claude
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
                    questions: a GitHub token, the repositories that hold your
                    brand, and which skills to load. Give it a read-only token;
                    the bundle README covers{" "}
                    <a href={`${BUNDLE}#read-only-access-three-rings`}>
                      scoping it
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
              <h2 className="h2">What happens when you pin a note</h2>
              <p className="lede">
                In the demo, Jordan (on a fictional marketing team) leaves two
                notes on revision 1, and messagingbot answers both in revision
                2. Switch to revision 2 to see the result.
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
                  <p className="ba-h">Annotations</p>
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
                    Each note is pinned to the element it&apos;s about. Send
                    delivers both together, and OAP first checks that Jordan may
                    view this design and take part in the session.
                  </p>
                </>
              }
              panel2={
                <>
                  <p className="ba-h">Change note</p>
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
                    Same artifact, new revision. Revision 1 is still in the
                    history, unchanged.
                  </p>
                </>
              }
            />
            <div className="ba-foot">
              <span>The artifact viewer, from the scripted demo.</span>
              <span className="sim">Simulated · fictional company</span>
            </div>
          </div>
        </section>

        {/* WHY OAP */}
        <section id="why">
          <div className="rail">
            <div className="head">
              <h2 className="h2">
                Why it was <em>easy on OAP</em>
              </h2>
              <p className="lede">
                We chose OAP because a design agent your whole team uses needs
                more than a canvas. Everything below comes with the platform, so
                we described the agent and switched it on.
              </p>
            </div>
            <div className="why">
              <WhyItem
                title="Guardrails outside the model"
                icon={
                  <svg viewBox="0 0 40 40" aria-hidden="true">
                    <use href="#oap-a" x="5" y="6" width="30" height="27" />
                  </svg>
                }
              >
                The repository gate, inert rendering and the check on every
                reviewer are OAP&apos;s, not code in the bundle, and no prompt
                can switch them off. <a href="#step-2">See the refusal</a>.
              </WhyItem>
              <WhyItem
                title="A record you can verify"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    aria-hidden="true"
                  >
                    <rect x="12" y="6" width="22" height="16" rx="2" />
                    <rect
                      x="9"
                      y="11"
                      width="22"
                      height="16"
                      rx="2"
                      fill="var(--lp-bg)"
                    />
                    <rect
                      x="6"
                      y="16"
                      width="22"
                      height="16"
                      rx="2"
                      fill="var(--lp-bg)"
                    />
                    <path d="M10 24h14M10 28h9" />
                  </svg>
                }
              >
                Revisions can&apos;t be changed after the fact, and each
                session&apos;s transcript is a signed{" "}
                <a href="/docs/audit-log">audit log</a> you can verify offline:
                what the agent read, and who asked for what.
              </WhyItem>
              <WhyItem
                title="Runs in your cluster"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    aria-hidden="true"
                  >
                    <path d="M20 5l13 7.5v15L20 35 7 27.5v-15z" />
                    <path d="M7 12.5L20 20l13-7.5M20 20v15" />
                  </svg>
                }
              >
                The agent, its designs and its logs live on infrastructure you
                run. The model is whichever provider your cluster is configured
                with: Anthropic, OpenAI or OpenRouter.
              </WhyItem>
              <WhyItem
                title="Where your team already talks"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    aria-hidden="true"
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
                title="Swappable parts"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    aria-hidden="true"
                  >
                    <path d="M8 14h22l-5-5M32 26H10l5 5" />
                  </svg>
                }
              >
                Skills and brand repositories are chosen at install and the
                model is a cluster setting, so the same bundle works for another
                brand by pointing it at different repos.
              </WhyItem>
              <WhyItem
                title="Multiplayer, with the owner's say-so"
                icon={
                  <svg
                    viewBox="0 0 40 40"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    aria-hidden="true"
                  >
                    <circle cx="14" cy="14" r="5" />
                    <path d="M5 31c0-5 4-8.5 9-8.5s9 3.5 9 8.5" />
                    <circle cx="28" cy="16" r="4" />
                    <path d="M25 23.5c1-.6 2-.9 3-.9 4 0 7 2.8 7 7" />
                    <path d="M29 6.5l2 2 3.5-3.5" />
                  </svg>
                }
              >
                A colleague in the thread asks to join, the session&apos;s owner
                approves, and then they can chat with messagingbot and pin notes
                too. <a href="/docs/multiplayer-sessions">Multiplayer</a> works
                out of the box.
              </WhyItem>
            </div>
          </div>
        </section>

        {/* STILL AHEAD */}
        <section id="ahead">
          <div className="rail">
            <div className="ahead">
              <div>
                <h2 className="h2">Where Claude Design is still ahead</h2>
                <div className="prose">
                  <p>
                    Claude Design does more today: direct editing on the canvas,
                    slides and prototypes, export to tools like Canva, and
                    handoff to Claude Code. Most of these could be added to
                    messagingbot with parts OAP already has; our marketing team
                    just hasn&apos;t needed them yet.
                  </p>
                  <p>
                    messagingbot is open source, so if you need one first,{" "}
                    <a href={BUNDLE}>contributions are welcome</a>.
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
                    prototypes would be a new renderer, which is new code.
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

        {/* FAQ, then further reading */}
        <section id="faq">
          <div className="rail">
            <div className="head">
              <h2 className="h2">Frequently asked questions</h2>
            </div>
            <div className="faq">
              {[FAQ.slice(0, 3), FAQ.slice(3)].map((col, c) => (
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
            <p className="disclaimer">
              The demo on this page is a scripted simulation of Slack and the
              artifact viewer; its company, people, and repositories are
              fictional. The configuration shown is the real bundle.
            </p>
          </div>
        </section>

        {/* TRY IT: the page ends on the action */}
        <section id="try">
          <div className="rail">
            <div className="try-band">
              <div>
                <h2 className="h2">Try messagingbot with your brand</h2>
                <p className="lede">
                  Two steps: get OAP running, then install the bundle. Its
                  install questions are where you point it at your own design
                  and messaging repos.
                </p>
              </div>
              <ol className="try-steps">
                <li>
                  <p className="try-step-h">
                    <span aria-hidden="true">1</span>Install OAP
                  </p>
                  <p>
                    One command brings it up on a laptop or a Mac. The guide
                    lists what you need first.
                  </p>
                  <a
                    className="lp-btn lp-btn--primary"
                    href="/docs/install-overview"
                  >
                    Install OAP
                  </a>
                </li>
                <li>
                  <p className="try-step-h">
                    <span aria-hidden="true">2</span>Install messagingbot
                  </p>
                  <div className="term">
                    <CopyButton text={INSTALL_TEXT} />
                    {INSTALL.map((l, i) =>
                      l.note ? (
                        <span className="term-line term-note" key={i}>
                          {l.note}
                        </span>
                      ) : (
                        <span className="term-line" key={i}>
                          <span className="p">$ </span>
                          {l.cmd}
                        </span>
                      ),
                    )}
                  </div>
                  <p className="try-note">
                    The repository allowlist blocks from the first call.
                    OAP&apos;s per-person permission checks start by logging
                    what they would deny, and block once you switch them to
                    enforcing.
                  </p>
                  <a className="try-more" href={BUNDLE}>
                    View the bundle on GitHub{" "}
                    <span aria-hidden="true">&rarr;</span>
                  </a>
                </li>
              </ol>
            </div>
          </div>
        </section>
      </main>

      <SiteFooter />

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
  title,
  icon,
  children,
}: {
  title: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="why-item">
      {icon}
      <div>
        <h3>{title}</h3>
        <p>{children}</p>
      </div>
    </div>
  );
}
