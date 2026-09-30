"use client";
import {
  useCallback,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { Wordmark } from "@/components/Wordmark";
import { SiteFooter } from "@/components/SiteFooter";
import { DiscordIcon, GitHubIcon } from "@/components/BrandIcons";
import { DISCORD_URL } from "@/lib/site";
import { getAsset } from "@/lib/manifest";
import { OWASP, coverageSummary } from "./owasp";

const REPO = "https://github.com/authzed/openagentprimitives";

/* The page's copy follows the repository README: its order, its emphasis and
 * its claims. When the README's story changes, change this page to match. */

/* ------------------------------------------------------------------ data --- */

interface Step {
  cmd: string;
}

const INSTALL: {
  id: string;
  label: string;
  steps: Step[];
  comment: string[];
}[] = [
  {
    id: "desktop",
    label: "macOS desktop",
    steps: [
      { cmd: "mage desktop:all" },
      { cmd: "open build/desktop/out/oap.app" },
    ],
    comment: ["# a local VM with a menubar app. No Kubernetes needed."],
  },
  {
    id: "kubernetes",
    label: "Local Kubernetes",
    steps: [
      { cmd: "mage build:oap" },
      { cmd: "kind create cluster --name oap-dev" },
      { cmd: "./bin/oap init --local --pinning-mode=warn --wizard" },
    ],
    comment: ["# builds, installs, and walks you through setup."],
  },
];

const QUESTIONS = [
  "What can it reach?",
  "Who granted that access?",
  "What happens when a tool result tells it to do something else?",
  "What is exposed if one component is compromised?",
];

const COMPARE: [string, string, string][] = [
  [
    "What an agent can reach",
    "Whatever its credentials allow",
    "Exactly what you granted",
  ],
  [
    "Who decides an action is allowed",
    "The model, in the moment",
    "The platform, before the call",
  ],
  [
    "An injected instruction mid-session",
    "Can redirect the agent",
    "Cannot widen what it's authorized to do",
  ],
  [
    "Tool credentials",
    "Shared across tools in one sandbox",
    "Held only by the tool that uses them",
  ],
  [
    "Restricting an MCP server",
    "Needs a narrow upstream token",
    "Declared by you, enforced per call",
  ],
  [
    "Revoking access",
    "Rotate credentials, redeploy",
    "One permission graph call",
  ],
  [
    "The audit log",
    "Append-only, enforced by the store",
    "Signed, chained, verifiable offline",
  ],
];

const PRIMITIVES = [
  {
    verb: "Define",
    title: "Agent definition",
    body: "An AgentClass is a reviewable template kept in git. Each AgentSession gets its own pod, budget and memory, and is torn down when the work is done.",
    href: "/docs/agent-definition",
  },
  {
    verb: "Constrain",
    title: "Safe tools",
    body: "A tool is a declarative contract, checked when it is written and again when it runs. Calls execute in a hardened sandbox, never through a shell.",
    href: "/docs/safe-tools",
  },
  {
    verb: "Broker",
    title: "Identity & credentials",
    body: "Credentials resolve at the moment of use, and the agent never holds them.",
    href: "/docs/identity",
  },
  {
    verb: "Decide",
    title: "Authorization",
    body: "One relationship graph answers every action: may this person do this, to this resource, right now. Every path fails closed.",
    href: "/docs/authorization",
  },
  {
    verb: "Carry",
    title: "Channels & continuity",
    body: "Agents work in Slack, the browser, the CLI and GitHub. Threads last for days, and approvals appear where the conversation already is.",
    href: "/docs/channels",
  },
  {
    verb: "Remember",
    title: "Memory & knowledge",
    body: "Recall, search and a knowledge graph. Reading memory is an authorization decision, and the transcript is a signed ledger.",
    href: "/docs/memory",
  },
];

interface Area {
  title: string;
  points: [string, ReactNode][];
  links: [string, string][];
}

const AREAS: Area[] = [
  {
    title: "Every action is authorized",
    points: [
      [
        "Checked per action.",
        "Tool calls, memory, lookups and interactions all go through one permission graph.",
      ],
      [
        "Operation-level rules.",
        "One agent can be read-only for one team and read-write for another.",
      ],
      [
        "Directory sync.",
        "Groups from Slack, GitHub and 1Password keep permissions in step with your organization.",
      ],
      [
        "Multiplayer sessions.",
        "Joining, directing and sensitive tools can each require approval.",
      ],
    ],
    links: [
      ["Authorization", "/docs/authorization"],
      ["Identity modes", "/docs/identity-modes"],
      ["Multiplayer", "/docs/multiplayer-sessions"],
    ],
  },
  {
    title: "Injected instructions can't widen access",
    points: [
      [
        "Plan gating.",
        "A person approves the agent's plan, and every later action is checked against it.",
      ],
      [
        "Slots.",
        "A session commits to one resource and cannot drift to another.",
      ],
      [
        "Approvals the model can't reword.",
        "The platform describes the action from the call itself.",
      ],
      [
        "Tool specs.",
        "Make a read-write integration read-only without a read-only token.",
      ],
    ],
    links: [
      ["Plan gating", "/docs/plan-gating"],
      ["Tool specs", "/docs/toolspec-validation"],
    ],
  },
  {
    title: "Data reaches only its audience",
    points: [
      [
        "Leakage tracking.",
        "Data is tagged with its origin and checked against who will receive it.",
      ],
      [
        "Slot-scoped memory.",
        "One customer's context never reaches another customer's session.",
      ],
      [
        "Secret scrubbing.",
        "The model sees an opaque handle in place of the value.",
      ],
    ],
    links: [
      ["Information leakage", "/docs/information-leakage"],
      ["Memory authorization", "/docs/memory-authorization"],
    ],
  },
  {
    title: "Least privilege by default",
    points: [
      [
        "Opt-in capabilities.",
        "No tool or capability is granted until someone adds it.",
      ],
      [
        "A sandbox per tool.",
        "Each tool holds only its own credentials, and the agent holds none.",
      ],
      [
        "Narrowing delegation.",
        "Sub-agents request their own permissions instead of inheriting yours.",
      ],
    ],
    links: [
      ["Sandboxed execution", "/docs/sandboxed-execution"],
      ["Sub-agents", "/docs/subagents-delegation"],
    ],
  },
  {
    title: "Control and oversight",
    points: [
      [
        "Inherited policy.",
        "Cluster, namespace and class defaults that agents can't opt out of.",
      ],
      [
        "Revocation.",
        "Remove one relationship and the next action is refused, with no credential rotation or redeploy.",
      ],
      [
        "Verifiable audit.",
        <>
          Signed, hash-chained entries, checked offline with{" "}
          <code>oap audit verify</code>.
        </>,
      ],
      [
        "Pinning and budgets.",
        "Approved versions, capped spend and circuit breakers.",
      ],
    ],
    links: [
      ["Audit log", "/docs/audit-log"],
      ["Revocation", "/docs/revocation"],
      ["Pinning", "/docs/supply-chain-pinning"],
    ],
  },
  {
    title: "Isolation in the platform",
    points: [
      [
        "Sanitized output.",
        "Each content type has its own sanitizer, and rendered output runs under a strict CSP.",
      ],
      [
        "Separate pods.",
        "Runner, operator and sanitizers run apart, on micro-VMs where available.",
      ],
      [
        "Scoped credentials.",
        "Each component reaches only its own session on the control-plane bus.",
      ],
      [
        "Reviewable packages.",
        "Each agent ships as one OCI package you can gate like any artifact.",
      ],
    ],
    links: [
      ["Artifact safety", "/docs/artifact-safety"],
      ["Packaging", "/docs/packaging-oap"],
    ],
  },
];

const BUILDER = [
  {
    step: "Describe",
    title: "Say what it's for",
    body: "Agent Builder picks the tools, connects accounts, and sets what the new agent may do.",
  },
  {
    step: "Test",
    title: "Try it in a workshop",
    body: "Drafts run in isolation and can't touch your live agents.",
  },
  {
    step: "Deliver",
    title: "Hand over a bundle",
    body: "You get a portable .oap bundle for an administrator to review and install.",
  },
];

const EVERYTHING = [
  {
    title: "Your choice of model",
    body: "Anthropic, OpenAI or OpenRouter, swappable per deployment.",
  },
  {
    title: "Your infrastructure",
    body: "A laptop, a local cluster, GKE, EKS, AKS or self-managed Kubernetes.",
  },
  {
    title: "Channels",
    body: "Slack, the browser, the CLI and GitHub, plus signed webhooks and scheduled runs. Transports are pluggable, so you can add your own.",
  },
  {
    title: "Memory and knowledge graph",
    body: "Structured recall, ranked search and graph queries.",
  },
  {
    title: "Built in and swappable",
    body: "The runner, authorization, approvals, sandboxing, credentials, memory and audit ship built in, and each can be swapped for one you already run.",
  },
  {
    title: "Under your control",
    body: "OAP runs inside a cluster you operate. Your team owns its network, access and upgrades.",
  },
];

const COV_LABEL: Record<string, string> = {
  substantial: "Substantial",
  partial: "Partial",
  na: "Architectural N/A",
};

/* ------------------------------------------------------------ components --- */

function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  const copy = useCallback(() => {
    void navigator.clipboard?.writeText(text).then(() => {
      setDone(true);
      window.setTimeout(() => setDone(false), 1600);
    });
  }, [text]);
  return (
    <button type="button" className="lp-copy-btn" onClick={copy}>
      {done ? "copied" : "copy"}
    </button>
  );
}

/* A two-tab terminal: Desktop is the README's fastest path, kind the one for
 * development. Arrow keys move between tabs, per the ARIA tabs pattern. */
function InstallTerminal() {
  const [active, setActive] = useState(0);
  const tabs = useRef<(HTMLButtonElement | null)[]>([]);
  const current = INSTALL[active];

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    e.preventDefault();
    const next =
      (active + (e.key === "ArrowRight" ? 1 : -1) + INSTALL.length) %
      INSTALL.length;
    setActive(next);
    tabs.current[next]?.focus();
  };

  return (
    <div className="lp-term">
      <div className="lp-term-bar">
        <div
          className="lp-term-tabs"
          role="tablist"
          aria-label="Install path"
          onKeyDown={onKeyDown}
        >
          {INSTALL.map((t, i) => (
            <button
              key={t.id}
              ref={(el) => {
                tabs.current[i] = el;
              }}
              type="button"
              role="tab"
              id={`lp-tab-${t.id}`}
              aria-selected={i === active}
              aria-controls={`lp-panel-${t.id}`}
              tabIndex={i === active ? 0 : -1}
              className="lp-term-tab"
              onClick={() => setActive(i)}
            >
              {t.label}
            </button>
          ))}
        </div>
        <CopyButton text={current.steps.map((s) => s.cmd).join("\n")} />
      </div>
      <pre
        role="tabpanel"
        id={`lp-panel-${current.id}`}
        aria-labelledby={`lp-tab-${current.id}`}
      >
        {current.steps.map((s) => (
          <span key={s.cmd}>
            <span className="p">$ </span>
            {s.cmd}
            {"\n"}
          </span>
        ))}
        {current.comment.map((line) => (
          <span key={line}>
            <span className="c">{line}</span>
            {"\n"}
          </span>
        ))}
      </pre>
    </div>
  );
}

/* A narrated demo clip, resolved from the docs media manifest. The narration
 * is the point, so there is no autoplay: it starts on a click, unmuted, from
 * its poster frame. */
function DemoClip({ name, caption }: { name: string; caption: ReactNode }) {
  const asset = getAsset(name);
  if (!asset || (!asset.webm && !asset.mp4)) return null;
  return (
    <figure className="lp-clip">
      <video controls playsInline preload="metadata" poster={asset.poster}>
        {asset.webm && <source src={asset.webm} type="video/webm" />}
        {asset.mp4 && <source src={asset.mp4} type="video/mp4" />}
      </video>
      <figcaption>{caption}</figcaption>
    </figure>
  );
}

function SectionHead({
  kicker,
  children,
}: {
  kicker: string;
  children: ReactNode;
}) {
  return (
    <>
      <p className="lp-kicker">{kicker}</p>
      <h2 className="lp-h2">{children}</h2>
    </>
  );
}

/* ------------------------------------------------------------------ page --- */

export function Landing() {
  return (
    <div className="lp">
      <nav className="lp-nav">
        <a
          className="lp-nav-brand"
          href="/"
          aria-label="Open Agent Primitives home"
        >
          <Wordmark className="lp-nav-wordmark" />
        </a>
        <div className="lp-nav-links">
          <a className="lp-nav-keep" href="/docs/what-is-oap">
            Docs
          </a>
          <a href="#secure">Security</a>
          <a href="#agent-builder">Agent Builder</a>
          <a className="lp-nav-icon" href={REPO} aria-label="GitHub">
            <GitHubIcon />
          </a>
          <a className="lp-nav-icon" href={DISCORD_URL} aria-label="Discord">
            <DiscordIcon />
          </a>
          <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
            Get started
          </a>
        </div>
      </nav>

      {/* ------------------------------------------------------------ hero --- */}
      <main>
        <header className="lp-hero">
          <div>
            <h1>
              A secure way to run <em>enterprise AI agents</em>.
            </h1>
            <p className="lp-hero-sub">
              Building blocks for running enterprise agents in your own cluster,
              on the models you choose.{" "}
              <strong>
                The AI never decides what it&rsquo;s allowed to do. OAP checks
                every action before it runs.
              </strong>
            </p>
            <p className="lp-hero-qs-head">
              Trusting an agent means answering four questions:
            </p>
            <ol className="lp-hero-qs">
              {QUESTIONS.map((q) => (
                <li key={q}>{q}</li>
              ))}
            </ol>
            <p className="lp-hero-qs-foot">
              OAP answers each one in the platform, before the agent acts.
            </p>
            <div className="lp-hero-cta">
              <a className="lp-btn lp-btn--primary" href="#secure">
                How it stays secure
              </a>
            </div>
          </div>
          <div className="lp-hero-aside">
            <InstallTerminal />
            <DemoClip
              name="reviewbot-demo"
              caption={
                <>
                  Watch one: <a href="/docs/reviewbot">reviewbot</a> reviews a
                  pull request read-only and records a Check Run.
                </>
              }
            />
          </div>
        </header>

        {/* ---------------------------------------------------- 01 problem --- */}
        <section className="lp-section" id="problem">
          <SectionHead kicker="The problem">
            Your agent can reach <em>everything</em> its credentials can.
          </SectionHead>
          <p className="lp-lede">
            A token for one repository usually reaches every repository. An
            agent inherits all of it.
          </p>
          <p className="lp-lede">
            A model can&rsquo;t reliably tell instructions from data, so its
            judgment can&rsquo;t be the boundary. A better prompt is still just
            an instruction.
          </p>
          <p className="lp-lede">
            <strong>
              So OAP enforces every rule itself, before each action runs, where
              no prompt can change it.
            </strong>
          </p>

          <div className="lp-compare-wrap">
            <table className="lp-compare">
              <thead>
                <tr>
                  <th scope="col">
                    <span className="lp-sr">Question</span>
                  </th>
                  <th scope="col">Typical agent platform</th>
                  <th scope="col">OAP</th>
                </tr>
              </thead>
              <tbody>
                {COMPARE.map(([q, typical, oap]) => (
                  <tr key={q}>
                    <th scope="row">{q}</th>
                    <td className="lp-compare-typical">{typical}</td>
                    <td className="lp-compare-oap">{oap}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        {/* ------------------------------------------------- 02 primitives --- */}
        <section className="lp-section" id="primitives">
          <SectionHead kicker="The primitives">
            Six concerns every agent has to solve.
          </SectionHead>
          <p className="lp-lede">
            A primitive is a basic building block that agents rely on, whatever
            they do. OAP ships a working implementation of every one.
          </p>
          <div className="lp-prims">
            {PRIMITIVES.map((p, i) => (
              <article className="lp-prim" key={p.title}>
                <p className="lp-prim-eyebrow">
                  #{i + 1} {p.verb}
                </p>
                <h3>
                  <a href={p.href}>{p.title}</a>
                </h3>
                <p>{p.body}</p>
              </article>
            ))}
          </div>
        </section>

        {/* ---------------------------------------------------- 03 secure --- */}
        <section className="lp-section" id="secure">
          <SectionHead kicker="What makes it secure">
            Controls in six areas.
          </SectionHead>
          <p className="lp-lede">
            The platform enforces every control. No prompt or tool output can
            affect enforcement.
          </p>
          <ol className="lp-slab lp-slab--2 lp-areas">
            {AREAS.map((a) => (
              <li key={a.title}>
                <h3 className="lp-areas-title">{a.title}</h3>
                <ul className="lp-points">
                  {a.points.map(([lead, body]) => (
                    <li key={lead}>
                      <strong>{lead}</strong> {body}
                    </li>
                  ))}
                </ul>
                <p className="lp-area-links">
                  {a.links.map(([label, href]) => (
                    <a key={href} href={href}>
                      {label}
                    </a>
                  ))}
                </p>
              </li>
            ))}
          </ol>
          <p className="lp-note">
            Plan gating and leakage tracking are opt-in per agent class. Network
            policy is on by default.
          </p>
        </section>

        {/* --------------------------------------------- 04 agent builder --- */}
        <section className="lp-section" id="agent-builder">
          <SectionHead kicker="Agent Builder">
            Build an agent by talking to an agent.
          </SectionHead>
          <p className="lp-lede">
            Agent Builder is an OAP agent that creates other agents. Describe
            what you need in plain language, then test it live.
          </p>
          <div className="lp-slab lp-slab--3">
            {BUILDER.map((b) => (
              <div key={b.step}>
                <p className="lp-prim-eyebrow">{b.step}</p>
                <h3>{b.title}</h3>
                <p>{b.body}</p>
              </div>
            ))}
          </div>
          <DemoClip
            name="builder-first-agent"
            caption="One build end to end in the real web UI: describe the agent, answer its questions, approve the stage once, test it live, and keep the draft."
          />
          <div className="lp-hero-cta lp-section-cta">
            <a className="lp-btn lp-btn--ghost" href="/docs/agent-builder">
              Read the Agent Builder guide
            </a>
          </div>
          <p className="lp-note">
            Off by default. You turn it on by naming who may use it, and it
            follows every control above.
          </p>
        </section>

        {/* -------------------------------------------- 05 everything else --- */}
        <section className="lp-section" id="everything-else">
          <SectionHead kicker="Everything else">
            The rest of the platform, included.
          </SectionHead>
          <p className="lp-lede">
            The capabilities you&rsquo;d expect from any agent platform.
          </p>
          <div className="lp-slab lp-slab--3">
            {EVERYTHING.map((e) => (
              <div key={e.title}>
                <h3>{e.title}</h3>
                <p>{e.body}</p>
              </div>
            ))}
          </div>
        </section>

        {/* ------------------------------------------------------- 06 owasp --- */}
        <section className="lp-section" id="owasp">
          <SectionHead kicker="Security posture">
            Coverage of the OWASP{" "}
            <span className="lp-nowrap">Agentic Top 10.</span>
          </SectionHead>
          <p className="lp-lede">
            How OAP maps to each risk in the OWASP Top 10 for Agentic
            Applications, with the remaining gaps listed alongside. This is a
            self-assessment, not a certification.
          </p>
          <div className="lp-owasp-wrap">
            <table className="lp-owasp">
              <caption>{coverageSummary(OWASP)}</caption>
              <thead>
                <tr>
                  <th scope="col">Item</th>
                  <th scope="col">Risk</th>
                  <th scope="col">Coverage</th>
                  <th scope="col">Known gap</th>
                </tr>
              </thead>
              <tbody>
                {OWASP.map(([id, title, level, gap]) => (
                  <tr key={id}>
                    <td className="lp-id">
                      <a href={`/docs/owasp-top10#${id.toLowerCase()}`}>{id}</a>
                    </td>
                    <td className="lp-title">{title}</td>
                    <td>
                      <span className={`lp-cov lp-cov--${level}`}>
                        {COV_LABEL[level]}
                      </span>
                    </td>
                    <td className="lp-gap">{gap}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="lp-hero-cta lp-section-cta">
            <a className="lp-btn lp-btn--ghost" href="/docs/owasp-top10">
              Read the coverage map
            </a>
          </div>
          <div className="lp-evidence">
            <p className="lp-col-head">How we know the gates still hold</p>
            <p>
              Every merge runs whole-session scenarios, golden authorization
              traces and replays of real sessions. A permission check that stops
              working fails the build.
            </p>
          </div>
          <p className="lp-note">
            Based on the{" "}
            <a href="https://genai.owasp.org">
              OWASP Top 10 for Agentic Applications (2026)
            </a>{" "}
            by the OWASP GenAI Security Project, licensed under{" "}
            <a href="https://creativecommons.org/licenses/by-sa/4.0/">
              CC BY-SA 4.0
            </a>
            . This assessment is not affiliated with or endorsed by OWASP.
          </p>
        </section>

        {/* ----------------------------------------------------------- close --- */}
        <section className="lp-close">
          <p className="lp-kicker">Defense in depth</p>
          <h2 className="lp-h2">Every layer assumes the others may fail.</h2>
          <p className="lp-lede">
            To misuse an agent, an attacker has to get past an approved plan, a
            locked slot, a tool spec, a check on every call, and a sandbox that
            never held the credential.
          </p>
          <div className="lp-hero-cta">
            <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
              Get started
            </a>
            <a className="lp-btn lp-btn--ghost" href="/docs/what-is-oap">
              Read the docs
            </a>
            <a className="lp-btn lp-btn--ghost" href={REPO}>
              Source
            </a>
          </div>
        </section>
      </main>

      <SiteFooter />
    </div>
  );
}
