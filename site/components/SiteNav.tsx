"use client";
import {
  useEffect,
  useId,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { useTheme } from "next-themes";
import { Wordmark } from "./Wordmark";
import { OAP_MARK_PATHS } from "./OapMark";
import { THEME_MODES, type ThemeMode } from "./ThemeToggle";
import { DiscordIcon, GitHubIcon } from "./BrandIcons";
import { DISCORD_URL, REPO_URL } from "@/lib/site";

/* The top bar shared by the landing page and the stories. Its styles live in
 * app/(landing)/landing.css (.lp-nav*), so a page that renders it imports that
 * stylesheet and sits inside an .lp frame. Each page passes its own text links
 * and primary action; the brand, the GitHub and Discord icons and the theme
 * menu are the same everywhere. */
export function SiteNav({
  children,
  cta,
}: {
  children: ReactNode;
  cta: { href: string; label: ReactNode };
}) {
  return (
    <nav className="lp-nav">
      <a
        className="lp-nav-brand lp-relay-host"
        href="/"
        aria-label="Open Agent Primitives home"
      >
        <span className="lp-nav-mark">
          <Wordmark className="lp-nav-wordmark" />
          <MarkRelay />
        </span>
      </a>
      <div className="lp-nav-links">
        {children}
        <a className="lp-nav-icon" href={REPO_URL} aria-label="GitHub">
          <GitHubIcon />
        </a>
        <a className="lp-nav-icon" href={DISCORD_URL} aria-label="Discord">
          <DiscordIcon />
        </a>
        <ThemeMenu />
        <a className="lp-btn lp-btn--primary" href={cta.href}>
          {cta.label}
        </a>
      </div>
    </nav>
  );
}

/* The relay band: the mark's own outline used as a clip over a moving
 * gradient of the five hues. It sits on top of a plain mark and fades in only
 * while its .lp-relay-host is hovered or focused (landing.css). */
const RELAY_STOPS: (0 | 1 | 2 | 3 | 4)[] = [0, 1, 2, 3, 4, 3, 2, 1, 0];

export function MarkRelay({ className }: { className?: string }) {
  const id = useId();
  const clip = `${id}-clip`;
  const band = `${id}-band`;
  return (
    <svg
      className={className ? `lp-mark-relay ${className}` : "lp-mark-relay"}
      viewBox="0 0 1000 393"
      aria-hidden="true"
    >
      <defs>
        <clipPath id={clip}>
          {OAP_MARK_PATHS.map((d) => (
            <path key={d} d={d} />
          ))}
        </clipPath>
        <linearGradient
          id={band}
          gradientUnits="userSpaceOnUse"
          x1="0"
          y1="0"
          x2="3200"
          y2="0"
          spreadMethod="repeat"
        >
          {RELAY_STOPS.map((h, i) => (
            <stop
              key={i}
              offset={i / (RELAY_STOPS.length - 1)}
              style={{ stopColor: `var(--lp-h${h}, var(--lp-state))` }}
            />
          ))}
        </linearGradient>
      </defs>
      <g clipPath={`url(#${clip})`}>
        <rect
          className="lp-mark-band"
          width="4400"
          height="393"
          fill={`url(#${band})`}
        />
      </g>
    </svg>
  );
}

/* The nav's theme control: one button whose icon shows the current choice,
 * opening a three-item menu. It reads and writes the same next-themes state as
 * the footer's ThemeToggle, so the two always agree. */
function ThemeMenu() {
  const { theme, setTheme } = useTheme();
  // The server cannot know the stored choice, so the button shows System
  // until mount; rendering the real one would mismatch hydration.
  const [mounted, setMounted] = useState(false);
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const items = useRef<(HTMLButtonElement | null)[]>([]);
  useEffect(() => setMounted(true), []);

  const mode: ThemeMode = mounted
    ? (THEME_MODES.find((m) => m.id === theme)?.id ?? "system")
    : "system";
  const current = THEME_MODES.find((m) => m.id === mode);

  useEffect(() => {
    if (!open) return;
    items.current[THEME_MODES.findIndex((m) => m.id === mode)]?.focus();
    const onPointerDown = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      button.current?.focus();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
    // Focus moves into the menu once, when it opens; not on every mode change.
  }, [open]);

  const onMenuKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = TAB_STEP[e.key];
    if (!step) return;
    e.preventDefault();
    const at = items.current.findIndex((el) => el === document.activeElement);
    const next = (at + step + THEME_MODES.length) % THEME_MODES.length;
    items.current[next]?.focus();
  };

  return (
    <div className="theme-menu lp-nav-theme" ref={root}>
      <button
        ref={button}
        type="button"
        className="tm-btn"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Color theme: ${current?.label ?? "System"}`}
        title="Color theme"
        onClick={() => setOpen((o) => !o)}
      >
        {current?.icon}
      </button>
      {open && (
        <div
          className="tm-list"
          role="menu"
          aria-label="Color theme"
          onKeyDown={onMenuKeyDown}
        >
          {THEME_MODES.map((m, i) => (
            <button
              key={m.id}
              ref={(el) => {
                items.current[i] = el;
              }}
              type="button"
              className="tm-item"
              role="menuitemradio"
              aria-checked={m.id === mode}
              onClick={() => {
                setTheme(m.id);
                setOpen(false);
                button.current?.focus();
              }}
            >
              {m.icon}
              {m.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/* Arrow keys move one tab either way, whichever way the list is laid out. */
export const TAB_STEP: Record<string, number> = {
  ArrowDown: 1,
  ArrowRight: 1,
  ArrowUp: -1,
  ArrowLeft: -1,
};
