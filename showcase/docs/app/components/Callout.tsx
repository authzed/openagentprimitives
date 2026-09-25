import type { ReactNode } from 'react'

const ICON: Record<string, string> = {
  note: 'ℹ️',
  tip: '💡',
  warning: '⚠️',
  security: '🔒',
}

// A callout box. `kind` picks the accent + glyph.
export function Callout({ kind = 'note', title, children }: { kind?: string; title?: string; children: ReactNode }) {
  return (
    <aside className={`doc-callout doc-callout--${kind}`}>
      <div className="doc-callout-head">
        <span className="doc-callout-icon">{ICON[kind] ?? ICON.note}</span>
        {title && <span className="doc-callout-title">{title}</span>}
      </div>
      <div className="doc-callout-body">{children}</div>
    </aside>
  )
}
