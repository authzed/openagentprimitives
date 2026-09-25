// Minimal ANSI SGR helpers for authoring colored terminal output. xterm.js
// renders these escape codes natively, so a scenario writes `green('✓ done')`
// and gets real terminal color — the same bytes the real CLI would emit.
const ESC = '\x1b['
const sgr =
  (open: number) =>
  (s: string): string =>
    `${ESC}${open}m${s}${ESC}0m`

export const bold = sgr(1)
export const dim = sgr(2)
export const red = sgr(31)
export const green = sgr(32)
export const yellow = sgr(33)
export const blue = sgr(34)
export const magenta = sgr(35)
export const cyan = sgr(36)
export const gray = sgr(90)

// Common CLI glyphs, pre-colored.
export const ok = green('✓')
export const warnGlyph = yellow('⚠')
export const failGlyph = red('✗')
export const step = cyan('▸')
