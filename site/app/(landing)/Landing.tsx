"use client";
import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type ComponentProps,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { useTheme } from "next-themes";
import { Wordmark } from "@/components/Wordmark";
import { OapMark, OAP_MARK_PATHS } from "@/components/OapMark";
import { SiteFooter } from "@/components/SiteFooter";
import { THEME_MODES, type ThemeMode } from "@/components/ThemeToggle";
import { DiscordIcon, GitHubIcon } from "@/components/BrandIcons";
import { DISCORD_URL } from "@/lib/site";
import { getAsset } from "@/lib/manifest";
import { GeoFigure } from "./geo/GeoFigure";
import { OWASP, COVERAGE_LABEL, coverageSummary } from "./owasp";

const REPO = "https://github.com/authzed/openagentprimitives";

/* The page's copy follows the repository README: its order, its emphasis and
 * its claims. When the README's story changes, change this page to match. */

/* ------------------------------------------------------------------ data --- */

/* One of the five relay hues (--lp-h0..--lp-h4). Each list cycles through
 * them so neighbouring items never share a color; the hue is data, not a
 * position-derived calculation, so reordering a list keeps each item's color. */
type Hue = 0 | 1 | 2 | 3 | 4;
type GeoName = ComponentProps<typeof GeoFigure>["name"];

const hueStyle = (hue: Hue): CSSProperties =>
  ({ "--hue": `var(--lp-h${hue})` }) as CSSProperties;

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

/* The README's four trust questions. Each links to the part of the page that
 * answers it; the #area-N targets select that tab in the security section. */
const QUESTIONS: { q: string; to: string; href: string; hue: Hue }[] = [
  { q: "What can it reach?", to: "The problem", href: "#problem", hue: 0 },
  { q: "Who said it could?", to: "Authorization", href: "#area-1", hue: 1 },
  {
    q: "What if a tool result tells it to do something else?",
    to: "Injection",
    href: "#area-2",
    hue: 2,
  },
  {
    q: "If one piece is compromised, what’s exposed?",
    to: "Isolation",
    href: "#area-6",
    hue: 3,
  },
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
    "Visible to the model and every tool",
    "Injected for the tool that needs them, never shown to the model",
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

interface Primitive {
  verb: string;
  title: string;
  body: string;
  href: string;
  hue: Hue;
  geo: GeoName;
  /** The figure's accessible description: what the drawing shows. */
  figLabel: string;
}

const PRIMITIVES: Primitive[] = [
  {
    verb: "Define",
    title: "Agent definition",
    body: "An AgentClass is a reviewable, versioned template. Each AgentSession gets its own isolated runtime, budget and audit trail, and is torn down when the work is done.",
    href: "/docs/agent-definition",
    hue: 0,
    geo: "define",
    figLabel:
      "An agent class stamps out sessions, each with its own runtime, torn down when done",
  },
  {
    verb: "Constrain",
    title: "Safe tools",
    body: "A tool is a declarative contract, checked when it is written and again when it runs. Calls execute in a hardened sandbox, with no shell interpolation of arguments.",
    href: "/docs/safe-tools",
    hue: 1,
    geo: "gate",
    figLabel:
      "Each tool call passes through a gate; allowed calls reach the tool, denied ones never run",
  },
  {
    verb: "Broker",
    title: "Identity & credentials",
    body: "An agent acts as itself or on a person's behalf. It gets the use of a credential without being given it: a broker injects it into the tool that needs it, and the model never sees it.",
    href: "/docs/identity",
    hue: 2,
    geo: "broker",
    figLabel:
      "The broker injects the credential into the tool that uses it; the model holds only a reference",
  },
  {
    verb: "Decide",
    title: "Authorization",
    body: "Every action is checked at the moment it runs: may this agent, or the person it acts for, do this to this resource? The agent asks; it never answers.",
    href: "/docs/authorization",
    hue: 3,
    geo: "lens",
    figLabel:
      "Two overlapping circles, a person and the agent. Unattended, the agent works with its full scope; with a person present it may do only what both allow",
  },
  {
    verb: "Carry",
    title: "Channels & continuity",
    body: "Agents work in Slack, the browser, the CLI and GitHub. Every message and approval click is checked against who sent it before it counts.",
    href: "/docs/channels",
    hue: 4,
    geo: "channels",
    figLabel: "A schedule, GitHub and Slack each wake a session",
  },
  {
    verb: "Remember",
    title: "Memory & knowledge",
    body: "One store holds what the agent learned and a signed record of what it did. Every read is checked item by item against who is asking.",
    href: "/docs/memory",
    hue: 0,
    geo: "memory",
    figLabel:
      "A session reads memory only for the resources it's bound to; reading another is refused",
  },
];

/* Icons are drawn on a 48-unit grid. `lp-ico-fill` marks the shape that takes
 * the item's hue; `lp-ico-dot` a node that takes it as a stroke. */
interface Area {
  title: string;
  hue: Hue;
  icon: ReactNode;
  points: [string, ReactNode][];
  links: [string, string][];
}

const AREAS: Area[] = [
  {
    title: "Every action is authorized",
    hue: 0,
    icon: (
      <>
        <path d="M2 30h44" />
        <path
          d="M22.71 8.75C23.28 7.75 24.72 7.75 25.29 8.75L40.78 35.77C41.35 36.76 40.64 38 39.49 38H8.51C7.36 38 6.65 36.76 7.22 35.77L22.71 8.75ZM24.65 21.13C24.36 20.63 23.64 20.63 23.35 21.13L18.57 29.47C18.29 29.97 18.65 30.58 19.22 30.58H28.78C29.35 30.58 29.71 29.97 29.43 29.47L24.65 21.13Z"
          fillRule="evenodd"
          className="lp-ico-fill"
        />
      </>
    ),
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
    hue: 1,
    icon: (
      <>
        <path
          d="M22.45 6.9C23.14 5.7 24.86 5.7 25.55 6.9L44.14 39.32C44.82 40.51 43.96 42 42.59 42H5.41C4.04 42 3.17 40.51 3.86 39.32L22.45 6.9ZM24.78 21.76C24.43 21.16 23.57 21.16 23.22 21.76L17.49 31.76C17.15 32.36 17.58 33.1 18.26 33.1H29.74C30.42 33.1 30.85 32.36 30.51 31.76L24.78 21.76Z"
          fillRule="evenodd"
          className="lp-ico-fill"
        />
        <path d="M46 18l-8 6" />
        <circle cx="46" cy="18" r="1.5" className="lp-ico-dot" />
      </>
    ),
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
    hue: 2,
    icon: (
      <>
        <circle cx="20" cy="24" r="14" />
        <circle cx="20" cy="24" r="7" className="lp-ico-fill" />
        <circle cx="40" cy="24" r="5" />
        <path d="M34 24h-6" />
      </>
    ),
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
    hue: 3,
    icon: (
      <>
        <path d="M22.45 6.9C23.14 5.7 24.86 5.7 25.55 6.9L44.14 39.32C44.82 40.51 43.96 42 42.59 42H5.41C4.04 42 3.17 40.51 3.86 39.32L22.45 6.9Z" />
        <path
          d="M23.35 25.37C23.64 24.88 24.36 24.88 24.65 25.37L32.39 38.88C32.68 39.38 32.32 40 31.75 40H16.25C15.68 40 15.32 39.38 15.61 38.88L23.35 25.37ZM24.32 31.57C24.18 31.32 23.82 31.32 23.68 31.57L21.29 35.73C21.14 35.98 21.32 36.29 21.61 36.29H26.39C26.68 36.29 26.86 35.98 26.71 35.73L24.32 31.57Z"
          fillRule="evenodd"
          className="lp-ico-fill"
        />
      </>
    ),
    points: [
      [
        "Opt-in capabilities.",
        "No tool or capability is granted until someone adds it.",
      ],
      [
        "Isolated sandboxes.",
        "Credentials are injected only for the tool that needs them, and the model never sees them.",
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
    hue: 4,
    icon: (
      <>
        <path d="M8 16h32M8 32h14" />
        <path d="M22 32h18" className="lp-ico-faint" />
        <circle cx="22" cy="32" r="3" className="lp-ico-fill" />
        <path d="M22 8v32" />
      </>
    ),
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
    hue: 0,
    icon: (
      <>
        <path d="M12 18 20 22 12 26 4 22Z" className="lp-ico-fill" />
        <path d="M36 18 44 22 36 26 28 22Z" />
        <path d="M24 30 32 34 24 38 16 34Z" />
        <path d="M24 6 46 22 24 44 2 22Z" />
      </>
    ),
    points: [
      [
        "Sanitized output.",
        "Each content type has its own sanitizer, and rendered output runs under a strict CSP.",
      ],
      [
        "Separate pods.",
        "Runner, operator and sanitizers run in separate pods, and tool sandboxes can run on micro-VMs where the cluster provides them.",
      ],
      [
        "Scoped credentials.",
        "Every component gets its own bus credential, and a session's pods can publish only on that session's channels.",
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

const EVERYTHING: { title: string; body: string; hue: Hue; icon: ReactNode }[] =
  [
    {
      title: "Your choice of model",
      body: "Anthropic, OpenAI or OpenRouter, swappable per deployment.",
      hue: 0,
      icon: (
        <>
          <circle cx="10" cy="24" r="6" />
          <circle cx="24" cy="24" r="6" className="lp-ico-fill" />
          <circle cx="38" cy="24" r="6" />
          <path d="M16 24h2M30 24h2" />
        </>
      ),
    },
    {
      title: "Your infrastructure",
      body: "A laptop, a local cluster, GKE, EKS, AKS or self-managed Kubernetes.",
      hue: 1,
      icon: (
        <>
          <path d="M24 30 40 36 24 42 8 36Z" />
          <path d="M24 21 40 27 24 33 8 27Z" />
          <path d="M24 12 40 18 24 24 8 18Z" className="lp-ico-fill" />
        </>
      ),
    },
    {
      title: "Channels",
      body: "Slack, the browser, the CLI and GitHub, plus signed webhooks and scheduled runs. Transports are pluggable, so you can add your own.",
      hue: 2,
      icon: (
        <>
          <path d="M4 12c10 0 12 12 22 12M4 24h22M4 36c10 0 12-12 22-12" />
          <path
            d="M35.31 15.4C35.62 14.87 36.38 14.87 36.69 15.4L44.95 29.81C45.26 30.34 44.87 31 44.26 31H27.74C27.13 31 26.74 30.34 27.05 29.81L35.31 15.4ZM36.34 22C36.19 21.74 35.81 21.74 35.66 22L33.11 26.45C32.95 26.72 33.15 27.05 33.45 27.05H38.55C38.85 27.05 39.05 26.72 38.89 26.45L36.34 22Z"
            fillRule="evenodd"
            className="lp-ico-fill"
          />
        </>
      ),
    },
    {
      title: "Memory and knowledge graph",
      body: "Structured recall, ranked search and graph queries.",
      hue: 3,
      icon: (
        <>
          <path d="M12 14 24 10 36 18 30 34 16 36 12 14ZM24 10 30 34M12 14 36 18" />
          <circle cx="12" cy="14" r="3" className="lp-ico-dot" />
          <circle cx="24" cy="10" r="3" className="lp-ico-dot" />
          <circle cx="36" cy="18" r="3" className="lp-ico-dot" />
          <circle cx="30" cy="34" r="3" className="lp-ico-dot" />
          <circle cx="16" cy="36" r="3" className="lp-ico-dot" />
        </>
      ),
    },
    {
      title: "Built in and swappable",
      body: "The runner, authorization, approvals, sandboxing, credentials, memory and audit ship built in. Model providers, sandbox backends, memory stores, channels and artifact storage plug in through interfaces, so you can use what you already run.",
      hue: 4,
      icon: (
        <>
          <rect
            x="6"
            y="8"
            width="16"
            height="16"
            rx="2"
            className="lp-ico-fill"
          />
          <rect x="26" y="24" width="16" height="16" rx="2" />
          <path d="M28 10h6a4 4 0 0 1 4 4v4M35 15l3 3 3-3M20 38h-6a4 4 0 0 1-4-4v-4M13 33l-3-3-3 3" />
        </>
      ),
    },
    {
      title: "Under your control",
      body: "OAP runs inside a cluster you operate. Your team owns its network, access and upgrades.",
      hue: 0,
      icon: (
        <>
          <rect x="5" y="5" width="38" height="38" rx="6" />
          <path
            d="M23.14 13.5C23.52 12.83 24.48 12.83 24.86 13.5L35.19 31.51C35.57 32.17 35.09 33 34.33 33H13.67C12.91 33 12.43 32.17 12.81 31.51L23.14 13.5ZM24.43 21.76C24.24 21.42 23.76 21.42 23.57 21.76L20.38 27.31C20.19 27.64 20.43 28.06 20.81 28.06H27.19C27.57 28.06 27.81 27.64 27.62 27.31L24.43 21.76Z"
            fillRule="evenodd"
            className="lp-ico-fill"
          />
        </>
      ),
    },
  ];

/* ------------------------------------------------------------ components --- */

function Ico({ children }: { children: ReactNode }) {
  return (
    <svg className="lp-ico" viewBox="0 0 48 48" aria-hidden="true">
      {children}
    </svg>
  );
}

/* The relay band: the mark's own outline used as a clip over a moving
 * gradient of the five hues. It sits on top of a plain mark and fades in only
 * while its .lp-relay-host is hovered or focused (landing.css). */
const RELAY_STOPS: Hue[] = [0, 1, 2, 3, 4, 3, 2, 1, 0];

function MarkRelay({ className }: { className?: string }) {
  const id = useId();
  const clip = `${id}-clip`;
  const band = `${id}-band`;
  return (
    <svg
      className={className ? `lp-mark-relay ${className}` : "lp-mark-relay"}
      viewBox="0 0 1000 393"
      aria-hidden="true"
    >
      <defs>
        <clipPath id={clip}>
          {OAP_MARK_PATHS.map((d) => (
            <path key={d} d={d} />
          ))}
        </clipPath>
        <linearGradient
          id={band}
          gradientUnits="userSpaceOnUse"
          x1="0"
          y1="0"
          x2="3200"
          y2="0"
          spreadMethod="repeat"
        >
          {RELAY_STOPS.map((h, i) => (
            <stop
              key={i}
              offset={i / (RELAY_STOPS.length - 1)}
              style={{ stopColor: `var(--lp-h${h}, var(--lp-state))` }}
            />
          ))}
        </linearGradient>
      </defs>
      <g clipPath={`url(#${clip})`}>
        <rect
          className="lp-mark-band"
          width="4400"
          height="393"
          fill={`url(#${band})`}
        />
      </g>
    </svg>
  );
}

/* The nav's theme control: one button whose icon shows the current choice,
 * opening a three-item menu. It reads and writes the same next-themes state as
 * the footer's ThemeToggle, so the two always agree. */
function ThemeMenu() {
  const { theme, setTheme } = useTheme();
  // The server cannot know the stored choice, so the button shows System
  // until mount; rendering the real one would mismatch hydration.
  const [mounted, setMounted] = useState(false);
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const items = useRef<(HTMLButtonElement | null)[]>([]);
  useEffect(() => setMounted(true), []);

  const mode: ThemeMode = mounted
    ? (THEME_MODES.find((m) => m.id === theme)?.id ?? "system")
    : "system";
  const current = THEME_MODES.find((m) => m.id === mode);

  useEffect(() => {
    if (!open) return;
    items.current[THEME_MODES.findIndex((m) => m.id === mode)]?.focus();
    const onPointerDown = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      button.current?.focus();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
    // Focus moves into the menu once, when it opens; not on every mode change.
  }, [open]);

  const onMenuKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = TAB_STEP[e.key];
    if (!step) return;
    e.preventDefault();
    const at = items.current.findIndex((el) => el === document.activeElement);
    const next = (at + step + THEME_MODES.length) % THEME_MODES.length;
    items.current[next]?.focus();
  };

  return (
    <div className="theme-menu lp-nav-theme" ref={root}>
      <button
        ref={button}
        type="button"
        className="tm-btn"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Color theme: ${current?.label ?? "System"}`}
        title="Color theme"
        onClick={() => setOpen((o) => !o)}
      >
        {current?.icon}
      </button>
      {open && (
        <div
          className="tm-list"
          role="menu"
          aria-label="Color theme"
          onKeyDown={onMenuKeyDown}
        >
          {THEME_MODES.map((m, i) => (
            <button
              key={m.id}
              ref={(el) => {
                items.current[i] = el;
              }}
              type="button"
              className="tm-item"
              role="menuitemradio"
              aria-checked={m.id === mode}
              onClick={() => {
                setTheme(m.id);
                setOpen(false);
                button.current?.focus();
              }}
            >
              {m.icon}
              {m.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/* Arrow keys move one tab either way, whichever way the list is laid out. */
const TAB_STEP: Record<string, number> = {
  ArrowDown: 1,
  ArrowRight: 1,
  ArrowUp: -1,
  ArrowLeft: -1,
};

function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  const copy = useCallback(() => {
    if (!navigator.clipboard) return;
    navigator.clipboard.writeText(text).then(
      () => {
        setDone(true);
        window.setTimeout(() => setDone(false), 1600);
      },
      (err: unknown) => console.warn("copy to clipboard failed", err),
    );
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
    const step = TAB_STEP[e.key];
    if (!step) return;
    e.preventDefault();
    const next = (active + step + INSTALL.length) % INSTALL.length;
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
function DemoClip({ name, caption }: { name: string; caption?: ReactNode }) {
  const asset = getAsset(name);
  if (!asset || (!asset.webm && !asset.mp4)) return null;
  return (
    <figure className="lp-clip">
      <video controls playsInline preload="metadata" poster={asset.poster}>
        {asset.webm && <source src={asset.webm} type="video/webm" />}
        {asset.mp4 && <source src={asset.mp4} type="video/mp4" />}
      </video>
      {caption && <figcaption>{caption}</figcaption>}
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
    <div className="lp-head-title">
      <p className="lp-kicker">{kicker}</p>
      <h2 className="lp-h2">{children}</h2>
    </div>
  );
}

/* A section head: title on the left, lede on the right (stacked on narrow
 * screens). */
function Head({
  kicker,
  title,
  lede,
}: {
  kicker: string;
  title: ReactNode;
  lede: ReactNode;
}) {
  return (
    <div className="lp-head">
      <SectionHead kicker={kicker}>{title}</SectionHead>
      <p className="lp-lede">{lede}</p>
    </div>
  );
}

const prefersReducedMotion = () =>
  window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/* The primitives, as a list beside a deck of cards. As each list row passes
 * 30% of the viewport the deck deals one card: the row's card comes to the
 * top (depth 0), the ones before it fly off (dealt), the rest stack behind.
 * Below 760px the deck is hidden and each row shows its own figure inline. */
function Primitives() {
  // Index of the last row past the line; -1 leaves the cover on top. The
  // server renders -1, the same state the page has before any scrolling.
  const [dealt, setDealt] = useState(-1);
  const rows = useRef<(HTMLLIElement | null)[]>([]);

  useEffect(() => {
    let frame = 0;
    const deal = () => {
      frame = 0;
      const line = window.innerHeight * 0.3;
      let at = -1;
      rows.current.forEach((row, i) => {
        if (row && row.getBoundingClientRect().top <= line) at = i;
      });
      setDealt(at);
    };
    const schedule = () => {
      if (!frame) frame = window.requestAnimationFrame(deal);
    };
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule, { passive: true });
    deal();
    return () => {
      window.removeEventListener("scroll", schedule);
      window.removeEventListener("resize", schedule);
      if (frame) window.cancelAnimationFrame(frame);
    };
  }, []);

  // A card further down the stack is a shortcut to its row: clicking it
  // scrolls that row to the middle, which deals the card to the top.
  const goTo = (i: number) =>
    rows.current[i]?.scrollIntoView({
      behavior: prefersReducedMotion() ? "auto" : "smooth",
      block: "center",
    });

  return (
    <div className="lp-explore">
      <div>
        <Head
          kicker="The primitives"
          title="Six concerns every agent has to solve."
          lede="A primitive is a basic building block that agents rely on, whatever they do. OAP ships a working implementation of every one."
        />
        <ol className="lp-explore-list">
          {PRIMITIVES.map((p, i) => (
            <li
              key={p.title}
              ref={(el) => {
                rows.current[i] = el;
              }}
              className={`lp-explore-item${i === dealt ? " is-active" : ""}`}
              aria-current={i === dealt ? "step" : undefined}
              style={hueStyle(p.hue)}
            >
              <div className="lp-explore-inline geo-slot">
                <GeoFigure name={p.geo} label={p.figLabel} hue={p.hue} />
              </div>
              <p className="lp-prim-eyebrow">
                #{i + 1} {p.verb}
              </p>
              <h3>
                <a href={p.href}>{p.title}</a>
              </h3>
              <p>{p.body}</p>
            </li>
          ))}
        </ol>
      </div>
      <div className="lp-explore-stage geo-slot">
        <div className="lp-deck">
          <DeckCard
            depth={dealt < 0 ? 0 : -1}
            className="lp-deck-cover lp-relay-host"
          >
            <p className="lp-tcard-top">
              <span>OAP</span>
              <span>The primitives</span>
            </p>
            <div className="lp-tcard-art lp-cover-art">
              <OapMark className="lp-cover-mark" />
              <MarkRelay className="lp-cover-mark" />
            </div>
            <p className="lp-tcard-title">Six primitives</p>
            <p className="lp-tcard-foot">
              <span>Scroll to view</span>
              <span>{PRIMITIVES.length} cards</span>
            </p>
          </DeckCard>
          {PRIMITIVES.map((p, i) => {
            const depth = i - dealt;
            return (
              <DeckCard
                key={p.title}
                depth={depth}
                hue={p.hue}
                onClick={depth > 0 ? () => goTo(i) : undefined}
              >
                <p className="lp-tcard-top">
                  <span>#{i + 1}</span>
                  <span>{p.verb}</span>
                </p>
                <div className="lp-tcard-art">
                  <GeoFigure
                    name={p.geo}
                    label={p.figLabel}
                    hue={p.hue}
                    paused={depth !== 0}
                  />
                </div>
                <p className="lp-tcard-title">{p.title}</p>
                <p className="lp-tcard-foot">
                  <span>{p.href.replace(/^\//, "")}</span>
                  <span>
                    {i + 1} / {PRIMITIVES.length}
                  </span>
                </p>
              </DeckCard>
            );
          })}
        </div>
      </div>
    </div>
  );
}

/* One card in the deck. Depth 0 is on top and the only one exposed to
 * assistive tech; a negative depth means already dealt off the top. */
function DeckCard({
  depth,
  hue,
  className,
  onClick,
  children,
}: {
  depth: number;
  hue?: Hue;
  className?: string;
  onClick?: () => void;
  children: ReactNode;
}) {
  const style = {
    "--d": Math.max(depth, 0),
    ...(hue === undefined ? {} : hueStyle(hue)),
  } as CSSProperties;
  return (
    // The click is a pointer shortcut only; keyboard and screen-reader users
    // reach the same row through the list's links.
    <div
      className={`lp-deck-card${className ? ` ${className}` : ""}`}
      data-depth={depth >= 0 ? depth : undefined}
      data-dealt={depth < 0 ? "" : undefined}
      style={style}
      aria-hidden={depth !== 0}
      onClick={onClick}
    >
      {children}
    </div>
  );
}

/* The six control areas as ARIA tabs. The selected tab follows the URL hash,
 * so #area-N links (the four questions) open their area. Before hydration
 * every panel renders, so the content is all there without JavaScript; only
 * once mounted are the unselected panels hidden. Below 760px the index is
 * hidden and the panels stack (landing.css). */
function SecurityAreas() {
  const [active, setActive] = useState(0);
  const [mounted, setMounted] = useState(false);
  // Bumped on a hash navigation, so the effect below scrolls once the newly
  // selected panel has rendered visible. Selecting a tab never scrolls.
  const [jump, setJump] = useState(0);
  const tabs = useRef<(HTMLButtonElement | null)[]>([]);

  useEffect(() => {
    setMounted(true);
    const fromHash = () => {
      const m = /^#area-(\d+)$/.exec(window.location.hash);
      if (!m) return;
      const i = Number(m[1]) - 1;
      if (i < 0 || i >= AREAS.length) return;
      setActive(i);
      setJump((n) => n + 1);
    };
    fromHash();
    window.addEventListener("hashchange", fromHash);
    return () => window.removeEventListener("hashchange", fromHash);
  }, []);

  useEffect(() => {
    if (!jump) return;
    const m = /^#(area-\d+)$/.exec(window.location.hash);
    if (m) document.getElementById(m[1])?.scrollIntoView({ block: "center" });
  }, [jump]);

  const select = (i: number, focus: boolean) => {
    setActive(i);
    if (focus) tabs.current[i]?.focus();
  };

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    let next: number;
    if (e.key === "Home") next = 0;
    else if (e.key === "End") next = AREAS.length - 1;
    else if (TAB_STEP[e.key])
      next = (active + TAB_STEP[e.key] + AREAS.length) % AREAS.length;
    else return;
    e.preventDefault();
    select(next, true);
  };

  return (
    <div className="lp-sec">
      <div
        className="lp-sec-index"
        role="tablist"
        aria-label="Control areas"
        aria-orientation="vertical"
        onKeyDown={onKeyDown}
      >
        {AREAS.map((a, i) => (
          <button
            key={a.title}
            ref={(el) => {
              tabs.current[i] = el;
            }}
            type="button"
            role="tab"
            id={`area-tab-${i + 1}`}
            aria-selected={i === active}
            aria-controls={`area-${i + 1}`}
            tabIndex={i === active ? 0 : -1}
            className="lp-sec-tab"
            style={hueStyle(a.hue)}
            onClick={() => select(i, false)}
          >
            <Ico>{a.icon}</Ico>
            {a.title}
          </button>
        ))}
      </div>
      <div className="lp-sec-panels">
        {AREAS.map((a, i) => (
          <article
            key={a.title}
            id={`area-${i + 1}`}
            role="tabpanel"
            aria-labelledby={`area-tab-${i + 1}`}
            hidden={mounted && i !== active}
            className="lp-sec-panel"
            style={hueStyle(a.hue)}
          >
            <div className="lp-sec-panel-head">
              <Ico>{a.icon}</Ico>
              <div className="lp-sec-panel-name">
                <h3 className="lp-areas-title">{a.title}</h3>
              </div>
            </div>
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
          </article>
        ))}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ page --- */

export function Landing() {
  return (
    <div className="lp">
      <nav className="lp-nav">
        <a
          className="lp-nav-brand lp-relay-host"
          href="/"
          aria-label="Open Agent Primitives home"
        >
          <span className="lp-nav-mark">
            <Wordmark className="lp-nav-wordmark" />
            <MarkRelay />
          </span>
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
          <ThemeMenu />
          <a className="lp-btn lp-btn--primary" href="/docs/quickstart">
            Get started
          </a>
        </div>
      </nav>

      {/* ------------------------------------------------------------ hero --- */}
      <main>
        <header className="lp-hero">
          <div className="lp-hero-main">
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

        {/* -------------------------------------------------- questions --- */}
        <section className="lp-qs" aria-label="Four questions">
          <p className="lp-hero-qs-head">
            Before you trust an agent with real work, you need answers to four
            questions.
          </p>
          <ol className="lp-hero-qs">
            {QUESTIONS.map((q) => (
              <li key={q.href} style={hueStyle(q.hue)}>
                <a href={q.href}>
                  <span className="lp-qs-q">{q.q}</span>
                  <span className="lp-qs-to">{q.to}</span>
                </a>
              </li>
            ))}
          </ol>
          <p className="lp-hero-qs-foot">
            OAP answers all four in the platform, before the agent acts.{" "}
            <a href="#secure">See how</a>
          </p>
        </section>

        {/* ---------------------------------------------------- 01 problem --- */}
        <section className="lp-section" id="problem">
          <div className="lp-split">
            <div>
              <SectionHead kicker="The problem">
                Your agent can reach <em>everything</em> its credentials can.
              </SectionHead>
              <p className="lp-lede">
                A token for one repository usually reaches every repository. An
                agent inherits all of it.
              </p>
              <p className="lp-lede">
                A model can&rsquo;t reliably tell instructions from data, so its
                judgment can&rsquo;t be the boundary. A better prompt is still
                just an instruction.
              </p>
              <p className="lp-lede">
                <strong>
                  So OAP enforces every rule itself, before each action runs,
                  where no prompt can change it.
                </strong>
              </p>
            </div>
            <div className="geo-slot lp-split-fig">
              <GeoFigure
                name="union"
                label="The agent's token can access five repositories, but its task needs only one; the other four are unneeded access"
                hue={0}
              />
            </div>
          </div>

          {/* The rows are laid out as a grid, which strips a table's implicit
              roles in some browsers; the explicit roles put them back. */}
          <div className="lp-compare-wrap">
            <table className="lp-compare" role="table">
              <thead role="rowgroup">
                <tr role="row">
                  <th scope="col" role="columnheader">
                    <span className="lp-sr">Question</span>
                  </th>
                  <th scope="col" role="columnheader">
                    Typical platform
                  </th>
                  <th scope="col" role="columnheader">
                    OAP
                  </th>
                </tr>
              </thead>
              <tbody role="rowgroup">
                {COMPARE.map(([q, typical, oap]) => (
                  <tr key={q} role="row">
                    <th scope="row" role="rowheader">
                      {q}
                    </th>
                    <td className="lp-compare-typical" role="cell">
                      {typical}
                    </td>
                    <td className="lp-compare-oap" role="cell">
                      {oap}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        {/* ------------------------------------------------- 02 primitives --- */}
        <section className="lp-section" id="primitives">
          <Primitives />
        </section>

        {/* ---------------------------------------------------- 03 secure --- */}
        <section className="lp-section" id="secure">
          <Head
            kicker="What makes it secure"
            title="Controls in six areas."
            lede="The platform enforces every control. No prompt or tool output can affect enforcement."
          />
          <SecurityAreas />
          <p className="lp-note">
            Plan gating and leakage tracking are opt-in per agent class. Network
            policy is on by default.
          </p>
        </section>

        {/* --------------------------------------------- 04 agent builder --- */}
        <section className="lp-section" id="agent-builder">
          <Head
            kicker="Agent Builder"
            title="Build an agent by talking to an agent."
            lede="Agent Builder is an OAP agent that creates other agents. Describe what you need in plain language, then test it live."
          />
          <div className="lp-builder">
            <ol className="lp-steps">
              {BUILDER.map((b) => (
                <li key={b.step}>
                  <p className="lp-prim-eyebrow">{b.step}</p>
                  <h3>{b.title}</h3>
                  <p>{b.body}</p>
                </li>
              ))}
            </ol>
            <DemoClip name="builder-first-agent" />
          </div>
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
          <Head
            kicker="Everything else"
            title="The rest of the platform, included."
            lede={
              <>The capabilities you&rsquo;d expect from any agent platform.</>
            }
          />
          <div className="lp-list3">
            {EVERYTHING.map((e) => (
              <div key={e.title} style={hueStyle(e.hue)}>
                <Ico>{e.icon}</Ico>
                <h3>{e.title}</h3>
                <p>{e.body}</p>
              </div>
            ))}
          </div>
        </section>

        {/* ------------------------------------------------------- 06 owasp --- */}
        <section className="lp-section" id="owasp">
          <Head
            kicker="Security posture"
            title={
              <>
                Coverage of the OWASP{" "}
                <span className="lp-nowrap">Agentic Top 10.</span>
              </>
            }
            lede="How OAP maps to each risk in the OWASP Top 10 for Agentic Applications, with the remaining gaps listed alongside. Each item links to its full write-up."
          />
          <div className="lp-owasp-wrap">
            <table className="lp-owasp">
              <caption>
                {coverageSummary(OWASP)}
                {/* One segment per row, in row order: the caption's counts at
                    a glance. */}
                <span className="lp-cov-bar" aria-hidden="true">
                  {OWASP.map(([id, , level]) => (
                    <i key={id} className={`lp-cov-seg lp-cov-seg--${level}`} />
                  ))}
                </span>
              </caption>
              <thead>
                <tr>
                  <th scope="col">Item</th>
                  <th scope="col">Risk</th>
                  <th scope="col">Coverage</th>
                  <th scope="col">Not yet</th>
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
                        {COVERAGE_LABEL[level]}
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
          <div className="lp-close-text">
            <p className="lp-kicker">Defense in depth</p>
            <h2 className="lp-h2">Every layer assumes the others may fail.</h2>
            <p className="lp-lede">
              To misuse an agent, an attacker has to get past an approved plan,
              a locked slot, a check on every call, a tool spec, and a sandbox.
              The model never sees a credential.
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
          </div>
          <div className="geo-slot lp-close-fig">
            <GeoFigure
              name="depth"
              label="Five stacked layers: plan, slot, call check, tool spec, sandbox. An attack gets through one layer and the next holds"
              hue={0}
            />
          </div>
        </section>
      </main>

      <SiteFooter />
    </div>
  );
}
