// viewSession.tsx carries the session a rendered page belongs to — supplied
// by the HOST (the view that mounted the page), never by a node. A component
// that must address the session's own routes (ap:attachment's meta lookup and
// download link) reads it here, so a fill can never name a different session
// than the one the viewer is looking at.
import * as React from "react";

export interface ViewSession {
  ns: string;
  name: string;
}

const ViewSessionContext = React.createContext<ViewSession | null>(null);

export function ViewSessionProvider({ value, children }: { value: ViewSession; children: React.ReactNode }): React.ReactElement {
  return <ViewSessionContext.Provider value={value}>{children}</ViewSessionContext.Provider>;
}

// useViewSession returns the host's session, or null when rendered outside any
// view — a standalone render has no session to address.
export function useViewSession(): ViewSession | null {
  return React.useContext(ViewSessionContext);
}
