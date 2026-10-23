// A coverage badge for the OWASP page — the four levels the coverage doc uses.
export const COVERAGE_LABELS: Record<string, string> = {
  substantial: "Substantial",
  partial: "Partial",
  minimal: "Minimal",
  na: "Architectural N/A",
};

export function Coverage({ level }: { level: string }) {
  return (
    <span className={`doc-cov doc-cov--${level}`}>
      {COVERAGE_LABELS[level] ?? level}
    </span>
  );
}
