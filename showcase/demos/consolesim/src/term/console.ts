import { Terminal, type ITheme } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import type { TermControl } from '../runtime/story'

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms))

// Terminal palettes. Background/foreground drive the window; the 16 ANSI colors
// are what the scenarios' color helpers resolve to.
const DARK: ITheme = {
  background: '#0c0e14',
  foreground: '#d7dae0',
  cursor: '#7dcfff',
  black: '#15161e',
  red: '#f7768e',
  green: '#4fd6a6',
  yellow: '#e0af68',
  blue: '#7aa2f7',
  magenta: '#bb9af7',
  cyan: '#7dcfff',
  white: '#a9b1d6',
  brightBlack: '#565f89',
  brightGreen: '#73daca',
  brightYellow: '#ffc777',
  brightCyan: '#b4f9f8',
  brightWhite: '#e6e8ef',
}
const LIGHT: ITheme = {
  background: '#fbfbfa',
  foreground: '#24292f',
  cursor: '#0969da',
  black: '#24292f',
  red: '#cf222e',
  green: '#1a7f37',
  yellow: '#9a6700',
  blue: '#0969da',
  magenta: '#8250df',
  cyan: '#1b7c83',
  white: '#6e7781',
  brightBlack: '#8c959f',
  brightGreen: '#1a7f37',
  brightYellow: '#9a6700',
  brightCyan: '#1b7c83',
  brightWhite: '#57606a',
}

export interface ConsoleOpts {
  theme: 'dark' | 'light'
  prompt: string
  cols: number
  rows: number
  title: string
  caption: (text: string | null) => void
}

// mountConsole builds a terminal-window (title bar + xterm) inside root and
// returns the async TermControl a story drives it with.
export function mountConsole(root: HTMLElement, opts: ConsoleOpts): TermControl {
  const win = document.createElement('div')
  win.className = 'cs-window'
  win.innerHTML = `
    <div class="cs-titlebar">
      <span class="cs-dots"><i class="cs-dot cs-red"></i><i class="cs-dot cs-yellow"></i><i class="cs-dot cs-green"></i></span>
      <span class="cs-title">${opts.title}</span>
    </div>
    <div class="cs-body"></div>`
  root.appendChild(win)

  const term = new Terminal({
    theme: opts.theme === 'light' ? LIGHT : DARK,
    fontFamily: '"JetBrains Mono", "SF Mono", Menlo, Consolas, monospace',
    fontSize: 14,
    lineHeight: 1.4,
    letterSpacing: 0,
    cols: opts.cols,
    rows: opts.rows,
    cursorBlink: true,
    cursorStyle: 'bar',
    convertEol: true, // a bare \n moves to column 0 of the next line
    disableStdin: true,
    scrollback: 0,
  })
  term.open(win.querySelector('.cs-body') as HTMLElement)

  // term.write is callback-based; wrap it so a beat can await each chunk.
  const put = (s: string) => new Promise<void>((res) => term.write(s, res))

  return {
    caption: opts.caption,
    prompt: () => put(opts.prompt),
    async type(text, o) {
      const delay = 1000 / (o?.cps ?? 24) // deterministic keystroke cadence
      for (const ch of text) {
        await put(ch)
        await sleep(delay)
      }
    },
    write: (text) => put(text),
    line: (text = '') => put(text + '\n'),
    enter: () => put('\n'),
    wait: (ms) => sleep(ms),
    async clear() {
      term.clear()
      await put('')
    },
  }
}
