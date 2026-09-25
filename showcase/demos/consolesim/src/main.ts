import { installOverlay } from './runtime/overlay'
import { bindStory } from './runtime/story'
import { loadStory } from './scenarios'
import { mountConsole } from './term/console'
import { cyan } from './term/ansi'
import './styles/overlay.css'
import './styles/console.css'

// Entry point. The capture engine loads this page with
//   ?scenario=<name>&theme=<light|dark>
// builds the named terminal story, and drives it through window.__showcase +
// window.__showcaseStory — the same contract slacksim uses.
const params = new URLSearchParams(window.location.search)
const story = loadStory(params.get('scenario'))
const theme = params.get('theme') === 'light' ? 'light' : 'dark'
document.documentElement.dataset.theme = theme

const overlay = installOverlay()
window.__showcase = overlay

const control = mountConsole(document.getElementById('root')!, {
  theme,
  prompt: story.prompt ?? `${cyan('❯')} `,
  cols: story.cols ?? 92,
  rows: story.rows ?? 30,
  title: 'oap — zsh',
  caption: overlay.caption,
})

bindStory(story, control)
