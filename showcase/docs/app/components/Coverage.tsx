// A coverage badge for the OWASP page — the four levels the coverage doc uses.
const LABELS: Record<string, string> = {
  substantial: "Substantial",
  partial: "Partial",
  minimal: "Minimal",
  na: "Architectural N/A",
};

export function Coverage({ level }: { level: string }) {
  return (
    <span className={`doc-cov doc-cov--${level}`}>
      {LABELS[level] ?? level}
    </span>
  );
}
