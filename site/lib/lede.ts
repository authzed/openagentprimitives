// A guide's lede must be a `<div className="doc-lede">`, never a `<p>`. MDX
// wraps multi-line block content — including a top-level lede paragraph — in
// its own `<p>`; a `<p>` nested inside that `<p>` is invalid HTML, so the
// browser re-parents the inner tag out of the outer one at parse time. The
// server-rendered markup and the client's re-parsed DOM then disagree, and
// React throws hydration error #418 — but only in production, where minified
// error messages hide the mismatch and dev's verbose warning doesn't fire the
// same way. See site/AGENTS.md for the authoring rule this enforces.
const LEDE_P_TAG = /<p\s+className="doc-lede"/;

export interface LedeProblem {
  file: string;
}

export function checkLedeTags(
  files: { file: string; src: string }[],
): LedeProblem[] {
  const problems: LedeProblem[] = [];
  for (const { file, src } of files) {
    if (LEDE_P_TAG.test(src)) problems.push({ file });
  }
  return problems;
}
