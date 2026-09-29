"use client";
import {
  useCallback,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { OapMark } from "./OapMark";
import { ThemeToggle } from "@/components/ThemeToggle";
import { OWASP } from "./owasp";

/* Every outbound URL the page needs that does not yet exist. Kept in one object
 * so filling them in is a single edit. Anything pointing at `/docs/<slug>` is
 * a real guide on this site and is written inline. */
const TODO = {
  repo: "https://example.invalid/PLACEHOLDER-repo",
  discuss: "https://example.invalid/PLACEHOLDER-community",
  owaspFull: "https://example.invalid/PLACEHOLDER-owasp-coverage",
};

/* The page's copy follows the repository README: its order, its emphasis and
 * its claims. When the README's story changes, change this page to match. */

/* ------------------------------------------------------------------ data --- */

interface Step {
  cmd: string;
  note?: string;
}

const INSTALL: {
  id: string;
  label: string;
  steps: Step[];
  comment: string[];
}[] = [
  {
    id: "desktop",
    label: "Desktop · macOS",
    steps: [
      { cmd: "mage desktop:all" },
      { cmd: "open build/desktop/out/oap.app" },
    ],
    comment: [
      "# runs OAP in a lightweight Linux VM reachable only from",
      "# your Mac, with a menubar app for chats, sessions, the",
      "# admin dashboard and agent installation.",
    ],
  },
  {
    id: "kubernetes",
    label: "Kubernetes · kind",
    steps: [
      { cmd: "mage build:oap" },
      { cmd: "kind create cluster --name oap-dev" },
      { cmd: "./bin/oap init --local --pinning-mode=warn --wizard" },
    ],
    comment: [
      "# builds and loads images, installs OAP, waits for it to",
      "# come up healthy, and walks you through security",
      "# settings and model configuration.",
    ],
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
    "Cannot exceed the approved plan",
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
    body: "An AgentClass is a reviewable template you keep in git. An AgentSession is a disposable instance with its own runner pod, budget, memory scope and signing key, torn down when the work is done.",
    chips: ["AgentClass", "AgentSession", "one pod per session"],
    href: "/docs/agent-definition",
  },
  {
    verb: "Constrain",
    title: "Safe tools",
    body: 'A tool is a declarative contract, validated when it is authored and again when it runs. Calls execute as argv arrays in a hardened sandbox pod, never through a shell, so "what can this agent do" has an enumerable answer.',
    chips: ["argv, never a shell", "deny by default", "CEL constraints"],
    href: "/docs/safe-tools",
  },
  {
    verb: "Broker",
    title: "Identity & credentials",
    body: 'Credentials live in named catalogs and resolve at the moment of execution. The agent is handed a valet key, not the keyring, and "acting as Alice" is a governed mode rather than a shared secret.',
    chips: [
      "named catalogs",
      "just-in-time resolution",
      "per-user credentials",
    ],
    href: "/docs/identity",
  },
  {
    verb: "Decide",
    title: "Authorization",
    body: "One relationship graph answers every state-touching action: may this subject do this, to this resource, right now. The graph mirrors ownership in your organization, and every path fails closed.",
    chips: ["SpiceDB", "per subject, per action", "fails closed"],
    href: "/docs/authorization",
  },
  {
    verb: "Carry",
    title: "Channels & continuity",
    body: "A Channel binds a transport to an AgentClass. Threads survive across days, and approvals render in the channel where the work is happening, so nobody has to leave the conversation to grant or deny an action.",
    chips: [
      "Slack · browser · CLI · GitHub",
      "threads persist",
      "signed webhooks",
    ],
    href: "/docs/channels",
  },
  {
    verb: "Remember",
    title: "Memory & knowledge",
    body: "Typed memory with structured recall, ranked search and a knowledge graph. Recall is an authorization decision rather than a database query, and the transcript is a signed ledger rather than a mutable table.",
    chips: ["typed kinds", "ranked search", "append-only audit"],
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
        "What is checked.",
        "Tool calls, interactions, memory access, lookups and preferences all go through a relationship graph that mirrors ownership in your organization.",
      ],
      [
        "Operation-level rules.",
        "One agent can be read-only for one team and read-write for another, instead of a separate agent per audience.",
      ],
      [
        "Directory sync.",
        "Group memberships from Slack, GitHub and 1Password sync into the graph, so permissions follow your organization.",
      ],
      [
        "Multiplayer sessions.",
        "Joining a session, directing it and running sensitive tools can each require approval, and a requester cannot approve their own request.",
      ],
    ],
    links: [
      ["Authorization", "/docs/authorization"],
      ["Identity modes", "/docs/identity-modes"],
      ["Multiplayer", "/docs/multiplayer-sessions"],
    ],
  },
  {
    title: "Injected instructions cannot widen what an agent may do",
    points: [
      [
        "Plan gating.",
        "The agent states what it intends, a person approves that scope, and every later action is checked against it. Injected text can change what the model wants to do, not what it is allowed to do.",
      ],
      [
        "Slots.",
        "A session commits to the resource it is working on, once. It cannot drift to another resource even when its credentials would reach one.",
      ],
      [
        "Approvals the model cannot reword.",
        "The platform describes the action from the call itself, and the approval is bound to those exact arguments.",
      ],
      [
        "Tool specs.",
        "Declare which operations and resources an MCP server or CLI exposes. A read-write integration becomes read-only without a read-only token.",
      ],
    ],
    links: [
      ["Plan gating", "/docs/plan-gating"],
      ["Tool specs", "/docs/toolspec-validation"],
    ],
  },
  {
    title: "Data reaches only its authorized audience",
    points: [
      [
        "Leakage tracking.",
        "Tool responses are tagged with their origin, and the platform checks those tags against who will receive the data before anything leaves.",
      ],
      [
        "Slot-scoped memory.",
        "Sessions share memory only when bound to the same resource, so one customer's context does not reach another's. Nothing is shared by default.",
      ],
      [
        "Secret scrubbing.",
        "Mark a tool output as a secret and the model receives an opaque handle. The real value is substituted outside the model.",
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
        "Every toolkit, tool spec and capability was added deliberately, so a review covers what was added rather than what was left on.",
      ],
      [
        "A sandbox per tool.",
        "Each tool holds only its own credentials. A compromised tool cannot borrow another's access, and the orchestrating agent never holds them at all.",
      ],
      [
        "Sub-agents request their own permissions.",
        "Delegation narrows access instead of copying it.",
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
        "Inherited requirements.",
        "Administrators set defaults at the cluster, namespace or agent-class level, and agents cannot opt out.",
      ],
      [
        "Revocation.",
        "Remove a relationship and it takes effect everywhere at once, with no credential rotation or redeploy.",
      ],
      [
        "A verifiable audit log.",
        <>
          Every entry is signed and hash-chained, so modification, reordering
          and truncation are detectable. <code>oap audit verify</code> checks a
          session offline.
        </>,
      ],
      [
        "Pinning, budgets and hooks.",
        "Skills, containers and MCP servers pin to approved versions; budgets cap turns, spend and run time; your own hooks can trip a circuit breaker.",
      ],
    ],
    links: [
      ["Audit log", "/docs/audit-log"],
      ["Revocation", "/docs/revocation"],
      ["Pinning", "/docs/supply-chain-pinning"],
    ],
  },
  {
    title: "Isolation in the platform itself",
    points: [
      [
        "A sanitizer per content type.",
        "Each is written for that type's risks, and rendered output is contained under a Content Security Policy without cookie access.",
      ],
      [
        "Separate pods.",
        "The session runner, the operator and each sanitizer run apart, with micro-VM backing where the cluster provides it.",
      ],
      [
        "Scoped bus credentials.",
        "Each component dials the control-plane bus with its own credential, granting only its own session's subjects.",
      ],
      [
        "One package per agent.",
        "Each agent ships as one OCI-compliant package, so installation can be gated and reviewed like any other artifact.",
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
    title: "Say what the agent is for",
    body: "Agent Builder identifies the tools it needs, connects accounts through the platform, and defines what the new agent may do and what it must ask permission for.",
  },
  {
    step: "Test",
    title: "Try it in a workshop",
    body: "Each build runs in an isolated workshop. The draft cannot touch your live agents, and the builder cannot install it into the real environment.",
  },
  {
    step: "Deliver",
    title: "Hand over a bundle",
    body: "When you are satisfied you get a portable .oap bundle, and can submit an installation request for an administrator to review.",
  },
];

const EVERYTHING = [
  {
    title: "You choose the model",
    body: "Anthropic, OpenAI or OpenRouter, swappable per deployment. Nothing in an agent's definition hardcodes a provider.",
  },
  {
    title: "You choose the infrastructure",
    body: "A laptop, a local cluster, or your own Kubernetes: GKE, EKS, AKS or self-managed.",
  },
  {
    title: "Channels",
    body: "Slack, the browser, the CLI, GitHub, or a signed webhook trigger from another system.",
  },
  {
    title: "Memory and knowledge graph",
    body: "Structured recall, ranked search and graph-native queries over what an agent has learned.",
  },
  {
    title: "Built in and swappable",
    body: "The runner, authorization, approvals, sandboxing, credentials, memory and audit all ship built in, and each can be replaced by something your organization already runs.",
  },
  {
    title: "Nothing phones home",
    body: "The runtime's outbound calls are the ones you configure: your model provider, the MCP servers you declared, the hosts your tools reach.",
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

function SectionHead({
  n,
  kicker,
  children,
}: {
  n: string;
  kicker: string;
  children: ReactNode;
}) {
  return (
    <>
      <div className="lp-ghost" aria-hidden="true">
        {n}
      </div>
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
        <a className="lp-nav-brand" href="/">
          <OapMark />
          <span>Open Agent Primitives</span>
        </a>
        <div className="lp-nav-links">
          <a href="/docs/what-is-oap">Docs</a>
          <a href="#secure">Security</a>
          <a href="#agent-builder">Agent Builder</a>
          <a href={TODO.repo}>GitHub</a>
          <ThemeToggle />
          <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
            Get started
          </a>
        </div>
      </nav>

      {/* ------------------------------------------------------------ hero --- */}
      <header className="lp-hero">
        <div>
          <OapMark className="lp-hero-mark" />
          <h1>
            A secure way to run <em>enterprise AI agents</em>.
          </h1>
          <p className="lp-hero-sub">
            Open Agent Primitives is a set of building blocks for constructing
            and running enterprise agents, in your own Kubernetes cluster, on
            the models and infrastructure you choose.{" "}
            <strong>
              Every control sits outside the model, so the platform decides
              before the call.
            </strong>
          </p>
          <p className="lp-hero-qs-head">
            Trusting an agent with production work means answering four
            questions:
          </p>
          <ol className="lp-hero-qs">
            {QUESTIONS.map((q) => (
              <li key={q}>{q}</li>
            ))}
          </ol>
          <div className="lp-hero-cta">
            <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
              Get started
            </a>
            <a className="lp-btn lp-btn--ghost" href="#secure">
              How it stays secure
            </a>
          </div>
        </div>
        <div className="lp-hero-aside">
          <InstallTerminal />
          <ul className="lp-chips lp-hero-chips">
            <li>6 primitives</li>
            <li>27 security controls</li>
            <li>your cluster, your models</li>
            <li>Apache 2.0</li>
          </ul>
          <p className="lp-note">
            OAP builds from source today, from a clone of the repository. There
            is no published binary or container image yet.
          </p>
        </div>
      </header>

      {/* ---------------------------------------------------- 01 problem --- */}
      <section className="lp-section" id="problem">
        <SectionHead n="01" kicker="The problem">
          A credential is almost always <em>broader</em> than the task.
        </SectionHead>
        <p className="lp-lede">
          A token that reaches one git repository normally reaches every
          repository, and an integration built to expose a service exposes all
          of it. An agent inherits that whole surface, and the gap between what
          a task needs and what its credentials permit grows as agents take on
          more work.
        </p>
        <p className="lp-lede">
          A model cannot reliably separate instructions from data, so its
          judgment is not a dependable place to enforce a boundary, and a
          hardened prompt is still an instruction rather than an enforcement
          point. A permission check at the edge decides who may start an agent,
          not what it reaches once it is running.{" "}
          <strong>
            Every control in OAP therefore sits outside the model, and no
            decision depends on the agent&rsquo;s cooperation.
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

        <div className="lp-decide">
          <div>
            <div className="lp-panel-head">
              <span>Before the call: anatomy of one check</span>
            </div>
            <div className="lp-panel-body">
              <table className="lp-fields">
                <tbody>
                  <tr>
                    <th scope="row">subject</th>
                    <td>
                      user:alice@acme.example
                      <small>
                        the human who spoke, canonicalized at the channel
                        boundary
                      </small>
                    </td>
                  </tr>
                  <tr>
                    <th scope="row">permission</th>
                    <td>
                      push
                      <small>
                        declared by the toolkit, not chosen at call time
                      </small>
                    </td>
                  </tr>
                  <tr>
                    <th scope="row">resource</th>
                    <td>
                      git_repo:acme/widget
                      <small>
                        derived from the call&rsquo;s own arguments by a CEL
                        expression
                      </small>
                    </td>
                  </tr>
                  <tr className="lp-verdict">
                    <th scope="row">decision</th>
                    <td>
                      <span className="lp-deny">DENIED</span>
                      <small>
                        at a fixed position in the pre-tool-call pipeline. The
                        call never reaches the sandbox or the MCP server.
                      </small>
                    </td>
                  </tr>
                </tbody>
              </table>
              <p className="lp-caption">
                An unresolvable argument denies rather than guesses. In the git
                toolkit, a shorthand remote like <code>origin</code> yields an
                empty resource ID on purpose, so a push fails closed instead of
                authorizing against an unnamed repository.
              </p>
            </div>
          </div>
          <div>
            <div className="lp-panel-head">
              <span>Recorded, from a replayed session</span>
            </div>
            <div className="lp-panel-body">
              <pre className="lp-trace">
                {
                  "authz git_repo:https=3A//github=2Ecom/testorg/testreview#fetch "
                }
                <span className="d">denied</span>
                {"\n"}
                {"authz git_repo:workspace#read  "}
                <span className="d">denied</span>
                {"\n"}
                {"authz git_repo:workspace#write "}
                <span className="d">denied</span>
              </pre>
              <p className="lp-caption">
                Three lines of a golden authorization trace from the end-to-end
                suite. The trace is frozen per scenario, so a gate that stops
                refusing shows up as a diff in review rather than as an
                incident.
              </p>
            </div>
          </div>
        </div>

        <p className="lp-note">
          Precisely: every <em>state-touching</em> tool call. Tools a toolkit
          author declares stateless or passthrough resolve without a graph
          check, and that declaration is reviewable in the same YAML as the
          tool.
        </p>
      </section>

      {/* ------------------------------------------------- 02 primitives --- */}
      <section className="lp-section" id="primitives">
        <SectionHead n="02" kicker="The primitives">
          Six concerns every agent has to solve.
        </SectionHead>
        <p className="lp-lede">
          A primitive is a concern every agent has to solve, whatever it does.
          OAP names six, ships a working implementation of each, and composes
          every agent from them.
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
              <ul className="lp-chips">
                {p.chips.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </article>
          ))}
        </div>
      </section>

      {/* ---------------------------------------------------- 03 secure --- */}
      <section className="lp-section" id="secure">
        <SectionHead n="03" kicker="What makes it secure">
          Twenty-seven controls, in six areas.
        </SectionHead>
        <p className="lp-lede">
          Each control is a property of the platform rather than an instruction
          to the model, so no phrasing, context length or tool output changes
          the answer. The security model documents every one and names the
          package that implements it.
        </p>
        <ol className="lp-slab lp-slab--2 lp-areas">
          {AREAS.map((a) => (
            <li key={a.title}>
              <h3>{a.title}</h3>
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
          Some controls are opt-in per agent class, including plan gating and
          the information-leakage gate. Hostname-level egress is recorded on
          session status but enforced only by a DNS-aware CNI. Network policy at
          layers 3 and 4 is on by default, and the sandbox fails closed to
          deny-all when a class says nothing.
        </p>
      </section>

      {/* --------------------------------------------- 04 agent builder --- */}
      <section className="lp-section" id="agent-builder">
        <SectionHead n="04" kicker="Agent Builder">
          Build an agent by talking to an agent.
        </SectionHead>
        <p className="lp-lede">
          You don&rsquo;t have to start with manifest files. Agent Builder is an
          OAP agent whose job is to create other agents. Describe what you need
          in plain language, then test the result live.
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
        <div className="lp-hero-cta lp-section-cta">
          <a className="lp-btn lp-btn--ghost" href="/docs/agent-builder">
            Read the Agent Builder guide
          </a>
        </div>
        <p className="lp-note">
          <code>oap init</code> leaves Agent Builder off, because there is no
          safe default for who may start it; you enable it by naming the people
          or groups allowed to. It is subject to every control above: broad
          freedom inside its workshop, and no authority outside it.
        </p>
      </section>

      {/* -------------------------------------------- 05 everything else --- */}
      <section className="lp-section" id="everything-else">
        <SectionHead n="05" kicker="Everything else">
          The rest of the platform, included.
        </SectionHead>
        <p className="lp-lede">
          These capabilities are common to agent platforms. OAP includes them,
          and they are listed here so the set is complete.
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
        <SectionHead n="06" kicker="Security posture">
          The OWASP Agentic Top 10, including where we <em>fall short</em>.
          <span className="lp-draft">Draft</span>
        </SectionHead>
        <p className="lp-lede">
          We keep a coverage map of OAP against the OWASP Top 10 for Agentic
          Applications, and we publish the gaps column alongside the ratings. It
          is an advisory self-assessment for orientation, not a certification,
          an audit or a penetration test. It says a control exists and is wired
          into the relevant path. It does not say the control is free of bugs.
        </p>
        <div className="lp-owasp-wrap">
          <table className="lp-owasp">
            <caption>4 substantial · 5 partial · 1 architectural N/A</caption>
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
          <a className="lp-btn lp-btn--ghost" href={TODO.owaspFull}>
            Full assessment
          </a>
        </div>
        <div className="lp-evidence">
          <p className="lp-col-head">How we know the gates still hold</p>
          <p>
            Three test suites gate every merge. Scripted whole-session scenarios
            assert on the tool result rather than the reply, golden
            authorization traces turn any change in what was checked into a diff
            someone has to read, and replays of captured real sessions show a
            real model produced the interaction at least once. None of them pins
            what a model will do next.
          </p>
        </div>
        <p className="lp-note">
          OWASP materials are referenced under CC BY-SA 4.0. This assessment is
          not affiliated with or endorsed by OWASP. Coverage levels are a
          qualitative judgement of breadth, not a score.
        </p>
      </section>

      {/* ----------------------------------------------------------- close --- */}
      <section className="lp-close">
        <p className="lp-kicker">Defense in depth</p>
        <h2 className="lp-h2">Every layer assumes the others may fail.</h2>
        <p className="lp-lede">
          To misuse an OAP agent, an attacker has to get past a plan a human
          approved, a slot that cannot be reopened, a tool lens that cannot be
          widened, an authorization check on every call, and a sandbox that
          never held the credential in the first place. None of them is
          sufficient alone.
        </p>
        <div className="lp-hero-cta">
          <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
            Get started
          </a>
          <a className="lp-btn lp-btn--ghost" href="/docs/what-is-oap">
            Read the docs
          </a>
          <a className="lp-btn lp-btn--ghost" href={TODO.repo}>
            Source
          </a>
        </div>
      </section>

      <footer className="lp-footer">
        <OapMark className="lp-footer-mark" />
        <a href="/docs/what-is-oap">Docs</a>
        <a href="/docs/crd-reference">CRD reference</a>
        <a href="/docs/cli-reference">CLI reference</a>
        <a href="#owasp">OWASP coverage</a>
        <a href={TODO.discuss}>Community</a>
        <span>
          Built by <a href="https://authzed.com">AuthZed</a>, using{" "}
          <a href="https://github.com/authzed/spicedb">SpiceDB</a>.
        </span>
      </footer>
    </div>
  );
}
