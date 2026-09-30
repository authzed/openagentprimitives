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
    "No goal-lock or plan-divergence detection.",
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
    "Isolation is per session: tool calls within one session share a UID, /proc, /tmp and /work.",
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
    "partial",
    "Delegation stays within one cluster; there is no protocol for agents outside it.",
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

const SUMMARY_LABEL: Record<CoverageLevel, string> = {
  substantial: "substantial",
  partial: "partial",
  na: "architectural N/A",
};

/** The table caption, counted from the rows so it cannot drift from them. */
export function coverageSummary(
  rows: readonly (readonly [string, string, CoverageLevel, string])[],
): string {
  const order: CoverageLevel[] = ["substantial", "partial", "na"];
  return order
    .map((level) => [level, rows.filter((r) => r[2] === level).length] as const)
    .filter(([, n]) => n > 0)
    .map(([level, n]) => `${n} ${SUMMARY_LABEL[level]}`)
    .join(" · ");
}
