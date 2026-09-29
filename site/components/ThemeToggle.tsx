"use client";
import { useEffect, useState, type JSX } from "react";
import { useTheme } from "next-themes";
import "./theme-toggle.css";

type Mode = "light" | "dark" | "system";

// Native radios rather than buttons with aria-checked: a radio group gives
// arrow-key navigation, roving focus and the correct grouping semantics for
// free, and the three options are genuinely mutually exclusive.
const MODES: { id: Mode; label: string; icon: JSX.Element }[] = [
    {
    id: "light",
    label: "Light",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="3.1" />
        <path d="M8 1v1.6M8 13.4V15M1 8h1.6M13.4 8H15M3.05 3.05l1.13 1.13M11.82 11.82l1.13 1.13M12.95 3.05l-1.13 1.13M4.18 11.82l-1.13 1.13" />
      </svg>
    ),
  },
  {
    id: "dark",
    label: "Dark",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <path d="M13.5 9.6A5.8 5.8 0 0 1 6.4 2.5a5.8 5.8 0 1 0 7.1 7.1Z" />
      </svg>
    ),
  },
  {
    id: "system",
    label: "System",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <rect x="1.6" y="2.6" width="12.8" height="9" rx="1.2" />
        <path d="M5.6 13.9h4.8" />
      </svg>
    ),
  },
];

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, setTheme } = useTheme();
  // The server cannot know the stored choice, so no radio is checked until
  // mount; rendering one would mismatch hydration.
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);

  return (
    <fieldset className={`theme-toggle${className ? ` ${className}` : ""}`}>
      <legend className="tt-sr">Color theme</legend>
      {MODES.map((mode) => (
        <label key={mode.id} className="tt-opt" title={`${mode.label} theme`}>
          <input
            type="radio"
            name="oap-theme"
            value={mode.id}
            checked={mounted && theme === mode.id}
            onChange={() => setTheme(mode.id)}
          />
          <span className="tt-icon">{mode.icon}</span>
          <span className="tt-sr">{mode.label}</span>
        </label>
      ))}
    </fieldset>
  );
}
