import type { Config } from "tailwindcss";

const preset: Partial<Config> = {
  theme: {
    extend: {
      colors: {
        background: "hsl(var(--background))",
        foreground: "hsl(var(--foreground))",
        card: { DEFAULT: "hsl(var(--card))", foreground: "hsl(var(--card-foreground))" },
        popover: { DEFAULT: "hsl(var(--popover))", foreground: "hsl(var(--popover-foreground))" },
        primary: { DEFAULT: "hsl(var(--primary))", foreground: "hsl(var(--primary-foreground))" },
        secondary: { DEFAULT: "hsl(var(--secondary))", foreground: "hsl(var(--secondary-foreground))" },
        muted: { DEFAULT: "hsl(var(--muted))", foreground: "hsl(var(--muted-foreground))" },
        accent: { DEFAULT: "hsl(var(--accent))", foreground: "hsl(var(--accent-foreground))" },
        destructive: { DEFAULT: "hsl(var(--destructive))", foreground: "hsl(var(--destructive-foreground))" },
        success: "hsl(var(--success))",
        warning: "hsl(var(--warning))",
        link: "hsl(var(--link))",
        // Chart series as utilities (bg-chart-1 …) so hand-drawn bars and dots
        // use the chart palette instead of --primary, which is INK now and reads
        // as a black progress bar in light mode.
        "chart-1": "hsl(var(--chart-1))",
        "chart-2": "hsl(var(--chart-2))",
        "chart-3": "hsl(var(--chart-3))",
        "chart-4": "hsl(var(--chart-4))",
        "chart-5": "hsl(var(--chart-5))",
        state: { DEFAULT: "hsl(var(--state))", foreground: "hsl(var(--state-foreground))" },
        // Surface rungs. Utilities so a component can say bg-surface-2 instead
        // of reaching for an opacity modifier on --muted (which, on a dark
        // ground, moves TOWARD the background and separates nothing).
        "surface-2": "hsl(var(--surface-2))",
        "surface-3": "hsl(var(--surface-3))",
        "surface-hover": "hsl(var(--surface-hover))",
        "surface-active": "hsl(var(--surface-active))",
        border: "hsl(var(--border))",
        input: "hsl(var(--input))",
        ring: "hsl(var(--ring))",
      },
      borderRadius: { lg: "var(--radius)", md: "calc(var(--radius) - 2px)", sm: "calc(var(--radius) - 4px)" },
      fontFamily: {
        sans: ['"Inter Variable"', "system-ui", "sans-serif"],
        mono: ['"JetBrains Mono Variable"', "ui-monospace", "monospace"],
      },
      keyframes: {
        "ap-flow": { "0%": { backgroundPosition: "-50% 0" }, "100%": { backgroundPosition: "150% 0" } },
        "ap-pulse-dot": { "0%,100%": { opacity: ".5", transform: "scale(.85)" }, "50%": { opacity: "1", transform: "scale(1.1)" } },
      },
      animation: {
        "ap-flow": "ap-flow 1.8s linear infinite",
        "ap-pulse-dot": "ap-pulse-dot 1.4s ease-in-out infinite",
      },
    },
  },
};

export default preset;
