"use client";
import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { chatLinks, markdownPath } from "@/lib/copy-page";
import { SITE_URL } from "@/lib/site";
import "./copy-page.css";

type CopyState = "idle" | "copied" | "failed";

// How long "Copied" / "Copy failed" shows before the button resets.
const FEEDBACK_MS = 2000;

const ARROW_STEP: Record<string, number> = { ArrowDown: 1, ArrowUp: -1 };

// Copies the guide's Markdown rendition. The text is fetched after the click,
// and Safari drops the click's permission to write the clipboard across an
// await, so where ClipboardItem exists the fetch goes INTO the clipboard write
// as a promise rather than ahead of it.
async function copyMarkdown(path: string): Promise<void> {
  const text = fetch(path).then((res) => {
    if (!res.ok) throw new Error(`GET ${path}: ${res.status}`);
    return res.text();
  });
  if (typeof ClipboardItem !== "undefined" && navigator.clipboard?.write) {
    const blob = text.then((t) => new Blob([t], { type: "text/plain" }));
    await navigator.clipboard.write([
      new ClipboardItem({ "text/plain": blob }),
    ]);
    return;
  }
  await navigator.clipboard.writeText(await text);
}

/* The docs page's "Copy page" split button: the main half copies the page as
 * Markdown; the chevron opens a menu that also views the Markdown or opens
 * the page in an AI assistant. */
export function CopyPage({ slug }: { slug: string }) {
  const [state, setState] = useState<CopyState>("idle");
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const toggle = useRef<HTMLButtonElement>(null);
  const items = useRef<(HTMLElement | null)[]>([]);

  const path = markdownPath(slug);
  const links = chatLinks(SITE_URL + path);

  useEffect(() => {
    if (state === "idle") return;
    const t = setTimeout(() => setState("idle"), FEEDBACK_MS);
    return () => clearTimeout(t);
  }, [state]);

  useEffect(() => {
    if (!open) return;
    items.current[0]?.focus();
    const onPointerDown = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      toggle.current?.focus();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  const copy = async () => {
    setOpen(false);
    try {
      await copyMarkdown(path);
      setState("copied");
    } catch (err) {
      console.error("docs copy page: copy failed", { path, err });
      setState("failed");
    }
  };

  const onMenuKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = ARROW_STEP[e.key];
    if (!step) return;
    e.preventDefault();
    const n = items.current.length;
    const at = items.current.findIndex((el) => el === document.activeElement);
    items.current[(at + step + n) % n]?.focus();
  };

  const label =
    state === "copied"
      ? "Copied"
      : state === "failed"
        ? "Copy failed"
        : "Copy page";

  const item = (i: number) => (el: HTMLElement | null) => {
    items.current[i] = el;
  };

  return (
    <div className="copy-page" ref={root} data-pagefind-ignore>
      <div className="copy-page-split">
        <button type="button" className="copy-page-main" onClick={copy}>
          {state === "copied" ? <CheckIcon /> : <CopyIcon />}
          <span aria-live="polite">{label}</span>
        </button>
        <button
          ref={toggle}
          type="button"
          className="copy-page-toggle"
          aria-haspopup="menu"
          aria-expanded={open}
          aria-label="More ways to use this page"
          onClick={() => setOpen((o) => !o)}
        >
          <ChevronIcon />
        </button>
      </div>
      {open && (
        <div
          className="copy-page-menu"
          role="menu"
          aria-label="Use this page"
          onKeyDown={onMenuKeyDown}
        >
          <button
            ref={item(0)}
            type="button"
            role="menuitem"
            className="copy-page-item"
            onClick={copy}
          >
            <MenuRow icon={<CopyIcon />} title="Copy page">
              Copy this page as Markdown for LLMs
            </MenuRow>
          </button>
          <a
            ref={item(1)}
            role="menuitem"
            className="copy-page-item"
            href={path}
            type="text/markdown"
            onClick={() => setOpen(false)}
          >
            <MenuRow icon={<DocIcon />} title="View as Markdown">
              Open this page as plain text
            </MenuRow>
          </a>
          {links.map((l, i) => (
            <a
              key={l.id}
              ref={item(i + 2)}
              role="menuitem"
              className="copy-page-item"
              href={l.href}
              target="_blank"
              rel="noopener noreferrer"
              onClick={() => setOpen(false)}
            >
              <MenuRow icon={<ChatIcon />} title={l.label} external>
                Ask questions about this page
              </MenuRow>
            </a>
          ))}
        </div>
      )}
    </div>
  );
}

function MenuRow({
  icon,
  title,
  external,
  children,
}: {
  icon: ReactNode;
  title: string;
  external?: boolean;
  children: ReactNode;
}) {
  return (
    <>
      <span className="copy-page-item-icon">{icon}</span>
      <span className="copy-page-item-text">
        <span className="copy-page-item-title">
          {title}
          {external && <ExternalIcon />}
          {external && (
            <span className="copy-page-sr"> (opens in a new tab)</span>
          )}
        </span>
        <span className="copy-page-item-desc">{children}</span>
      </span>
    </>
  );
}

const svg = {
  viewBox: "0 0 16 16",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.5,
  strokeLinecap: "round",
  strokeLinejoin: "round",
  "aria-hidden": true,
} as const;

function CopyIcon() {
  return (
    <svg {...svg}>
      <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" />
      <path d="M3.5 10.5h-.5A1.5 1.5 0 0 1 1.5 9V3A1.5 1.5 0 0 1 3 1.5h6A1.5 1.5 0 0 1 10.5 3v.5" />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg {...svg}>
      <path d="M3 8.5l3 3 7-7" />
    </svg>
  );
}

function ChevronIcon() {
  return (
    <svg {...svg}>
      <path d="M4 6l4 4 4-4" />
    </svg>
  );
}

function DocIcon() {
  return (
    <svg {...svg}>
      <path d="M9.5 1.5H4A1.5 1.5 0 0 0 2.5 3v10A1.5 1.5 0 0 0 4 14.5h8a1.5 1.5 0 0 0 1.5-1.5V5.5z" />
      <path d="M9.5 1.5v4h4M5.5 9h5M5.5 11.5h3" />
    </svg>
  );
}

function ChatIcon() {
  return (
    <svg {...svg}>
      <path d="M2.5 4A1.5 1.5 0 0 1 4 2.5h8A1.5 1.5 0 0 1 13.5 4v6a1.5 1.5 0 0 1-1.5 1.5H7l-3 2.5v-2.5A1.5 1.5 0 0 1 2.5 10z" />
    </svg>
  );
}

function ExternalIcon() {
  return (
    <svg {...svg} className="copy-page-ext">
      <path d="M6 3.5H3.5v9h9V10M9 2.5h4.5V7M13.5 2.5L7.5 8.5" />
    </svg>
  );
}
