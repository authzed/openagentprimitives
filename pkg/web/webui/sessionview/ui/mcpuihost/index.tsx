// Entry for the sandbox-origin MCP-UI widget renderer (Plan 4 Task 3). Mounts
// McpUiHost with the props widgets.go's mcpuiHostPage embedded into
// <script type="application/json" id="ap-bootstrap">.
//
// Deliberately does NOT use @ap/runtime's mount()/ThemeProvider: that helper
// side-effect-imports the full @ap/design stylesheet and hardcodes the
// trusted shell's dark design theme — both wrong here. This bundle runs
// under widgets.go's restrictive, widget-scoped CSP (buildWidgetCSP), which
// has no allowance for the design system's external font/style needs, and
// the whole point of usePreferredTheme (McpUiHost.tsx) is to reflect the
// OS/browser's actual color-scheme preference, not the framework's fixed
// theme. So readBootstrap is reimplemented locally instead — the same
// element id + parse-defensively shape as @ap/runtime's readBootstrap, just
// without importing the package that would drag the design system in with it
// (mirrors artifactview/ui/host/index.tsx's own reason for staying off
// @ap/runtime).
import { createRoot } from "react-dom/client";
import { McpUiHost } from "./McpUiHost";

interface Bootstrap {
  uri?: string;
  html?: string;
  trustedOrigin?: string;
}

function readBootstrap(): Bootstrap {
  const el = document.getElementById("ap-bootstrap");
  if (!el || !el.textContent) return {};
  try {
    return JSON.parse(el.textContent) as Bootstrap;
  } catch {
    return {};
  }
}

const root = document.getElementById("root");
if (root) {
  const { uri, html, trustedOrigin } = readBootstrap();
  createRoot(root).render(<McpUiHost uri={uri ?? ""} html={html ?? ""} trustedOrigin={trustedOrigin ?? ""} />);
}
