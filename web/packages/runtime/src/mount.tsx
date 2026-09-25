import * as React from "react";
import { createRoot } from "react-dom/client";
import { ThemeProvider } from "./theme";
import { readBootstrap } from "./bootstrap";

// mount renders App into #root, wrapped in ThemeProvider, with the server-
// injected bootstrap props. It replaces the server-rendered fallback markup.
// P is constrained to `object` (not Record<string, unknown>) so plugin authors
// can type their props as an interface — interfaces lack the implicit index
// signature Record<string, unknown> would require.
export function mount<P extends object>(
  App: React.ComponentType<P>,
): void {
  const el = document.getElementById("root");
  if (!el) return;
  const props = readBootstrap<P>();
  createRoot(el).render(
    <React.StrictMode>
      <ThemeProvider>
        <App {...props} />
      </ThemeProvider>
    </React.StrictMode>,
  );
}
