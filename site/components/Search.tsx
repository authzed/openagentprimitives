"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import {
  isApplePlatform,
  isSearchShortcut,
  platformName,
  searchShortcutLabel,
} from "@/lib/shortcut";
import {
  loadPagefind,
  toPath,
  type Pagefind,
  type PagefindResult,
} from "@/lib/pagefind";

type State =
  | { kind: "idle" }
  | { kind: "unavailable" }
  | { kind: "error" }
  | { kind: "results"; items: PagefindResult[] };

// The index only exists after `pnpm build`, so on a dev server "unavailable"
// has a known fix; on a deployed site it means something failed to load.
const UNAVAILABLE =
  process.env.NODE_ENV === "production" ? (
    "Search is unavailable right now."
  ) : (
    <>
      Search is available after <code>pnpm build</code>.
    </>
  );

/** What the hidden status line announces for each state. */
function announce(state: State): string {
  switch (state.kind) {
    case "idle":
      return "";
    case "unavailable":
      return "Search is unavailable.";
    case "error":
      return "Search failed. Try again.";
    case "results":
      return state.items.length === 0
        ? "No matches."
        : `${state.items.length} result${state.items.length === 1 ? "" : "s"}.`;
  }
}

export function Search() {
  const pf = useRef<Pagefind | null | undefined>(undefined);
  const inputRef = useRef<HTMLInputElement>(null);
  // Bumped at the start of each onInput call; a stale (slower) call's result
  // is dropped instead of overwriting a newer call's state.
  const gen = useRef(0);
  const [state, setState] = useState<State>({ kind: "idle" });
  // Unknown until mount: the server can't know the visitor's platform, so the
  // shortcut hint renders only on the client and never mismatches hydration.
  const [apple, setApple] = useState<boolean | null>(null);
  const pathname = usePathname();

  // ⌘K / Ctrl K focuses search from anywhere on a docs page.
  useEffect(() => {
    const nav = navigator as Navigator & {
      userAgentData?: { platform?: string };
    };
    const isApple = isApplePlatform(
      platformName(nav.userAgentData?.platform, nav.platform),
    );
    setApple(isApple);
    const onKey = (e: KeyboardEvent) => {
      if (!isSearchShortcut(e, isApple)) return;
      e.preventDefault();
      inputRef.current?.focus();
      inputRef.current?.select();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Escape closes the search: clear it and give focus back to the page.
  function onKeyDown(e: ReactKeyboardEvent<HTMLInputElement>) {
    if (e.key !== "Escape") return;
    gen.current++;
    setState({ kind: "idle" });
    e.currentTarget.value = "";
    e.currentTarget.blur();
  }

  // Search lives in the docs layout, which persists across navigations.
  // Picking a result routes to a new page without remounting this
  // component, so without this the query and result list would still be
  // showing over the page you just navigated to. Bump the generation first
  // so a search already in flight can't repopulate state after we clear it.
  useEffect(() => {
    gen.current++;
    setState({ kind: "idle" });
    if (inputRef.current) inputRef.current.value = "";
  }, [pathname]);

  async function ensure() {
    if (pf.current === undefined) pf.current = await loadPagefind();
    if (pf.current === null) setState({ kind: "unavailable" });
    return pf.current;
  }

  async function onInput(q: string) {
    const my = ++gen.current;
    const engine = await ensure();
    if (!engine) return;
    if (!q.trim()) {
      if (my === gen.current) setState({ kind: "idle" });
      return;
    }
    try {
      const { results } = await engine.search(q);
      const items = await Promise.all(results.slice(0, 8).map((r) => r.data()));
      if (my === gen.current) setState({ kind: "results", items });
    } catch (err) {
      console.error("docs search: query failed", { query: q, err });
      if (my === gen.current) setState({ kind: "error" });
    }
  }

  return (
    <div className="doc-search" role="search">
      <div className="doc-search-field">
        <input
          ref={inputRef}
          className="doc-search-input"
          type="search"
          placeholder="Search docs"
          aria-label="Search docs"
          aria-keyshortcuts={
            apple === null ? undefined : apple ? "Meta+K" : "Control+K"
          }
          onFocus={ensure}
          onKeyDown={onKeyDown}
          onChange={(e) => void onInput(e.target.value)}
        />
        {apple !== null && (
          <kbd className="doc-search-kbd" aria-hidden="true">
            {searchShortcutLabel(apple)}
          </kbd>
        )}
      </div>
      <p className="doc-sr" role="status">
        {announce(state)}
      </p>
      {state.kind === "unavailable" && (
        <p className="doc-search-note">{UNAVAILABLE}</p>
      )}
      {state.kind === "error" && (
        <p className="doc-search-note">Search failed. Try again.</p>
      )}
      {state.kind === "results" && (
        <ul className="doc-search-results">
          {state.items.length === 0 && (
            <li className="doc-search-note">No matches.</li>
          )}
          {state.items.map((r) => (
            <li key={r.url}>
              <Link href={toPath(r.url)}>{r.meta.title ?? toPath(r.url)}</Link>
              {/* Pagefind's excerpt is its own escaped text with <mark> highlights. */}
              <p dangerouslySetInnerHTML={{ __html: r.excerpt }} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
