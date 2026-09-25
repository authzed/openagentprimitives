import { useEffect, useState } from 'react'
import { useLookups, useScenario, useStore } from './runtime/context'
import { WorkspaceRail } from './chrome/WorkspaceRail'
import { Sidebar } from './chrome/Sidebar'
import { MessagePane } from './chrome/MessagePane'
import { AppHome } from './chrome/AppHome'
import { ThreadPanel } from './chrome/ThreadPanel'

export function App({ initialTheme = 'dark' }: { initialTheme?: 'light' | 'dark' }) {
  const scenario = useScenario()
  const store = useStore()
  const { channel } = useLookups()
  const [theme, setTheme] = useState<'light' | 'dark'>(initialTheme)

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])

  const active = channel(scenario.view.activeChannelId)
  const showHome = active?.kind === 'app' && scenario.view.appHomeChannelId === active.id

  return (
    <div className="sk-app">
      <WorkspaceRail theme={theme} onToggleTheme={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))} />
      <Sidebar />
      <div className="sk-main">
        {active ? (
          showHome ? (
            <AppHome channel={active} />
          ) : (
            <MessagePane channel={active} />
          )
        ) : (
          <div className="sk-empty">No conversation selected.</div>
        )}
        {scenario.view.openThreadTs && !showHome && (
          <ThreadPanel parentTs={scenario.view.openThreadTs} onClose={() => store.closeThread()} />
        )}
      </div>
    </div>
  )
}
