import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './styles.css'
import iconDarkInkUrl from '../../../docs/assets/brand/oap-icon-dark.svg'
import iconLightInkUrl from '../../../docs/assets/brand/oap-icon-light.svg'

// Favicons are added here rather than in index.html because the brand files
// sit outside the Vite root: an HTML href to them is rewritten at build time
// only, while an import resolves in dev too. Two icons, not one: the favicon
// sits on the browser's tab strip, not on this page, so its ink follows the OS
// colour scheme.
for (const [href, scheme] of [
  [iconDarkInkUrl, 'light'],
  [iconLightInkUrl, 'dark'],
] as const) {
  const link = document.createElement('link')
  link.rel = 'icon'
  link.type = 'image/svg+xml'
  link.href = href
  link.media = `(prefers-color-scheme: ${scheme})`
  document.head.appendChild(link)
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
