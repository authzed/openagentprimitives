package browser

// actionfacts.go extracts the raw, deterministic "action facts" for a pending
// acting call (click/type/press/select/submit): the target's role and
// accessible text, the enclosing form's action (or a link's href), and a
// stable identifier for its nearest ancestor container. The AX snapshot
// (snapshot.go) already carries role; it does NOT carry form_action or
// container, since neither is part of the accessibility tree — both come from
// the DOM, read over CDP via a single Runtime.callFunctionOn evaluate on the
// resolved node (the same backendNodeID-keyed resolution the screenshot tool
// and Phase D capture already use — see dom.GetBoxModel().WithBackendNodeID in
// tools.go's screenshot).
//
// Phase E's later tasks (E4 stable-key, E5 page fingerprint) hash these
// fields, so every normalization here must be deterministic across repeated
// calls against the same element: the same button must always yield the same
// Role/Text/FormAction/Container, independent of a CSS-module/styled-components
// rebuild that changes its hashed class names between page loads.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ActionFacts is the raw fact set extracted for one pending acting call,
// before E4 keys it and E5 fingerprints the page. Every field is derived
// deterministically from the current DOM/AX state; nothing here is a judgment
// about consequence (that is E6/E7's job).
type ActionFacts struct {
	Op         string // the acting op: click | type | press | select | submit
	Role       string // the target's AX role (from the resolved ref's snapshot entry)
	Text       string // the target's accessible name / visible text, normalized
	FormAction string // URL PATH of the enclosing <form action>, or a link's href path; "" if neither
	Container  string // stable identifier for the nearest ancestor container: id, else a non-hashed class, else tag; "" if none found
	Host       string // canonical page host (CurrentHost())
	PageURL    string // the current page URL
}

// actionFactsRaw is the JSON shape actionFactsScript returns from the page.
type actionFactsRaw struct {
	FormAction string `json:"formAction"`
	Container  string `json:"container"`
	Text       string `json:"text"`
}

// actionFactsScript walks the DOM from the resolved node (bound as `this` by
// Runtime.callFunctionOn): it finds the enclosing form's action (or a link's
// href when there is no enclosing form), and the nearest ANCESTOR (starting at
// the parent, never the node itself) carrying an id, else a stable class, else
// falls back to that ancestor's tag name. All in one evaluate, so the walk sees
// a single consistent DOM snapshot rather than racing a page mutation between
// several round trips.
const actionFactsScript = `function() {
  function isStableClass(c) {
    if (!c) { return false; }
    if (/^(css-|sc-)/.test(c)) { return false; }
    if (c.length > 12 && /^[a-zA-Z0-9_-]+$/.test(c) && /[0-9]/.test(c)) { return false; }
    return true;
  }
  function containerOf(start) {
    var n = start;
    while (n && n.nodeType === 1) {
      if (n.id) { return n.id; }
      if (n.classList) {
        for (var i = 0; i < n.classList.length; i++) {
          if (isStableClass(n.classList[i])) { return n.classList[i]; }
        }
      }
      n = n.parentElement;
    }
    return (start && start.tagName) ? start.tagName.toLowerCase() : "";
  }
  function text(el) {
    if (!el) { return ""; }
    var label = el.getAttribute ? el.getAttribute("aria-label") : "";
    if (label && label.trim()) { return label.trim(); }
    if (typeof el.innerText === "string" && el.innerText.trim()) { return el.innerText.trim(); }
    if (typeof el.textContent === "string" && el.textContent.trim()) { return el.textContent.trim(); }
    if (el.value) { return String(el.value).trim(); }
    return "";
  }
  var formAction = "";
  var form = this.closest ? this.closest("form") : null;
  if (form && form.getAttribute("action")) {
    formAction = form.action;
  } else {
    var link = (this.tagName === "A" && this.href) ? this : (this.closest ? this.closest("a[href]") : null);
    if (link && link.href) { formAction = link.href; }
  }
  var container = containerOf(this.parentElement);
  return { formAction: formAction, container: container, text: text(this) };
}`

// actionFacts resolves ref to its backend DOM node and extracts its ActionFacts.
// op is carried through verbatim (the caller already knows which acting op this
// is; actionFacts does not validate it). A stale ref, a CDP failure, or a
// JavaScript exception in actionFactsScript all return a wrapped error — never
// a silently-zeroed ActionFacts — per the repo's no-silent-errors rule. A node
// with no enclosing form/link or no identifying ancestor is NOT an error: those
// fields are simply "".
func (s *Session) actionFacts(ctx context.Context, op, ref string) (ActionFacts, error) {
	facts := ActionFacts{Op: op}
	err := s.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := s.lookupRef(ref)
		if !ok {
			return fmt.Errorf("ref %s is stale", ref)
		}
		facts.Role = e.role

		var loc string
		if ae := runAction(c, func(ac context.Context) error {
			return chromedp.Location(&loc).Do(ac)
		}); ae != nil {
			return fmt.Errorf("reading page location: %w", ae)
		}
		facts.PageURL = loc

		var obj *runtime.RemoteObject
		if ae := runAction(c, func(ac context.Context) error {
			var e2 error
			obj, e2 = dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			return e2
		}); ae != nil {
			return fmt.Errorf("resolving DOM node for ref %s: %w", ref, ae)
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("ref %s resolved to no JavaScript object", ref)
		}

		var raw actionFactsRaw
		if ae := runAction(c, func(ac context.Context) error {
			result, exc, e2 := runtime.CallFunctionOn(actionFactsScript).
				WithObjectID(obj.ObjectID).
				WithReturnByValue(true).
				Do(ac)
			if e2 != nil {
				return e2
			}
			if exc != nil {
				return fmt.Errorf("javascript error walking the DOM for ref %s: %w", ref, exc)
			}
			if result == nil {
				return fmt.Errorf("callFunctionOn returned no result for ref %s", ref)
			}
			return json.Unmarshal(result.Value, &raw)
		}); ae != nil {
			return fmt.Errorf("extracting action facts for ref %s: %w", ref, ae)
		}

		facts.Text = normalizeActionText(raw.Text)
		facts.Container = raw.Container
		facts.FormAction = urlPath(raw.FormAction)
		return nil
	})
	if err != nil {
		return ActionFacts{}, fmt.Errorf("action facts for ref %s: %w", ref, err)
	}
	// CurrentHost takes its own s.do; called after the block above has released
	// the lock, never nested inside it.
	facts.Host = s.CurrentHost()
	return facts, nil
}

// normalizeActionText trims, collapses internal whitespace, and lowercases the
// target's accessible text — matching what E4/E5 hash, so the same control
// always normalizes identically regardless of incidental whitespace in the
// page's markup.
func normalizeActionText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// urlPath extracts the URL path from an absolute URL string (the DOM's
// resolved `.action` / `.href` property always returns one), returning "" for
// an empty or unparsable input rather than erroring: a missing form/link is a
// fact, not a failure.
func urlPath(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}
