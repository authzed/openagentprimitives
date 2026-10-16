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
    "Goal-lock and plan-divergence detection aren't built yet.",
  ],
  [
    "ASI02",
    "Tool Misuse and Exploitation",
    "substantial",
    "Egress is enforced at L3/L4 so far, and volume budgets are per call and opt-in for now.",
  ],
  [
    "ASI03",
    "Identity and Privilege Abuse",
    "substantial",
    "Sidecar credentials are fixed when the pod starts, and revocation is delivered at most once for now.",
  ],
  [
    "ASI04",
    "Agentic Supply Chain",
    "partial",
    "A hash pins content but not yet origin. Descriptor signing and an SBOM or AIBOM aren't there yet.",
  ],
  [
    "ASI05",
    "Unexpected Code Execution",
    "substantial",
    "Isolation is per session for now: tool calls within one session share a UID, /proc, /tmp and /work.",
  ],
  [
    "ASI06",
    "Memory and Context Poisoning",
    "partial",
    "Writes aren't content-validated yet, and quarantine, decay and rollback aren't built yet.",
  ],
  [
    "ASI07",
    "Insecure Inter-Agent Communication",
    "partial",
    "Delegation stays within one cluster for now; a protocol for agents outside it isn't there yet.",
  ],
  [
    "ASI08",
    "Cascading Failures",
    "partial",
    "Breakers work per tool and per origin; cross-cutting breakers aren't there yet.",
  ],
  [
    "ASI09",
    "Human-Agent Trust Exploitation",
    "substantial",
    "A plan-divergence cross-check and confidence-weighted cues in the UI aren't built yet.",
  ],
  [
    "ASI10",
    "Rogue Agents",
    "partial",
    "The tool manifest is enforced but not signed yet: it's tamper-evident, not tamper-proof.",
  ],
];

/** How each level reads in the table's Coverage column. */
export const COVERAGE_LABEL: Record<CoverageLevel, string> = {
  substantial: "Substantial",
  partial: "In progress",
  na: "Architectural N/A",
};

/** The table caption, counted from the rows so it cannot drift from them.
 * Labels are COVERAGE_LABEL with the first letter lowercased, so the caption and
 * the column always name a level the same way. */
export function coverageSummary(
  rows: readonly (readonly [string, string, CoverageLevel, string])[],
): string {
  const order: CoverageLevel[] = ["substantial", "partial", "na"];
  return order
    .map((level) => [level, rows.filter((r) => r[2] === level).length] as const)
    .filter(([, n]) => n > 0)
    .map(([level, n]) => `${n} ${summaryLabel(level)}`)
    .join(" · ");
}

function summaryLabel(level: CoverageLevel): string {
  return COVERAGE_LABEL[level].replace(/^\S/, (c) => c.toLowerCase());
}
