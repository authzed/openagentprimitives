import * as React from "react";
import { applyStoredTheme } from "@ap/design";

// ThemeProvider owns the theme attribute on <html> for every mounted app. The
// design CSS (imported below for its side-effect) carries the tokens for both
// themes; which one applies is the viewer's stored choice (design/theme.ts:
// dark, light, or follow the OS). It used to hardcode "dark" here, which
// silently overrode any earlier application on every mount.
import "@ap/design/styles.css";

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  React.useEffect(() => applyStoredTheme(), []);
  return <>{children}</>;
}
