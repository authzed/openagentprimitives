// DetailPage is the shared shell every Phase C detail page (Session, Agent,
// Tool, …) renders inside: a back affordance, a title/subtitle, an optional
// header-right slot, a horizontal tab bar, and the active tab's content. Purely
// presentational — the parent owns tab state and routing (via navigate).
import type { ReactNode } from "react";
import { ArrowLeft } from "lucide-react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@ap/design";
import { navigate, type Route } from "../lib/router";

export interface DetailTab {
  id: string;
  label: string;
}

export interface DetailPageProps {
  title: ReactNode;
  subtitle?: ReactNode;
  backLabel?: string;
  // backRoute is the parent list to return to (e.g. the agents view). When set,
  // the back button navigates there; otherwise it falls back to history.back().
  backRoute?: Route;
  tabs: DetailTab[];
  activeTab: string;
  onTab: (id: string) => void;
  children: ReactNode;
  headerRight?: ReactNode;
}

export function DetailPage({
  title,
  subtitle,
  backLabel = "Back",
  backRoute,
  tabs,
  activeTab,
  onTab,
  children,
  headerRight,
}: DetailPageProps) {
  return (
    <div className="flex flex-col gap-4">
      <div>
        <button
          type="button"
          onClick={() => {
            if (backRoute) {
              navigate(backRoute);
            } else if (typeof window !== "undefined") {
              window.history.back();
            }
          }}
          className="mb-2 inline-flex items-center gap-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft className="h-3.5 w-3.5" />
          {backLabel}
        </button>
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <h2 className="truncate text-lg font-semibold">{title}</h2>
            {subtitle && <p className="text-sm text-muted-foreground">{subtitle}</p>}
          </div>
          {headerRight && <div className="shrink-0">{headerRight}</div>}
        </div>
      </div>

      <Tabs value={activeTab} onValueChange={onTab}>
        <TabsList>
          {tabs.map((t) => (
            <TabsTrigger key={t.id} value={t.id}>
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>
        {/* A force-mounted panel per tab so every trigger's aria-controls
            resolves to a real element (Radix unmounts inactive content by
            default, which is what left the dangling reference A2 flagged);
            inactive panels stay in the DOM but hidden, and only the active tab
            receives `children`, which the parent swaps as the route ?tab=
            changes. */}
        {tabs.map((t) => (
          <TabsContent key={t.id} value={t.id} className="mt-4" forceMount hidden={t.id !== activeTab}>
            {t.id === activeTab ? children : null}
          </TabsContent>
        ))}
      </Tabs>
    </div>
  );
}
