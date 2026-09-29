import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { ThemeProvider } from "next-themes";
import { Analytics } from "@/components/Analytics";
import iconDarkInk from "../../docs/assets/brand/oap-icon-dark.svg";
import iconLightInk from "../../docs/assets/brand/oap-icon-light.svg";

export const metadata: Metadata = {
  title: { default: "Open Agent Primitives", template: "%s · OAP docs" },
  description:
    "OAP is a Kubernetes-native runtime for LLM agents. Every state-touching tool call is checked against a SpiceDB relationship graph before it runs, so the model is never asked whether it may act.",
  // The favicon sits on the browser's tab strip, not on this page, so its ink
  // follows the OS scheme rather than the site's toggle.
  icons: {
    icon: [
      { url: iconDarkInk.src, type: "image/svg+xml", media: "(prefers-color-scheme: light)" },
      { url: iconLightInk.src, type: "image/svg+xml", media: "(prefers-color-scheme: dark)" },
    ],
  },
};

export const viewport: Viewport = {
  colorScheme: "dark light",
  themeColor: [
    { media: "(prefers-color-scheme: dark)", color: "#0c050f" },
    { media: "(prefers-color-scheme: light)", color: "#fcfcfc" },
  ],
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    // next-themes sets data-theme on <html> before hydration; this is the one
    // element whose attributes are allowed to differ from the server render.
    <html lang="en" suppressHydrationWarning>
      <body>
        <ThemeProvider attribute="data-theme" defaultTheme="system" enableSystem disableTransitionOnChange>
          {children}
          <Analytics />
        </ThemeProvider>
      </body>
    </html>
  );
}
