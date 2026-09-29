"use client";
import { useCallback, useState } from "react";
import { OapMark } from "./OapMark";
import { ThemeToggle } from "@/components/ThemeToggle";
import { OWASP } from "./owasp";

/* Every outbound URL the page needs that does not yet exist. Kept in one object
 * so filling them in is a single edit. Anything pointing at `/docs/<slug>` is
 * a real guide on this site and is written inline. */
const TODO = {
  repo: "https://example.invalid/PLACEHOLDER-repo",
  clone: "https://example.invalid/PLACEHOLDER-repo.git",
  discuss: "https://example.invalid/PLACEHOLDER-community",
  owaspFull: "https://example.invalid/PLACEHOLDER-owasp-coverage",
  paper: (slug: string) =>
    `https://example.invalid/PLACEHOLDER-whitepaper-${slug}`,
};

const INSTALL = `git clone ${TODO.clone}
cd agentprimitives && mage build
./bin/oap install`;

/* ------------------------------------------------------------------ data --- */

const PRIMITIVES = [
  {
    verb: "Define",
    title: "Agent definition",
    body: "An AgentClass is a reviewable template you keep in git. An AgentSession is a disposable instance: one runner pod, one budget, one memory scope, one signing key, torn down when the work is done.",
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
    title: "Identity and credentials",
    body: 'Credentials live in named catalogs and resolve to bytes at the moment of execution. The agent is handed a valet key, not the keyring, and "acting as Alice" is a governed mode rather than a shared secret.',
    chips: ["4 credential kinds", "7 setup flows", "JIT resolution"],
    href: "/docs/identity",
  },
  {
    verb: "Decide",
    title: "Authorization",
    body: "One SpiceDB graph answers every state-touching action: may this subject do this, to this resource, right now. The schema is 663 lines and 18 definitions, with 15 permissions on the session alone.",
    chips: ["SpiceDB", "per subject, per action", "fails closed"],
    href: "/docs/authorization",
  },
  {
    verb: "Carry",
    title: "Channels and continuity",
    body: "A Channel binds a transport to an AgentClass. Threads survive across days, approvals render in the channel where the work is happening, and the button re-checks standing at click time rather than at render time.",
    chips: ["7 transports", "threads persist", "47 interaction kinds"],
    href: "/docs/channels",
  },
  {
    verb: "Remember",
    title: "Memory and knowledge",
    body: "Forty-four typed memory kinds, twenty-seven of them append-only and Ed25519 signed. Recall is an authorization decision rather than a database query, and the transcript is a ledger rather than a mutable table.",
    chips: ["44 kinds", "27 append-only", "RRF search"],
    href: "/docs/memory",
  },
];

const CANNOT = [
  {
    title: "The model never votes on its own permissions",
    body: "The permission and resource type come from the tool’s declaration. The resource ID is computed from the call’s own validated arguments by a CEL program, not asserted by the model. The subject is the human who spoke, canonicalized at the channel boundary.",
  },
  {
    title: "A denied call never reaches anything",
    body: "The check runs at the dispatch site, at a fixed position in a hook pipeline whose order is owned by code rather than configuration. A refusal means no sandbox exec and no MCP request, so there is nothing to undo.",
  },
  {
    title: "An approval cannot be moved to a different target",
    body: "Approval is bound to the exact arguments by an HMAC-SHA256 under a per-session key the model never sees. Approve acme/widget and you approved acme/widget. A call against acme/other needs its own approval.",
  },
  {
    title: "A requester cannot approve their own request",
    body: "Who may approve is derived from who owns the resource, not from who can see the button. Session standing is neither required nor sufficient, so an agent’s owner cannot self-approve their way into someone else’s data.",
  },
  {
    title: "A revoked credential does not survive the next call",
    body: "Using a credential is itself a graph permission, re-checked fully consistent immediately before every upstream request. Revocation lands on the next action rather than the next pod restart.",
  },
  {
    title: "An audit entry cannot be quietly edited",
    body: "Twenty-seven memory kinds refuse a write that changes existing content. Each entry carries a signature, a sequence number and the previous entry’s hash, so modification, fabrication, gaps and reordering are all detectable.",
  },
];

const SEAMS = [
  {
    n: "7",
    name: "Channel transports",
    list: "slack · github · browser · local · agent · bento · fake",
  },
  { n: "3", name: "LLM providers", list: "anthropic · openai · openrouter" },
  { n: "44", name: "Memory kinds", list: "27 of them append-only and signed" },
  {
    n: "6",
    name: "Artifact renderers",
    list: "html · css · svg · image · mcpui · oap",
  },
  {
    n: "7",
    name: "Identity setup flows",
    list: "oauth · pat · kubeconfig · scim · authkey · …",
  },
  { n: "5", name: "Pinning kinds", list: "mcp · image · cli · skill · oap" },
  {
    n: "6",
    name: "Cluster kinds",
    list: "gke · eks · aks · default · local · desktop",
  },
  {
    n: "4",
    name: "Search providers",
    list: "postgres · sqlite · graphiti · inmem",
  },
  { n: "3", name: "Memory backends", list: "postgres · sqlite · inmem" },
  {
    n: "3",
    name: "Revocation kinds",
    list: "credential · tool-origin · session-hold",
  },
  { n: "2", name: "Sandbox backends", list: "pod · agent-sandbox" },
  { n: "2", name: "Content guards", list: "prompt-injection · url-allowlist" },
];

const COV_LABEL: Record<string, string> = {
  substantial: "Substantial",
  partial: "Partial",
  na: "Architectural N/A",
};

const PAPERS = [
  {
    slug: "agent-definition",
    title: "Agent definition",
    blurb:
      "Why an agent should be a reviewable object in a cluster rather than a process someone started, and what a session boundary buys you.",
  },
  {
    slug: "safe-tools",
    title: "Safe tools",
    blurb:
      "Tools as declarative contracts: dual validation, argv over shell, and what it takes to make an undeclared capability structurally uncallable.",
  },
  {
    slug: "identity",
    title: "Identity and credentials",
    blurb:
      "Named catalogs, just-in-time resolution, and the difference between an agent acting for a person and an agent holding their keys.",
  },
  {
    slug: "authorization",
    title: "Authorization",
    blurb:
      "The relationship graph, where the check sits in the turn loop, how approvals bind to arguments, and every path that fails closed.",
  },
  {
    slug: "channels",
    title: "Channels and continuity",
    blurb:
      "Threads that outlive a process, approvals rendered where the work is, and what continuity means when the participants change.",
  },
  {
    slug: "memory",
    title: "Memory and knowledge",
    blurb:
      "Typed, authorized recall; ranked retrieval across providers; and an append-only ledger whose trust root the cluster witnesses.",
  },
];

/* ------------------------------------------------------------- component --- */

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
          <a href="#papers">Whitepapers</a>
          <a href="#owasp">Security</a>
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
            The agent proposes.
            <br />
            <em>SpiceDB</em> decides.
          </h1>
          <p className="lp-hero-sub">
            Open Agent Primitives is a Kubernetes-native runtime for LLM agents.
            Every state-touching tool call resolves to a subject, a permission
            and a resource, and is checked against a SpiceDB relationship graph
            at the dispatch site.{" "}
            <strong>The model is never asked whether it may act.</strong>
          </p>
          <div className="lp-hero-cta">
            <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
              Build your first agent
            </a>
            <a className="lp-btn lp-btn--ghost" href="#authorization">
              See how a decision is made
            </a>
          </div>
        </div>
        <div className="lp-hero-aside">
          <div className="lp-term">
            <div className="lp-term-bar">
              <span className="lp-term-label">install</span>
              <CopyButton text={INSTALL} />
            </div>
            <pre>
              <span className="p">$ </span>git clone {TODO.clone}
              {"\n"}
              <span className="p">$ </span>cd agentprimitives && mage build
              {"\n"}
              <span className="p">$ </span>./bin/oap install
              {"\n"}
              <span className="c">
                # installs the CRDs, operator, channel daemon, NATS and
              </span>
              {"\n"}
              <span className="c">
                # SpiceDB into a cluster you already have. It does not
              </span>
              {"\n"}
              <span className="c"># create one.</span>
            </pre>
          </div>
          <ul className="lp-chips lp-hero-chips">
            <li>33 CRDs</li>
            <li>34 pluggable seams</li>
            <li>1 SpiceDB schema</li>
            <li>no telemetry code</li>
          </ul>
          <p className="lp-note">
            OAP builds from source today. There is no published binary or
            container image yet, and the repository URL above is a placeholder
            until the project is public.
          </p>
        </div>
      </header>

      {/* --------------------------------------------------- 01 the layer --- */}
      <section className="lp-section" id="authorization">
        <div className="lp-ghost" aria-hidden="true">
          01
        </div>
        <p className="lp-kicker">The decision</p>
        <h2 className="lp-h2">
          A guardrail in a system prompt is a <em>request</em>.
        </h2>
        <p className="lp-lede">
          Ask a model not to do something and you have a preference. It holds
          until the context gets long, the tool output gets adversarial, or
          someone phrases the ask differently. Most agent platforms answer this
          with a better prompt, a classifier, or a human staring at a stream of
          approvals until they stop reading them.
        </p>
        <p className="lp-lede">
          OAP moves the decision out of the prompt and into a relationship graph
          the model cannot reach, reason about, or talk its way around.{" "}
          <strong>
            Authorization is computed from the tool&rsquo;s declaration and the
            call&rsquo;s own validated arguments, then checked in SpiceDB before
            dispatch.
          </strong>{" "}
          If the answer is no, the call does not happen.
        </p>

        <div className="lp-decide">
          <div>
            <div className="lp-panel-head">
              <span>Anatomy of one check</span>
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
                        SpiceDB, at a fixed position in the pre-tool-call
                        pipeline. The call never reaches the sandbox or the MCP
                        server.
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
          author declares stateless or passthrough resolve immediately without a
          graph check, and that declaration is reviewable in the same YAML as
          the tool.
        </p>
      </section>

      {/* ------------------------------------------------- 02 primitives --- */}
      <section className="lp-section" id="primitives">
        <div className="lp-ghost" aria-hidden="true">
          02
        </div>
        <p className="lp-kicker">The primitives</p>
        <h2 className="lp-h2">Six pieces, and nothing you have to invent.</h2>
        <p className="lp-lede">
          Running an agent safely keeps decomposing into the same six problems.
          OAP names each one, gives it a resource type, and makes the answer
          reviewable in git rather than resident in someone&rsquo;s head.
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
        <p className="lp-note">
          Thirty-three custom resources in total. Kubernetes already knows how
          to review, diff, admit and roll them back, which is the point: your
          platform team does not learn a second control plane to operate agents.
        </p>
      </section>

      {/* --------------------------------------------------- 03 cannot do --- */}
      <section className="lp-section" id="guarantees">
        <div className="lp-ghost" aria-hidden="true">
          03
        </div>
        <p className="lp-kicker">Guarantees</p>
        <h2 className="lp-h2">
          What the runtime <em>cannot</em> be talked into.
        </h2>
        <p className="lp-lede">
          Capability claims are hard to check. Constraints are easy. Each of
          these is a property of the runtime rather than an instruction to the
          model, which means no phrasing, context length or tool output changes
          the answer.
        </p>
        <ol className="lp-slab lp-slab--2 lp-cannot">
          {CANNOT.map((c) => (
            <li key={c.title}>
              <div>
                <h3>{c.title}</h3>
                <p>{c.body}</p>
              </div>
            </li>
          ))}
        </ol>
        <p className="lp-note">
          Honest scoping: plan gating and the information-leakage gate are
          opt-in per AgentClass, and hostname-level egress is recorded on
          session status but enforced only by a DNS-aware CNI. Network policy at
          layer 3 and 4 is on by default, and the sandbox fails closed to
          deny-all when a class says nothing.
        </p>
      </section>

      {/* ------------------------------------------------- 04 multiplayer --- */}
      <section className="lp-section" id="multiplayer">
        <div className="lp-ghost" aria-hidden="true">
          04
        </div>
        <p className="lp-kicker">Single player, then a second person</p>
        <h2 className="lp-h2">
          The hard part starts when someone else replies.
        </h2>
        <p className="lp-lede">
          On your own, an agent needs a sandbox, a budget and a credential. Add
          one more human to the thread and four questions appear at once, and
          none of them is answered by a better model.
        </p>
        <div className="lp-cols">
          <div className="lp-col">
            <p className="lp-col-head">The four questions</p>
            <dl className="lp-qa">
              <dt>Whose permissions apply?</dt>
              <dd>
                If the agent holds the token, the answer is the union of
                everyone&rsquo;s access, handed to whoever typed last.
              </dd>
              <dt>Who can steer?</dt>
              <dd>
                A thread one person opened gets silently redirected the moment
                anyone else replies to it.
              </dd>
              <dt>Who can approve?</dt>
              <dd>
                Where an approval exists at all, the click is usually authorized
                by visibility: whoever received the message can resolve it.
              </dd>
              <dt>Whose data is it?</dt>
              <dd>
                Some data belongs to a resource with its own owner, and neither
                the requester nor the session owner can speak for it.
              </dd>
            </dl>
          </div>
          <div className="lp-col">
            <p className="lp-col-head">What OAP answers</p>
            <dl className="lp-qa">
              <dt>The subject is the human, not the agent.</dt>
              <dd>
                Agents are not subjects in the graph; only people are. A class
                picks whether to check the current requester, the person who
                started the session, or both.
              </dd>
              <dt>Joining is an owner decision.</dt>
              <dd>
                A message from a non-participant raises a permission request
                instead of being acted on. Approve and the held message replays,
                so nobody retypes it.
              </dd>
              <dt>Approval standing comes from the resource.</dt>
              <dd>
                Eligibility at raise time and authorization at click time apply
                one rule. Any single owner is enough, and a bystander is
                rejected.
              </dd>
              <dt>The ask is routed to the data&rsquo;s owner.</dt>
              <dd>
                Reads are tagged with the resource they came from, and every
                outbound has its audience computed live and intersected against
                who may see the tag.
              </dd>
            </dl>
          </div>
        </div>
        <p className="lp-note">
          Multiplayer is the Slack story today. The browser chat is single-owner
          by design: reach for it to drive an agent yourself, and for Slack when
          a team shares the thread.
        </p>
      </section>

      {/* ------------------------------------------------------ 05 control --- */}
      <section className="lp-section" id="control">
        <div className="lp-ghost" aria-hidden="true">
          05
        </div>
        <p className="lp-kicker">Your cluster, your models, your bill</p>
        <h2 className="lp-h2">Nothing here phones home.</h2>
        <p className="lp-lede">
          You bring the cluster and OAP installs into it. The outbound calls are
          the ones you configure: the model provider you chose, the MCP servers
          you declared, the hosts your tools reach. There is no telemetry or
          analytics code in the repository.
        </p>
        <div className="lp-slab lp-slab--3">
          <div>
            <p className="lp-prim-eyebrow">Models</p>
            <h3>Picked by policy, not by prompt</h3>
            <p>
              Three providers ship: Anthropic, OpenAI and OpenRouter. An admin
              curates the catalog, a cluster-wide denylist unions across tiers,
              and a class supplying its own key is opt-in and granted top down.
              A class may narrow a tier. It can never widen one. Keys live in
              Kubernetes Secrets, so an AgentClass stays in git.
            </p>
          </div>
          <div>
            <p className="lp-prim-eyebrow">Cost</p>
            <h3>Budgets that end a session</h3>
            <p>
              Turns, tokens, cumulative active run-time, a wall-clock expiry
              that applies while the session sleeps, and a cap on the whole
              delegation tree. Checked once per turn; the first tripped
              condition fails the session. Time parked waiting on a human is not
              billed against the agent.
            </p>
          </div>
          <div>
            <p className="lp-prim-eyebrow">Runaway use</p>
            <h3>Breakers on by default</h3>
            <p>
              Five consecutive failures open a per-tool breaker; ten across one
              origin trip every sibling tool at once. Two consecutive credential
              refusals end the session rather than retrying a dead token. All of
              it with no YAML. Rate limits and byte budgets are there when you
              want them.
            </p>
          </div>
        </div>
        <p className="lp-note">
          There is no dollar budget. Cost is reported after a session in
          micro-USD and labelled a list-price estimate rather than a metered
          bill, because the provider, not OAP, issues the invoice. Budgets that
          actually stop work are counted in turns, tokens and time.
        </p>
      </section>

      {/* ---------------------------------------------------- 06 registry --- */}
      <section className="lp-section" id="pluggable">
        <div className="lp-ghost" aria-hidden="true">
          06
        </div>
        <p className="lp-kicker">Extension</p>
        <h2 className="lp-h2">Registered, not branched on.</h2>
        <p className="lp-lede">
          Thirty-four aspects of the runtime are interface-and-registry seams
          with at least one shipping backend. Adding a variant is one package,
          one <code>Register</code> call in an <code>init()</code>, and one
          blank import. If a diff for a new backend touches anything outside its
          own directory, the seam is in the wrong place and the interface gets
          fixed rather than the consumer.
        </p>
        <div className="lp-seams">
          {SEAMS.map((s) => (
            <div className="lp-seam" key={s.name}>
              <span className="lp-seam-n">{s.n}</span>
              <span className="lp-seam-name">{s.name}</span>
              <span className="lp-seam-list">{s.list}</span>
            </div>
          ))}
          <a className="lp-seam lp-seam--open" href="/docs/crd-reference">
            <span className="lp-seam-n">+</span>
            <span className="lp-seam-name">Yours</span>
            <span className="lp-seam-list">
              one package, one Register, one blank import
            </span>
          </a>
        </div>
        <p className="lp-note">
          Three things are deliberately not pluggable, and the reason is the
          same for each: they are substrate, like the API server. SpiceDB, the
          NATS bus, and Kubernetes Secrets. Upstream secret managers integrate a
          layer below through External Secrets or the CSI driver, without OAP
          changing a line.
        </p>
      </section>

      {/* ------------------------------------------------------- 07 owasp --- */}
      <section className="lp-section" id="owasp">
        <div className="lp-ghost" aria-hidden="true">
          07
        </div>
        <p className="lp-kicker">Security posture</p>
        <h2 className="lp-h2">
          The OWASP Agentic Top 10, including where we <em>fall short</em>.
          <span className="lp-draft">Draft</span>
        </h2>
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
        <div className="lp-hero-cta" style={{ marginTop: 28, marginBottom: 0 }}>
          <a className="lp-btn lp-btn--ghost" href="/docs/owasp-top10">
            Read the coverage map
          </a>
          <a className="lp-btn lp-btn--ghost" href={TODO.owaspFull}>
            Full assessment
          </a>
        </div>
        <p className="lp-note">
          OWASP materials are referenced under CC BY-SA 4.0. This assessment is
          not affiliated with or endorsed by OWASP. Coverage levels are a
          qualitative judgement of breadth, not a score.
        </p>
      </section>

      {/* ------------------------------------------------------ 08 papers --- */}
      <section className="lp-section" id="papers">
        <div className="lp-ghost" aria-hidden="true">
          08
        </div>
        <p className="lp-kicker">The whitepapers</p>
        <h2 className="lp-h2">
          One paper per primitive.
          <span className="lp-draft">Draft</span>
        </h2>
        <p className="lp-lede">
          Six papers, one for each primitive, written for the person who has to
          defend the design in a review rather than the person evaluating a
          product. Each one states the problem, the mechanism, the failure it
          prevents, and what it still does not cover.
        </p>
        <div className="lp-papers">
          {PAPERS.map((p, i) => (
            <a className="lp-paper" key={p.slug} href={TODO.paper(p.slug)}>
              <span className="lp-paper-n">
                Paper {String(i + 1).padStart(2, "0")}
              </span>
              <h3>{p.title}</h3>
              <p>{p.blurb}</p>
            </a>
          ))}
        </div>
        <p className="lp-note">
          Links are placeholders while the final drafts are in review.
        </p>
      </section>

      {/* ----------------------------------------------------- 09 evidence --- */}
      <section className="lp-section" id="evidence">
        <div className="lp-ghost" aria-hidden="true">
          09
        </div>
        <p className="lp-kicker">Evidence</p>
        <h2 className="lp-h2">How we know the gates still hold.</h2>
        <p className="lp-lede">
          A permission boundary is only as good as the thing that notices when
          it stops being enforced. Three suites gate every merge, and the last
          one replays whole sessions through the real pipeline rather than
          testing the check in isolation.
        </p>
        <div className="lp-slab lp-slab--3">
          <div>
            <p className="lp-prim-eyebrow">Scripted</p>
            <h3>78 whole-session scenarios</h3>
            <p>
              Each one drives a real session, with its assertions on the tool
              result rather than the reply, because a scripted transcript says
              the same words whether a call returned rows or a permission error.
            </p>
          </div>
          <div>
            <p className="lp-prim-eyebrow">Frozen</p>
            <h3>30 golden authorization traces</h3>
            <p>
              Property assertions cover what the author thought to claim. A
              golden trace records everything that happened, so a change nobody
              named still arrives as a diff someone has to read.
            </p>
          </div>
          <div>
            <p className="lp-prim-eyebrow">Captured</p>
            <h3>Replays of a real session</h3>
            <p>
              A capture refuses to emit rather than write a bundle that would
              replay differently than it recorded. Eighteen self-check codes
              gate the write, and the tool says what it synthesized when it had
              to.
            </p>
          </div>
        </div>
        <p className="lp-note">
          The distinction is evidentiary and it is the reason the suites are
          separate: a green scripted scenario proves the system handles an
          interaction. A green captured one also shows a real model produced it,
          at least once. Neither pins what a model will do next.
        </p>
      </section>

      {/* ----------------------------------------------------------- close --- */}
      <section className="lp-close">
        <p className="lp-kicker">Start here</p>
        <h2 className="lp-h2">Give an agent a job, not the keyring.</h2>
        <p className="lp-lede">
          Install into a cluster you already run, declare one agent, and watch
          the first denial land before the tool ever executes.
        </p>
        <div className="lp-hero-cta">
          <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
            Build your first agent
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
        <a href="#owasp">Security</a>
        <a href={TODO.discuss}>Community</a>
        <span>Built on SpiceDB.</span>
      </footer>
    </div>
  );
}
