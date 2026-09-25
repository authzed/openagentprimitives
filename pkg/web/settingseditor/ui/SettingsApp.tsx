import { Tabs, TabsContent, TabsList, TabsTrigger, Alert, AlertDescription } from "@ap/design";
import { useAppState } from "./lib/useAppState";
import { GeneralTab } from "./general/GeneralTab";
import { ModelTab } from "./model/ModelTab";
import { ClusterTab } from "./cluster/ClusterTab";
import { AdvancedTab } from "./advanced/AdvancedTab";
import { AboutTab } from "./about/AboutTab";

export interface SettingsAppProps {
  apiBase: string;
}

// TAB is the fixed set of top-level settings tabs. All five render their
// real components as of Task 12 (Advanced/About).
const TABS = [
  { id: "general", label: "General" },
  { id: "model", label: "Model" },
  { id: "cluster", label: "Cluster" },
  { id: "advanced", label: "Advanced" },
  { id: "about", label: "About" },
] as const;

export function SettingsApp({ apiBase }: SettingsAppProps) {
  const { state, error } = useAppState(apiBase);

  // The shell is BOUNDED to the viewport (h-screen + min-h-0 chain down to
  // each tab): the document itself never scrolls, so a tab's own scroll
  // region is the only thing that moves and anything a tab renders after it
  // (the Cluster/Advanced ActionBar) stays on-screen. Same pattern as
  // adminui's AppShell and the sessions Chrome.
  return (
    <div className="mx-auto flex h-screen max-w-3xl flex-col gap-4 p-6">
      <header className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold">Desktop settings</h1>
        <StatusStrip phase={state?.phase} step={state?.step} />
      </header>

      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <Tabs defaultValue="general" className="flex min-h-0 flex-1 flex-col">
        <TabsList className="shrink-0 self-start">
          {TABS.map((t) => (
            <TabsTrigger key={t.id} value={t.id}>
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>

        {/* General/Model/About manage no footer of their own, so their whole
            panel scrolls; Cluster and Advanced own an internal scroll region
            (with the ActionBar below it) and must NOT be wrapped in another
            scroll container, or the bar would scroll away with the form. */}
        <TabsContent value="general" className="min-h-0 flex-1 overflow-y-auto">
          <GeneralTab
            apiBase={apiBase}
            running={state?.running ?? false}
            kubectlCurrent={state?.kubectlCurrent ?? false}
          />
        </TabsContent>
        <TabsContent value="model" className="min-h-0 flex-1 overflow-y-auto">
          <ModelTab apiBase={apiBase} running={state?.running ?? false} />
        </TabsContent>
        <TabsContent value="cluster" className="min-h-0 flex-1">
          <ClusterTab apiBase={apiBase} running={state?.running ?? false} />
        </TabsContent>
        <TabsContent value="advanced" className="min-h-0 flex-1">
          <AdvancedTab apiBase={apiBase} running={state?.running ?? false} />
        </TabsContent>
        <TabsContent value="about" className="min-h-0 flex-1 overflow-y-auto">
          <AboutTab apiBase={apiBase} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// StatusStrip is a thin phase readout: the phase alone once settled, plus the
// current step while the desktop cluster is starting up.
function StatusStrip({ phase, step }: { phase?: string; step?: string }) {
  if (!phase) return null;
  return (
    <div className="text-sm text-muted-foreground">
      <span className="font-medium text-foreground">{phase}</span>
      {phase === "starting" && step && <span> — {step}</span>}
    </div>
  );
}
