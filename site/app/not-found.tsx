import type { Metadata } from "next";
import { SiteFooter } from "@/components/SiteFooter";
import "./not-found.css";

export const metadata: Metadata = {
  title: { absolute: "Page not found · Open Agent Primitives" },
};

// Every unknown path lands here, including stale /docs/<slug> bookmarks, so
// the page offers the two ways back in.
export default function NotFound() {
  return (
    <div className="nf">
      <main className="nf-main">
        <p className="nf-code">404</p>
        <h1>This page doesn&rsquo;t exist.</h1>
        <p>
          It may have moved when the docs were reorganized. Try the docs, or
          start from the home page.
        </p>
        <p className="nf-links">
          <a href="/docs">Browse the docs</a>
          <a href="/">Go to the home page</a>
        </p>
      </main>
      <SiteFooter />
    </div>
  );
}
