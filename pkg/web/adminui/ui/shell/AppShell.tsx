import * as React from "react";
import { ChevronDown, LogOut, Moon, Sun } from "lucide-react";
import { OapMarkRelay, readThemeChoice, resolveTheme, setThemeChoice, type ThemeChoice } from "@ap/design";
import { NAV, VIEW_META, type ViewId } from "./nav";
import { ClusterBadge } from "./ClusterBadge";

interface AppShellProps {
  view: ViewId;
  onNavigate: (v: ViewId) => void;
  // role is still carried on the wire (the backend tags every admin "platform
  // admin") but the header no longer renders it — every user here is a platform
  // admin, so the label was pure noise. Kept optional so callers may still pass it.
  currentUser?: { email: string; role?: string };
  // apiBase ("/admin/api") lets the header's ClusterBadge fetch cluster
  // identity; the badge degrades quietly when the fetch fails.
  apiBase?: string;
  onSignOut?: () => void;
  children: React.ReactNode;
}

const DEFAULT_USER = { email: "admin@example.com" };

// defaultSignOut hits the server logout endpoint (/admin/logout), which expires
// the HttpOnly idd_session cookie JS cannot clear and redirects to re-login. A
// GET navigation (not a fetch) so the browser follows the server's redirect.
export function defaultSignOut(): void {
  if (typeof window === "undefined") return;
  window.location.assign("/admin/logout");
}

export function AppShell({
  view,
  onNavigate,
  currentUser = DEFAULT_USER,
  apiBase = "/admin/api",
  onSignOut = defaultSignOut,
  children,
}: AppShellProps) {
  const meta = VIEW_META[view];
  const [menuOpen, setMenuOpen] = React.useState(false);
  const [brandHover, setBrandHover] = React.useState(false);
  const [themeChoice, setThemeChoiceState] = React.useState<ThemeChoice>(() => readThemeChoice());
  const menuRef = React.useRef<HTMLDivElement>(null);

  // Close the user menu on an outside click or Escape.
  React.useEffect(() => {
    if (!menuOpen) return;
    function onDown(e: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setMenuOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setMenuOpen(false);
    }
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [menuOpen]);
  return (
    <div className="flex h-screen flex-col bg-background text-foreground">
      {/* ── Top header: brand left · current user right ── */}
      <header className="flex shrink-0 items-center border-b bg-card/30 px-5 py-2.5">
        <div
          className="mr-auto flex items-center gap-2.5"
          onPointerEnter={() => setBrandHover(true)}
          onPointerLeave={() => setBrandHover(false)}
        >
          {/* Brand: the OAP mark in the same ink as the name, no tinted tile —
              a filled mark on a filled tile is two shapes fighting for one slot.
              Hovering the brand runs the relay band around the mark; idle is the
              plain mark (motion for indication only). */}
          {/* The band is 12 mark units by default (3.5% of the width), which is
              0.7px on this 20px mark: sub-pixel. Small marks need a floor, so
              the header runs a 32-unit band (~1.9px) and a slightly longer one. */}
          <OapMarkRelay active={brandHover} bandWidth={32} band={14} className="h-5 w-5 text-foreground" />
          <span className="text-sm font-semibold">Open Agent Primitives</span>
          <ClusterBadge apiBase={apiBase} />
        </div>
        {/* Theme toggle: one click flips light/dark from whatever is showing
            now (a "system" viewer gets the opposite of what the OS resolved to,
            stored as an explicit choice). The 3-way choice including "system"
            lives in the user menu. */}
        <button
          type="button"
          aria-label={resolveTheme(themeChoice) === "dark" ? "Switch to light theme" : "Switch to dark theme"}
          title={resolveTheme(themeChoice) === "dark" ? "Light theme" : "Dark theme"}
          onClick={() => {
            const next: ThemeChoice = resolveTheme(themeChoice) === "dark" ? "light" : "dark";
            setThemeChoice(next);
            setThemeChoiceState(next);
          }}
          className="mr-2 grid h-7 w-7 place-items-center rounded-md border text-muted-foreground transition-colors hover:bg-surface-2 hover:text-foreground"
        >
          {resolveTheme(themeChoice) === "dark" ? <Sun className="h-3.5 w-3.5" aria-hidden="true" /> : <Moon className="h-3.5 w-3.5" aria-hidden="true" />}
        </button>
        {/* Current user chip → click-to-open menu (Sign Out) */}
        <div ref={menuRef} className="relative">
          <button
            type="button"
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            aria-label="User menu"
            onClick={() => setMenuOpen((o) => !o)}
            className="flex items-center gap-2 rounded-md border bg-card px-2 py-1 transition-colors hover:bg-accent/50"
          >
            {/* Avatar: the account's initial on a raised stone surface. There is
                no profile picture and no settings surface to set one, so this
                is the permanent face of the account and has to read well on
                its own: a letter beats a gradient blob that changed meaning
                every time the palette moved. */}
            <div
              aria-hidden="true"
              className="grid h-6 w-6 shrink-0 place-items-center rounded-full border border-border bg-surface-3 font-mono text-[11px] font-medium uppercase leading-none text-foreground"
            >
              {(currentUser.email.trim()[0] || "?")}
            </div>
            <div className="text-left leading-tight">
              <div className="font-mono text-xs">{currentUser.email}</div>
            </div>
            <ChevronDown
              className={`h-3.5 w-3.5 text-muted-foreground transition-transform ${menuOpen ? "rotate-180" : ""}`}
            />
          </button>
          {menuOpen && (
            <div
              role="menu"
              className="absolute right-0 z-50 mt-1 w-44 overflow-hidden rounded-md border bg-card shadow-lg"
            >
              {/* Appearance lives in the user menu because it is the only
                  per-person control the admin UI has today; it is stored per
                  browser (design/theme.ts), not on the account. */}
              <div className="px-3 pt-2 pb-1 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">Appearance</div>
              <div role="group" aria-label="Appearance" className="flex gap-1 px-2 pb-2">
                {(["dark", "light", "system"] as const).map((c) => (
                  <button
                    key={c}
                    type="button"
                    role="menuitemradio"
                    aria-checked={themeChoice === c}
                    onClick={() => { setThemeChoice(c); setThemeChoiceState(c); }}
                    className={[
                      "flex-1 rounded px-2 py-1 text-xs capitalize transition-colors",
                      themeChoice === c ? "bg-surface-active text-foreground" : "text-muted-foreground hover:bg-surface-2 hover:text-foreground",
                    ].join(" ")}
                  >
                    {c}
                  </button>
                ))}
              </div>
              <div className="border-t" />
              <button
                type="button"
                role="menuitem"
                onClick={() => { setMenuOpen(false); onSignOut(); }}
                className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-foreground transition-colors hover:bg-accent"
              >
                <LogOut aria-hidden="true" className="h-4 w-4 text-muted-foreground" />
                Sign Out
              </button>
            </div>
          )}
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* ── Left sidebar ── */}
        <aside className="flex w-48 shrink-0 flex-col border-r bg-card/20 lg:w-60">
          <nav className="flex-1 overflow-y-auto px-2 py-1">
            {NAV.map((group) => (
              <div key={group.group || "__root__"}>
                {group.group && (
                  <p className="mt-3 mb-0.5 px-2 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
                    {group.group}
                  </p>
                )}
                {group.items.map((item) => {
                  const Icon = item.icon;
                  const active = view === item.id;
                  return (
                    <button
                      key={item.id}
                      type="button"
                      aria-current={active ? "page" : undefined}
                      onClick={() => onNavigate(item.id)}
                      className={[
                        "flex w-full items-center gap-2 py-1.5 text-left text-sm transition-colors",
                        // Selected = a raised surface plus a 3px accent rule, full-bleed
                        // to the sidebar border (negative margin eats the nav padding,
                        // left corners squared so the rule meets the border). Hover is
                        // one surface rung, not an opacity modifier on --accent, which
                        // on this ground composited to almost nothing. The old
                        // `bg-accent text-primary` pill was the only saturated block on
                        // the page and read as a button, not "you are here".
                        active
                          ? "-ml-2 rounded-r-md bg-surface-active pl-4 pr-2 font-medium text-foreground shadow-[inset_3px_0_0_hsl(var(--state))]"
                          : "rounded-md px-2 text-foreground/80 hover:bg-surface-2 hover:text-foreground",
                      ].join(" ")}
                    >
                      <Icon className="h-4 w-4 shrink-0" />
                      {item.label}
                    </button>
                  );
                })}
              </div>
            ))}
          </nav>
        </aside>

        {/* ── Main content area ── */}
        <div className="flex min-w-0 flex-1 flex-col">
          <header className="flex shrink-0 items-center justify-between border-b bg-card/10 px-6 py-3.5">
            <div>
              <h1 className="text-base font-semibold">{meta.title}</h1>
              <p className="text-xs text-muted-foreground">{meta.sub}</p>
            </div>
          </header>
          <main className="flex-1 overflow-y-auto px-4 py-4 lg:px-6 lg:py-5">{children}</main>
        </div>
      </div>
    </div>
  );
}
