import { OapMark } from "./OapMark";
import { ThemeToggle } from "./ThemeToggle";
import "./site-footer.css";

// The AuthZed Discord, the destination authzed.com/discord redirects to.
const COMMUNITY = "https://discord.gg/TUd4k5McMX";
const LICENSE =
  "https://github.com/authzed/openagentprimitives/blob/main/LICENSE";

/* The footer shared by the landing page and the docs. Plain <a> throughout:
 * the two sections load different stylesheets, so crossing between them is a
 * full page load either way. */
export function SiteFooter() {
  return (
    <footer className="site-footer">
      <div className="site-footer-top">
        <a
          className="site-footer-home"
          href="/"
          aria-label="Open Agent Primitives home"
        >
          <OapMark className="site-footer-mark" />
        </a>
        <a href="/docs/what-is-oap">Docs</a>
        <a href="/docs/crd-reference">CRD reference</a>
        <a href="/docs/cli-reference">CLI reference</a>
        <a href="/#owasp">OWASP coverage</a>
        <a href={COMMUNITY}>Community</a>
      </div>
      <div className="site-footer-bottom">
        <p className="site-footer-credit">
          Built by <a href="https://authzed.com">AuthZed</a>, using{" "}
          <a href="https://github.com/authzed/spicedb">SpiceDB</a>. Open source
          under the <a href={LICENSE}>Apache 2.0 license</a>.
        </p>
        <ThemeToggle />
      </div>
    </footer>
  );
}
