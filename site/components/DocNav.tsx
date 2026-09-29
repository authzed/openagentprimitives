"use client";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { usePathname } from "next/navigation";

/* The docs sidebar. On a wide screen it is the full-height column; under
 * 900px it collapses to a top bar (brand, search, a Menu button) and the guide
 * list opens as an overlay beneath it. Only the open/closed state lives here;
 * the brand, search and guide list are rendered by the server layout and
 * passed in, so there is still exactly one search box on the page. */
export function DocNav({
  header,
  children,
}: {
  header: ReactNode;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const toggle = useRef<HTMLButtonElement>(null);
  const pathname = usePathname();

  // Choosing a guide navigates; the menu should not stay over the new page.
  useEffect(() => {
    setOpen(false);
  }, [pathname]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      toggle.current?.focus();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <aside className="doc-nav" data-open={open} data-pagefind-ignore>
      <div className="doc-nav-bar">
        {header}
        <button
          ref={toggle}
          type="button"
          className="doc-nav-toggle"
          aria-expanded={open}
          aria-controls="doc-nav-list"
          onClick={() => setOpen((o) => !o)}
        >
          {open ? "Close" : "Menu"}
        </button>
      </div>
      <nav id="doc-nav-list" className="doc-nav-list" aria-label="Guides">
        {children}
      </nav>
    </aside>
  );
}
