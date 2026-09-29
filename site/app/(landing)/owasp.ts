// The landing page's OWASP Agentic Top 10 coverage table. Each row's id
// (lowercased) must match an `id="…"` anchor in content/docs/owasp-top10.mdx;
// see owasp.test.ts, which pins this against a templated href the static
// link check (scripts/check.ts) cannot follow.
export type CoverageLevel = "substantial" | "partial" | "na";

export const OWASP: readonly (readonly [
  id: string,
  title: string,
  level: CoverageLevel,
  gap: string,
])[] = [
  [
    "ASI01",
    "Agent Goal Hijack",
    "partial",
    "No goal-lock or plan-divergence detection; meta tools bypass the gate.",
  ],
  [
    "ASI02",
    "Tool Misuse and Exploitation",
    "substantial",
    "Egress enforcement is L3/L4; volume budgets are per-call and opt-in.",
  ],
  [
    "ASI03",
    "Identity and Privilege Abuse",
    "substantial",
    "Sidecar credentials freeze at pod-create; revocation delivery is at-most-once.",
  ],
  [
    "ASI04",
    "Agentic Supply Chain",
    "partial",
    "A hash pins content, not origin. No descriptor signing, no SBOM or AIBOM.",
  ],
  [
    "ASI05",
    "Unexpected Code Execution",
    "substantial",
    "Per-call isolation deferred: calls in one session share UID, /proc, /tmp, /work.",
  ],
  [
    "ASI06",
    "Memory and Context Poisoning",
    "partial",
    "No content validation on writes; no quarantine, decay or rollback.",
  ],
  [
    "ASI07",
    "Insecure Inter-Agent Communication",
    "na",
    "One agent per session; channels are human to agent, not agent to agent.",
  ],
  [
    "ASI08",
    "Cascading Failures",
    "partial",
    "Breakers are per tool and per origin rather than cross-cutting.",
  ],
  [
    "ASI09",
    "Human-Agent Trust Exploitation",
    "substantial",
    "No plan-divergence cross-check; no confidence-weighted cues in the UI.",
  ],
  [
    "ASI10",
    "Rogue Agents",
    "partial",
    "The tool manifest is enforced but unsigned; tamper-evidence is not tamper-proofing.",
  ],
];
