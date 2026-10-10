package browser

// pagestate.go computes Task E5's page-state fingerprint: the deterministic
// value a browser mutate approval's SpiceDB caveat (pagestate_matches) is
// granted against, and which Phase E's acting-dispatch check later recomputes
// and presents at click time. SpiceDB authorizes the mutate only when the two
// fingerprints match, so this is the TOCTOU defense between "a human approved
// this" and "the agent is about to click it" — the page the human approved
// must still be the page about to be acted on.
//
// E3's actionFacts (actionfacts.go) extracts facts about ONLY the pending
// target node. E4's ActionKey (actionkey.go) hashes those facts into a stable
// SpiceDB resource id. Neither carries enough to detect page-wide or
// surrounding-container drift: the AX snapshot (snapshot.go) gives role/name/
// depth only, never action-path, disabled, or container grouping. So
// capturePageState below does its OWN focused CDP extraction — one
// Runtime.callFunctionOn evaluate bound to the target's resolved DOM node,
// mirroring actionFactsScript's resolution pattern — that additionally walks
// the whole document for a page-wide interaction skeleton and the target's
// own container for its interactive elements and display text.
//
// Per design ruling E-R2: the target container's TEXT content is load-bearing
// and must never be dropped from the fingerprint. The grant is keyed on the
// ACTION ("confirm seat 14C"), not on the price shown beside it — so if the
// fingerprint ignored container text, a price change from $342.50 to $402.50
// inside the same modal would still satisfy the caveat and the mutate would
// fire at the new, unapproved price. Container text is what catches that.
//
// Benign churn OUTSIDE the target's container (ads, a promo's hashed class,
// a footer timestamp, unrelated widgets reordering) must NOT move the
// fingerprint — that is the over-ask-prevention half of this design, proven
// by pagestate_chrome_test.go against the laya spike's own scenarios.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
)

// PageState is the deterministic fact set PageFingerprint hashes. Every field
// is a plain string (or slice of them) already normalized the same way E3
// normalizes ActionFacts, so two independent captures of the same logical
// page state always produce byte-identical PageState values.
type PageState struct {
	// URLPath is the current page's path (no scheme/host/query): the spike's
	// "url changed" code-tier check, verbatim.
	URLPath string
	// Skeleton is the sorted set of "role|normalized-name|action-path|disabled"
	// signatures for every page-wide interactive element (buttons, links,
	// inputs, textareas, selects, and ARIA-roled controls) — the spike's
	// skeleton_hash input.
	Skeleton []string
	// TargetSig is the pending target's OWN signature, read fresh from the
	// live DOM node (not carried over from the approval-time ActionFacts) so a
	// toggle of its own disabled state or its own form's action is visible
	// here even when nothing else on the page moved.
	TargetSig string
	// ContainerSigs is the sorted set of interactive-element signatures found
	// within the target's nearest identified ancestor container (same
	// containerOf() ancestor walk E3's actionFactsScript uses).
	ContainerSigs []string
	// ContainerTexts is the sorted set of normalized display-text lines found
	// within that same container — the field design ruling E-R2 requires: it
	// is what catches a price (or any other context text) changing beside an
	// otherwise-unchanged target control.
	ContainerTexts []string
}

// pageStateElSig is the raw (un-normalized) element signature pageStateScript
// returns for one interactive element: the DOM-read role/text/form-action/
// disabled, before Go applies E3's normalizeActionText/urlPath normalization.
type pageStateElSig struct {
	Role       string `json:"role"`
	Text       string `json:"text"`
	FormAction string `json:"formAction"`
	Disabled   bool   `json:"disabled"`
}

// pageStateRaw is the JSON shape pageStateScript returns from the page.
type pageStateRaw struct {
	URLPath        string           `json:"urlPath"`
	Skeleton       []pageStateElSig `json:"skeleton"`
	Target         pageStateElSig   `json:"target"`
	ContainerSigs  []pageStateElSig `json:"containerSigs"`
	ContainerTexts []string         `json:"containerTexts"`
}

// interactiveSelector is the CSS selector pageStateScript uses, page-wide and
// container-scoped, to find interactive/actionable elements: native controls
// (buttons, links, inputs, textareas, selects) plus the common ARIA interaction
// roles (button/link/textbox/searchbox/gridcell/checkbox/radio/combobox/
// menuitem/tab). It is deliberately its OWN set, independent of snapshot.go's
// interestingRole: that function drives the read-outline render and so includes
// non-actionable display roles (heading/list/image/row/text) while omitting some
// of these interaction roles (checkbox/radio/menuitem/tab) — the skeleton wants
// what a user can ACT on, not what the outline chooses to show.
const interactiveSelector = `button, a[href], input, textarea, select, ` +
	`[role="button"], [role="link"], [role="textbox"], [role="searchbox"], ` +
	`[role="gridcell"], [role="checkbox"], [role="radio"], [role="combobox"], ` +
	`[role="menuitem"], [role="tab"]`

// pageStateScript runs bound (via Runtime.callFunctionOn) to the pending
// target's resolved DOM node as `this`. It walks from the target's PARENT to
// find its container — the identical ancestor walk actionFactsScript's
// containerOf() performs (first ancestor with an id, else a non-hashed class,
// else the nearest ancestor itself) — then collects, in one consistent DOM
// read: every page-wide interactive element's raw signature, the target's own
// raw signature, the container-scoped interactive elements' raw signatures,
// and the container's rendered text lines. All normalization (text trim/
// collapse/lowercase, action/href -> URL path) happens in Go afterward via the
// SAME helpers E3 uses (normalizeActionText, urlPath), so there is exactly one
// implementation of each normalization rule in the package.
const pageStateScript = `function() {
  function isStableClass(c) {
    if (!c) { return false; }
    if (/^(css-|sc-)/.test(c)) { return false; }
    if (c.length > 12 && /^[a-zA-Z0-9_-]+$/.test(c) && /[0-9]/.test(c)) { return false; }
    return true;
  }
  function containerNodeOf(start) {
    var n = start;
    while (n && n.nodeType === 1) {
      if (n.id) { return n; }
      if (n.classList) {
        for (var i = 0; i < n.classList.length; i++) {
          if (isStableClass(n.classList[i])) { return n; }
        }
      }
      n = n.parentElement;
    }
    return start;
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
  function roleOf(el) {
    var r = el.getAttribute ? el.getAttribute("role") : "";
    if (r) { return r.toLowerCase(); }
    var tag = el.tagName ? el.tagName.toLowerCase() : "";
    if (tag === "a") { return "link"; }
    if (tag === "button") { return "button"; }
    if (tag === "select") { return "combobox"; }
    if (tag === "textarea") { return "textbox"; }
    if (tag === "input") {
      var it = (el.getAttribute("type") || "text").toLowerCase();
      if (it === "submit" || it === "button" || it === "reset" || it === "image") { return "button"; }
      if (it === "checkbox") { return "checkbox"; }
      if (it === "radio") { return "radio"; }
      return "textbox";
    }
    return tag;
  }
  function formActionOf(el) {
    if (el.tagName === "A" && el.href) { return el.href; }
    var form = el.closest ? el.closest("form") : null;
    if (form && form.getAttribute("action")) { return form.action; }
    return "";
  }
  function disabledOf(el) {
    if (el.disabled === true) { return true; }
    if (el.hasAttribute && el.hasAttribute("disabled")) { return true; }
    var ad = el.getAttribute ? el.getAttribute("aria-disabled") : null;
    return ad === "true";
  }
  function rawSig(el) {
    return { role: roleOf(el), text: text(el), formAction: formActionOf(el), disabled: disabledOf(el) };
  }
  var containerNode = containerNodeOf(this.parentElement) || document.body;

  var skeleton = [];
  var skelEls = document.querySelectorAll('` + interactiveSelector + `');
  for (var i = 0; i < skelEls.length; i++) { skeleton.push(rawSig(skelEls[i])); }

  var containerSigs = [];
  var containerEls = containerNode.querySelectorAll ? containerNode.querySelectorAll('` + interactiveSelector + `') : [];
  for (var j = 0; j < containerEls.length; j++) { containerSigs.push(rawSig(containerEls[j])); }

  var containerTexts = [];
  var rawText = containerNode.innerText || containerNode.textContent || "";
  var lines = rawText.split("\n");
  for (var k = 0; k < lines.length; k++) {
    if (lines[k].trim()) { containerTexts.push(lines[k]); }
  }

  return {
    urlPath: location.pathname,
    skeleton: skeleton,
    target: rawSig(this),
    containerSigs: containerSigs,
    containerTexts: containerTexts
  };
}`

// capturePageState captures the current PageState around the pending target
// identified by targetRef (a live, non-stale ref from the session's latest
// snapshot — see lookupRef). target carries the approval-time ActionFacts
// (E3); it is used only for error context here, never for the signature
// values themselves, because the whole point of a revalidation capture is to
// read what is ACTUALLY on the page right now, not what was recorded earlier.
// A stale ref, a CDP failure, or a JavaScript exception in pageStateScript all
// return a wrapped error — never a silently-zeroed PageState.
func (s *Session) capturePageState(ctx context.Context, target ActionFacts, targetRef string) (PageState, error) {
	var ps PageState
	err := s.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := s.lookupRef(targetRef)
		if !ok {
			return fmt.Errorf("%w: ref %s", ErrStaleRef, targetRef)
		}

		var obj *runtime.RemoteObject
		if ae := runAction(c, func(ac context.Context) error {
			var e2 error
			obj, e2 = dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			return e2
		}); ae != nil {
			return fmt.Errorf("resolving DOM node for ref %s: %w", targetRef, ae)
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("ref %s resolved to no JavaScript object", targetRef)
		}

		var raw pageStateRaw
		if ae := runAction(c, func(ac context.Context) error {
			result, exc, e2 := runtime.CallFunctionOn(pageStateScript).
				WithObjectID(obj.ObjectID).
				WithReturnByValue(true).
				Do(ac)
			if e2 != nil {
				return e2
			}
			if exc != nil {
				return fmt.Errorf("javascript error capturing page state for ref %s: %w", targetRef, exc)
			}
			if result == nil {
				return fmt.Errorf("callFunctionOn returned no result for ref %s", targetRef)
			}
			return json.Unmarshal(result.Value, &raw)
		}); ae != nil {
			return fmt.Errorf("extracting page state for ref %s: %w", targetRef, ae)
		}

		ps = buildPageState(raw)
		return nil
	})
	if err != nil {
		return PageState{}, fmt.Errorf("page state for %s ref %s (op %s): %w", target.Role, targetRef, target.Op, err)
	}
	return ps, nil
}

// capturePageStateAtFocus captures PageState anchored on the live
// document.activeElement — the element a `press` keystroke actually targets.
// It mirrors capturePageState but resolves the focused node via a JS evaluation
// instead of a ref lookup, because a press carries no ref (see stampPress). The
// SAME pageStateScript runs with `this` bound to the focused element, so a gated
// committing press gets a REAL, re-matchable fingerprint; an empty one would
// make the minted web_action grant permanently unapprovable — pagestate_matches
// denies a "" approved value. A JS failure (no focused element, a CDP fault)
// returns a wrapped error, never a silently-zeroed PageState, so the caller
// fails closed to an empty fingerprint rather than a bogus match.
func (s *Session) capturePageStateAtFocus(ctx context.Context) (PageState, error) {
	var ps PageState
	err := s.do(ctx, shortTimeout, func(c context.Context) error {
		var obj *runtime.RemoteObject
		if ae := runAction(c, func(ac context.Context) error {
			res, exc, e2 := runtime.Evaluate("document.activeElement").Do(ac)
			if e2 != nil {
				return e2
			}
			if exc != nil {
				return fmt.Errorf("javascript error resolving document.activeElement: %w", exc)
			}
			obj = res
			return nil
		}); ae != nil {
			return fmt.Errorf("resolving document.activeElement: %w", ae)
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("document.activeElement resolved to no JavaScript object")
		}

		var raw pageStateRaw
		if ae := runAction(c, func(ac context.Context) error {
			result, exc, e2 := runtime.CallFunctionOn(pageStateScript).
				WithObjectID(obj.ObjectID).
				WithReturnByValue(true).
				Do(ac)
			if e2 != nil {
				return e2
			}
			if exc != nil {
				return fmt.Errorf("javascript error capturing page state at focus: %w", exc)
			}
			if result == nil {
				return fmt.Errorf("callFunctionOn returned no result for document.activeElement")
			}
			return json.Unmarshal(result.Value, &raw)
		}); ae != nil {
			return fmt.Errorf("extracting page state at focus: %w", ae)
		}

		ps = buildPageState(raw)
		return nil
	})
	if err != nil {
		return PageState{}, fmt.Errorf("page state at focus (op press): %w", err)
	}
	return ps, nil
}

// buildPageState normalizes pageStateScript's raw JSON into a PageState,
// applying E3's own normalizeActionText/urlPath helpers so every signature in
// this package is normalized by exactly one implementation. Lists are sorted
// here (and re-sorted defensively inside PageFingerprint) so document order —
// which a benign unrelated-widget reorder changes — never affects the result.
func buildPageState(raw pageStateRaw) PageState {
	skeleton := make([]string, 0, len(raw.Skeleton))
	for _, e := range raw.Skeleton {
		skeleton = append(skeleton, elSigString(e))
	}
	containerSigs := make([]string, 0, len(raw.ContainerSigs))
	for _, e := range raw.ContainerSigs {
		containerSigs = append(containerSigs, elSigString(e))
	}
	containerTexts := make([]string, 0, len(raw.ContainerTexts))
	for _, line := range raw.ContainerTexts {
		if norm := normalizeActionText(line); norm != "" {
			containerTexts = append(containerTexts, norm)
		}
	}
	sort.Strings(skeleton)
	sort.Strings(containerSigs)
	sort.Strings(containerTexts)
	return PageState{
		URLPath:        raw.URLPath,
		Skeleton:       skeleton,
		TargetSig:      elSigString(raw.Target),
		ContainerSigs:  containerSigs,
		ContainerTexts: containerTexts,
	}
}

// elSigString normalizes one raw element signature into the
// "role|normalized-name|action-path|disabled" string every list in PageState
// hashes, using E3's own normalizeActionText and urlPath helpers.
func elSigString(e pageStateElSig) string {
	return fmt.Sprintf("%s|%s|%s|%t", e.Role, normalizeActionText(e.Text), urlPath(e.FormAction), e.Disabled)
}

// PageFingerprint is a PURE function of PageState: sha256 hex over the url
// path, the sorted interaction skeleton, the target's own signature, and the
// sorted container signatures/texts. It sorts a fresh copy of every slice
// field regardless of the order PageState arrived in, so callers never need
// to pre-sort and so benign reordering anywhere in the input can never change
// the result. No wall-clock, no randomness, no map iteration.
func PageFingerprint(ps PageState) string {
	var b strings.Builder
	b.WriteString("url_path=")
	b.WriteString(ps.URLPath)
	b.WriteByte(0)
	writeSortedFingerprintSection(&b, "skeleton", ps.Skeleton)
	b.WriteString("target=")
	b.WriteString(ps.TargetSig)
	b.WriteByte(0)
	writeSortedFingerprintSection(&b, "container_sigs", ps.ContainerSigs)
	writeSortedFingerprintSection(&b, "container_texts", ps.ContainerTexts)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeSortedFingerprintSection appends one labeled, sorted, NUL-section-
// terminated list to b. The items themselves are joined with \x1f (ASCII unit
// separator — never produced by normalizeActionText/urlPath), so no
// concatenation of two differently-split lists can collide.
func writeSortedFingerprintSection(b *strings.Builder, label string, items []string) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	b.WriteString(label)
	b.WriteByte('=')
	b.WriteString(strings.Join(sorted, "\x1f"))
	b.WriteByte(0)
}
