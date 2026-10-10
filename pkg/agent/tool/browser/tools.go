package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

const (
	// maxPageBytes is the read pagination budget per page.
	maxPageBytes = 16 * 1024
	// navigateTimeout bounds a navigation (navigate / follow / back).
	navigateTimeout = 30 * time.Second
	// snapshotTimeout bounds an accessibility snapshot (read / find).
	snapshotTimeout = 20 * time.Second
	// redactedScreenshotTimeout bounds CaptureRedacted's own s.do call —
	// DEDICATED rather than sharing snapshotTimeout, because the approval
	// screenshot is a best-effort capture whose caller (the runner's
	// captureBrowserScreenshotFn) already gives it a generous, decoupled outer
	// deadline (see that closure's doc) to survive a cold first capture
	// against a real browser pod; read/find have no such margin to spend and
	// must stay bounded at snapshotTimeout. Kept a few seconds above
	// snapshotTimeout, not blindly raised to it, so a slow redact (DPR read +
	// N box-model resolves + the capture itself) has room without widening
	// every other snapshot-timed call.
	redactedScreenshotTimeout = 25 * time.Second
	// shortTimeout bounds a quick action (scroll, a ref resolve).
	shortTimeout = 10 * time.Second
	// pollInterval is how often wait_for re-snapshots.
	pollInterval = 250 * time.Millisecond

	waitMinTimeout     = 1 * time.Second
	waitMaxTimeout     = 30 * time.Second
	waitDefaultTimeout = 30 * time.Second

	// hostArgKey is the named-argument key the acting tools surface the current
	// page host under (via NamedArgs) and that the web_host check's
	// ResourceIDTemplate reads it back from. One constant keeps the two halves —
	// what the tool writes and what the check reads — from drifting apart.
	hostArgKey = "host"

	// requestHumanStepOp is the human-handoff op: it asks a person to complete a
	// sign-in step the agent cannot do itself. It is offered ONLY in act mode (it
	// is part of the login flow), but it is NOT a mutating acting op — it writes
	// nothing to the page — so it is deliberately absent from actingOpNames and
	// carries StateImpact Stateless, not External on web_host. See
	// browserTool.Permission.
	requestHumanStepOp = "request_human_step"

	// defaultMutateThreshold mirrors haiku.DefaultMutateThreshold (0.15): the
	// policy cut (ruling E-R3) at or above which the judge's P(mutate) gates a
	// call as a mutate. Kept as a local constant rather than importing the haiku
	// backend so this CDP/DOM tool package does not pull the LLM-provider
	// transitive deps in just for a number; threshold_test.go pins it equal to
	// haiku.DefaultMutateThreshold so the two cannot drift. See
	// haiku.DefaultMutateThreshold for why 0.15 (chosen against a live Haiku sweep).
	defaultMutateThreshold = 0.15

	// webActionClassifyTimeout bounds the whole classify-and-stamp pass NamedArgs
	// runs on the authz path (action-fact extraction, the model Classify call, and
	// the page-fingerprint capture). NamedArgs has no caller context to thread, so
	// this is the standalone deadline for the model round trip; the per-CDP-step
	// timeouts inside Session.do still apply underneath it.
	webActionClassifyTimeout = 30 * time.Second
)

// actingOpNames is the set of ops that reach and mutate external page state
// (spec §3.2). It is the single source of truth for which ops are "acting":
// synthesize.go offers them only in interaction=act, Permission() gates them
// External on the web_host slot, and NamedArgs surfaces the page host for them.
// Every other op is an observation (Passthrough).
var actingOpNames = map[string]bool{
	"type":   true,
	"click":  true,
	"press":  true,
	"select": true,
	"submit": true,
}

// isActingOp reports whether op reaches external page state and therefore
// authorizes External against the current page host.
func isActingOp(op string) bool { return actingOpNames[op] }

// namedKeys maps the key names an acting press call may use to the single
// control rune chromedp.KeyEvent encodes into the right DevTools key event.
// KeyEvent otherwise types its argument CHARACTER BY CHARACTER, so without this
// map press("Enter") would type the five letters E-n-t-e-r rather than pressing
// Return.
var namedKeys = map[string]string{
	"Enter":      kb.Enter,
	"Return":     kb.Enter,
	"Tab":        kb.Tab,
	"Escape":     kb.Escape,
	"Esc":        kb.Escape,
	"Backspace":  kb.Backspace,
	"Delete":     kb.Delete,
	"Del":        kb.Delete,
	"ArrowUp":    kb.ArrowUp,
	"ArrowDown":  kb.ArrowDown,
	"ArrowLeft":  kb.ArrowLeft,
	"ArrowRight": kb.ArrowRight,
	"Home":       kb.Home,
	"End":        kb.End,
	"PageUp":     kb.PageUp,
	"PageDown":   kb.PageDown,
	"Space":      " ",
}

// browserTool is one observation tool for a resolved BrowserToolbox. Every tool
// of a toolbox shares one *Session and one origin; op selects the behavior.
type browserTool struct {
	name        string
	op          string
	description string
	schema      json.RawMessage
	origin      string

	sess   *Session
	rtName string // the LLM-facing toolbox Name, embedded in the policy error text
	mode   string
	sites  []browserpolicy.Site

	// catalog backs query_logins only; nil/empty for every other op (and for
	// query_logins itself when the toolbox binds no identity, in which case
	// the op is never offered — see synthesize.go's queryLoginsOp gate).
	catalog []spiceboxv1alpha1.BrowserLoginRef

	// filler resolves {{login...}} placeholders for the type op, strictly inside
	// Execute so the transcript only ever records the placeholder. Shared across
	// a toolbox's tools (one per session, over the session's §5.3 hold).
	filler CredentialFiller

	// humanStep backs request_human_step only; nil for every other op (and for
	// request_human_step itself when the toolbox is synthesized without a
	// requester, e.g. a unit test — Execute guards nil rather than panicking).
	// The runner implements it over the approval host (pkg/agent/runner); a fake
	// satisfies it in tests.
	humanStep HumanStepRequester

	// judge is the injected consequence classifier (E7's Haiku backend in
	// production; a fake in tests) consulted, for an acting op, to decide whether
	// a call is a MUTATE that needs the per-action web_action grant on top of the
	// coarse web_host one. A cheap deterministic fast-path (judge.NewDeterministic)
	// runs first and skips this model call for the obviously-benign cases
	// (link-follow, search entry, tab switch). nil means per-action revalidation
	// is NOT wired for this tool (a bare unit test, or an e2e run that did not set
	// the seam). A nil judge is FAIL-CLOSED, not open: NamedArgs stamps an acting
	// call on a live host as a mutate with an empty fingerprint (which the
	// web_action caveat refuses), so an un-judged acting call is gated, never
	// waved through on the coarse web_host grant alone. The production runner
	// always injects a non-nil judge (falling back to the deterministic one), so
	// this is a defensive floor. See NamedArgs.
	judge judge.ConsequenceJudge
}

var (
	_ agenttool.Tool          = (*browserTool)(nil)
	_ agenttool.OriginTool    = (*browserTool)(nil)
	_ agenttool.Cancellable   = (*browserTool)(nil)
	_ agenttool.NamedCallArgs = (*browserTool)(nil)
	_ ScreenshotProvider      = (*browserTool)(nil)
)

// ScreenshotCapturer returns this tool's session as a BrowserScreenshotCapturer
// — every tool of a toolbox shares one *Session (see the browserTool doc
// comment), so any op's tool can hand the runner a capturer for the same live
// page. Returns nil only if the tool was synthesized without a session (a unit
// test), never in production wiring.
func (t *browserTool) ScreenshotCapturer() BrowserScreenshotCapturer {
	if t.sess == nil {
		return nil
	}
	return t.sess
}

func (t *browserTool) Name() string                 { return t.name }
func (t *browserTool) Kind() agenttool.Kind         { return agenttool.KindMCP }
func (t *browserTool) Description() string          { return t.description }
func (t *browserTool) InputSchema() json.RawMessage { return t.schema }

// Permission for an OBSERVATION op is Passthrough: a browser read reaches
// external state but maps to no SpiceDB resource, so no per-tool Check is
// required — the gate's egress policy, not SpiceDB, authorizes which hosts the
// agent may reach.
//
// Permission for an ACTING op (click/press/select/submit) is External on the
// web_host slot: every one of these leaves the session and mutates the real
// site, so it must be satisfied by a human-approved slot grant for the current
// page host and nothing else. The instance is the page host, surfaced by
// NamedArgs under hostArgKey — it is PAGE STATE, not a model argument, so the
// model cannot name a host it is not actually on. An empty host (about:blank /
// no page) resolves the template to "", which the External check treats as an
// UnresolvedResource denial (fail-closed), never a silent allow.
//
// Shaped like git push's external check (toolkits/git.yaml), with the one
// difference a host forces: the resource id is the canonical page host
// (Session.CurrentHost runs browserpolicy.CanonicalHost), which is NOT a legal
// SpiceDB object id as-is. SpiceDB's object-id charset forbids dots and every
// real hostname has them, so `web_host:example.com` is rejected with
// InvalidArgument — the acting feature could never authorize against real SpiceDB
// without escaping. ResourceIDTransforms therefore runs browserschema's canonical
// chain (spicedb_escape: "." → "=2E"), the SAME chain the AgentClass reconciler
// publishes onto web_host's resolvedSlot so the grant-write legs mint the
// identical id. spicedb_escape is INJECTIVE, so the plan-gate card can keep
// DISPLAYING the raw host (derivedInstanceID drops the escape for display) while
// the grant is written on the escaped id, and the card stays a truthful claim
// about exactly which host was approved.
func (t *browserTool) Permission() authz.Permission {
	// request_human_step is offered only in act mode but is NOT a page mutation:
	// it posts a human-in-the-loop card (confirm) or a notice (captcha) and
	// reaches no external page state. Its authorization IS the human login_human_step
	// card it publishes (DecideApprovers, re-checked server-side), so it needs no
	// web_host slot gate — layering one would be a redundant second human gate for
	// one handoff. Stateless, exactly like set_preference, whose confirm is also its
	// own gate. (The emitted-code half types into the page via the runner, after
	// the human approves — never a model-driven acting call through this tool.)
	if t.op == requestHumanStepOp {
		return authz.Permission{StateImpact: authz.Stateless}
	}
	if !isActingOp(t.op) {
		return authz.Permission{StateImpact: authz.Passthrough}
	}
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       browserschema.WebHostType,
			Permission:         browserschema.ActPermission,
			ResourceIDTemplate: "{" + hostArgKey + "}",
			// Escape the host into a legal SpiceDB object id — dots are illegal in
			// one. Sourced from browserschema so the check path and every
			// grant-write leg derive the identical id. See Permission's doc.
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
			ResourceIDHint: "this acts on the page the browser is currently on; open an http(s) page with navigate first — " +
				"about:blank or no page has no host to authorize against",
		},
	}
}
func (t *browserTool) PermissionVariants() []authz.PermissionVariant { return nil }

// NamedArgs surfaces the current page host under hostArgKey so an acting op's
// web_host check resolves its resource instance from PAGE STATE rather than from
// a model argument, and — for a call it classifies as a MUTATE — additionally
// stamps the web_action key + live page fingerprint (Phase E's E8 gate) so the
// dispatch layer can layer a per-action web_action check on top of the coarse
// web_host one. Every stamped value is PAGE STATE or a judge verdict, written
// LAST so it wins over any same-named key the model may have put in the call —
// the host, the action key and the fingerprint are facts about the page, not
// things the caller gets to assert.
//
// For an observation op the args pass through unchanged (ok=true): those tools
// are Passthrough and resolve no resource, so there is nothing to surface and no
// reason to spend a round-trip reading the live location on the authz path.
//
// On any acting op, NamedArgs first DELETES the four reserved web_action keys
// the model may have supplied (reservedWebActionArgs) — they are page facts, not
// caller input — so nothing the model asserts can survive into the normalized
// args the dispatch layer lifts from.
//
// For a live page (host != "") the gate is then resolved FAIL-CLOSED on both
// axes. With a judge injected, the per-action classify-and-stamp runs
// (stampWebAction): a mutate stamps the key + live fingerprint + flag, a
// positively-benign observe/navigate/session stamps none of the three, and a
// judge error / unreadable facts stamp the mutate flag so the call is gated. A
// NIL judge (a degenerate/hand-built wiring; production always injects one)
// does NOT skip the gate: it stamps a mutate with an EMPTY fingerprint from
// op+host alone — which the web_action pagestate_matches caveat refuses — rather
// than letting the call through on the coarse web_host grant by itself. An empty
// host is the about:blank / no-page case the web_host check already denies on
// (UnresolvedResource), so there is nothing to revalidate and nothing is stamped.
//
// ok is always true: a browser call's arguments are plain JSON that always
// parse, unlike a sandbox argv. CurrentHost returning "" is NOT a parse failure
// — it is the fail-closed empty instance the External check denies on — so it is
// reported as a resolved (empty) host, not as ok=false.
func (t *browserTool) NamedArgs(args map[string]any) (map[string]any, bool) {
	if !isActingOp(t.op) {
		return args, true
	}
	out := make(map[string]any, len(args)+4)
	for k, v := range args {
		out[k] = v
	}
	// Strip every reserved web_action key the MODEL may have put in the call
	// BEFORE stamping. These four are facts the browser tool asserts about the
	// page (the gate flag, the action key, the live fingerprint, the human
	// description), never values the caller gets to supply — and the dispatch
	// layer lifts them off this normalized map into typed authz inputs. Deleting
	// them up front means a model cannot inject a forged grant/fingerprint on ANY
	// path below: not the mutate path (which overwrites them anyway), and not the
	// benign/stale/nil-judge paths (which previously left a model's own injected
	// keys flowing straight through). Defense in depth with the runner-side lift,
	// which already trusts only the browser tool for these.
	for _, k := range reservedWebActionArgs {
		delete(out, k)
	}
	host := t.sess.CurrentHost()
	out[hostArgKey] = host
	if host == "" {
		// about:blank / no page: the web_host check already denies on an empty
		// instance (UnresolvedResource), so there is nothing to revalidate.
		return out, true
	}
	if t.judge != nil {
		t.stampWebAction(out, args, host)
		return out, true
	}
	// No judge is wired for per-action revalidation, but this is still an acting
	// op on a LIVE host. Fail closed: stamp a mutate with an empty fingerprint
	// (which the web_action pagestate_matches caveat refuses) rather than waving
	// the call through on the coarse web_host grant alone. Built from op+host
	// only — a nil judge is a hand-built/degenerate wiring that may have no live
	// browser, so this path deliberately does NOT read the DOM. Production always
	// injects a judge (the runner falls back to the deterministic one), so this
	// is a defensive floor, not a hot path.
	out[browserschema.WebActionRequiredArg] = true
	out[browserschema.WebActionKeyArg] = ActionKey(ActionFacts{Op: t.op, Host: host})
	out[browserschema.WebActionFingerprintArg] = ""
	out[browserschema.WebActionDescriptionArg] = humanActionDescriptor(ActionFacts{Op: t.op, Host: host})
	return out, true
}

// reservedWebActionArgs are the four NamedArgs keys the browser tool stamps and
// the dispatch layer lifts — the gate flag, the action key, the live page
// fingerprint, and the human description. They are page facts / a judge verdict,
// never model-supplied, so NamedArgs deletes any copy the model put in the call
// before stamping its own.
var reservedWebActionArgs = []string{
	browserschema.WebActionRequiredArg,
	browserschema.WebActionKeyArg,
	browserschema.WebActionFingerprintArg,
	browserschema.WebActionDescriptionArg,
}

// stampWebAction classifies this acting call's consequence tier and, for a
// MUTATE, stamps the web_action key (E4) and the live page fingerprint (E5) into
// out so the dispatch layer can require a per-action web_action grant on top of
// the coarse web_host one. An observe/navigate/session verdict stamps nothing
// of THOSE three keys — but every call through here (mutate or not) stamps
// WebActionDescriptionArg, a human-readable "what this does" sentence (see
// action_descriptor.go): even a benign click can still need approval for the
// coarse web_host grant (e.g. the agent's first action on a new host), and
// that card deserves the same phrasing a mutate's does.
//
// A `press` is classified SEPARATELY, up front (see stampPress): it carries no
// element ref — its Execute sends the keystroke to document.activeElement,
// ignoring any ref the model supplies — so it is classified from its KEY, never
// from a ref'd element. Honoring a model-supplied ref for a press is exactly how
// a bogus ref re-opened an un-gated form submit, so the ref is not read here.
//
// For the ref-carrying ops (click/type/select/submit), FAIL-CLOSED holds with
// ONE deliberate exception: if the action's facts cannot be read because the ref
// is STALE (errors.Is ErrStaleRef — the element is simply gone), the call is NOT
// gated, because it cannot execute at all; each of those ops re-resolves the
// SAME ref in its own Execute handler and refuses it independently (see the
// ErrStaleRef branch below). Every OTHER fact-read fault (a CDP transport error,
// a JS exception) is treated as a mutate and gated, since those mean "we don't
// know what this would do," not "this cannot run." If the judge errors or is
// unavailable the call is treated as a mutate (classifyMutate); and a
// page-fingerprint capture failure stamps an EMPTY fingerprint, which the
// web_action check's pagestate_matches caveat refuses — never a silent allow.
// Every failure is logged (never silently dropped) with toolbox/op/host context.
func (t *browserTool) stampWebAction(out, args map[string]any, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), webActionClassifyTimeout)
	defer cancel()

	// A press has no element ref — its Execute sends the keystroke to whatever
	// is focused, ignoring any ref the model supplies — so classify it from the
	// KEY, not from a (model-controlled, Execute-ignored) ref. See stampPress.
	if t.op == "press" {
		t.stampPress(ctx, out, args, host)
		return
	}

	ref := stringArg(args, "ref")
	facts, err := t.sess.actionFacts(ctx, t.op, ref)
	if err != nil {
		if errors.Is(err, ErrStaleRef) {
			// The ref named an element that is simply GONE (the page re-rendered
			// since the agent last read it) — this call cannot execute, so it
			// cannot mutate anything either. Gating it as a mutate would only
			// raise a spurious web_action approval the user has to dismiss, with
			// no grant ever minted because the call never gets to use it (the bug
			// this exemption addresses). The ref-carrying acting ops
			// (click/type/select/submit) each independently re-resolve the SAME
			// ref via lookupRef in their own Execute handler and refuse with a
			// clean IsError tool result telling the model to read the page again —
			// nothing runs un-gated. A press never reaches this branch: it is
			// classified by key above, precisely because its Execute does NOT
			// re-resolve a ref and so has no such second check to fall back on.
			slog.Info("browser: ref is stale while extracting action facts; not gating as mutate (the call cannot execute)",
				"toolbox", t.rtName, "op", t.op, "host", host, "err", err.Error())
			out[browserschema.WebActionDescriptionArg] = humanActionDescriptor(ActionFacts{Op: t.op, Host: host})
			return
		}
		// Could not read what this call acts on for some OTHER reason (a CDP
		// fault, a JS error mid-walk, ...), so it cannot be proven benign: gate
		// it. The key is best-effort from what is known (op + host); the empty
		// fingerprint makes the web_action check deny regardless. The descriptor
		// degrades gracefully to a generic op+host phrase (never the raw ref) —
		// humanActionDescriptor is a pure function of ActionFacts and handles
		// every field being empty.
		slog.Warn("browser: action-fact extraction failed; gating acting call as mutate (fail-closed)",
			"toolbox", t.rtName, "op", t.op, "host", host, "err", err.Error())
		out[browserschema.WebActionRequiredArg] = true
		out[browserschema.WebActionKeyArg] = ActionKey(ActionFacts{Op: t.op, Host: host})
		out[browserschema.WebActionFingerprintArg] = ""
		out[browserschema.WebActionDescriptionArg] = humanActionDescriptor(ActionFacts{Op: t.op, Host: host})
		return
	}

	out[browserschema.WebActionDescriptionArg] = humanActionDescriptor(facts)

	if !t.classifyMutate(ctx, facts) {
		return // positively benign (observe/navigate/session): no per-action gate
	}

	key := ActionKey(facts)
	fingerprint := ""
	if ps, perr := t.sess.capturePageState(ctx, facts, ref); perr != nil {
		slog.Warn("browser: page-state capture failed; gating with an empty fingerprint (fail-closed)",
			"toolbox", t.rtName, "op", t.op, "host", host, "err", perr.Error())
	} else {
		fingerprint = PageFingerprint(ps)
	}
	out[browserschema.WebActionRequiredArg] = true
	out[browserschema.WebActionKeyArg] = key
	out[browserschema.WebActionFingerprintArg] = fingerprint
}

// stampPress classifies a `press` acting call. A press carries no element ref:
// its Execute (press()) sends the key to document.activeElement, ignoring any
// ref the model supplies — so it is classified from the KEY, never from a ref'd
// element (reading a model-supplied ref here is how a bogus ref re-opened an
// un-gated form submit). Per the "only submission commits" rule the consequence
// judge draws for every other op, a key that can COMMIT or ACTIVATE a form —
// Enter/Return (submit) or Space (activate the focused control) — is a MUTATE
// and gated; every other key (Tab, Escape, the arrows, editing keys, a single
// typed character) edits or navigates LOCAL page state and is not, so a press
// that merely moves focus or scrolls never raises a spurious approval.
//
// The gate's page-state fingerprint is captured against the FOCUSED element
// (capturePageStateAtFocus), the element the keystroke actually targets, so the
// approval binds to the page the user saw and re-matches on the next identical
// press. A capture failure stamps an EMPTY fingerprint, which the web_action
// pagestate_matches caveat refuses (fail-closed) — the same floor every other
// gated op uses.
func (t *browserTool) stampPress(ctx context.Context, out, args map[string]any, host string) {
	key := stringArg(args, "key")
	pf := ActionFacts{Op: t.op, Host: host, Key: key}
	out[browserschema.WebActionDescriptionArg] = humanActionDescriptor(pf)
	if !pressKeyCommits(key) {
		return // a focus/navigation/editing key: never commits server state
	}
	fingerprint := ""
	if ps, perr := t.sess.capturePageStateAtFocus(ctx); perr != nil {
		slog.Warn("browser: focus page-state capture failed for a committing press; gating with an empty fingerprint (fail-closed)",
			"toolbox", t.rtName, "op", t.op, "host", host, "key", key, "err", perr.Error())
	} else {
		fingerprint = PageFingerprint(ps)
	}
	out[browserschema.WebActionRequiredArg] = true
	out[browserschema.WebActionKeyArg] = ActionKey(pf)
	out[browserschema.WebActionFingerprintArg] = fingerprint
}

// pressKeyCommits reports whether a `press` key can COMMIT or ACTIVATE a form —
// the only presses that are mutates. Enter/Return submit a focused form (or
// activate its default button); Space activates the focused control (which may
// be a submit button). Every other named key the press op accepts (Tab, Escape,
// Backspace, Delete, the arrows, Home/End, PageUp/PageDown) and every single
// typed character edits or navigates LOCAL page state only — the same "a field
// edit is not a commit" line the consequence judge draws for `type`. The press
// op rejects modifier combos and multi-rune strings (see press()), so matching
// these three literal key names is exhaustive.
func pressKeyCommits(key string) bool {
	switch key {
	case "Enter", "Return", "Space":
		return true
	default:
		return false
	}
}

// classifyMutate reports whether this acting call should be gated as a MUTATE.
// Per ruling E-R3 the deterministic judge runs first as a cheap fast-path: when
// it positively recognizes a benign action (link-follow, search entry, tab
// switch) the model is never consulted. Anything it does not positively clear
// falls to the injected model (Haiku), the PRIMARY classifier, and the call is a
// mutate when the model's P(mutate) reaches the policy threshold. FAIL-CLOSED: a
// judge error is treated as a mutate — the model NEVER authorizes, it only
// classifies, and an uncertain classification must gate.
func (t *browserTool) classifyMutate(ctx context.Context, facts ActionFacts) bool {
	a := judge.Action{
		Op:            facts.Op,
		Role:          facts.Role,
		Text:          facts.Text,
		FormAction:    facts.FormAction,
		Container:     facts.Container,
		Host:          facts.Host,
		PageURL:       facts.PageURL,
		Submits:       facts.Submits,
		FormHasSubmit: facts.FormHasSubmit,
	}
	page := judge.PageContext{URLPath: urlPath(facts.PageURL)}

	// Fast-path: the deterministic judge is documented never to error (it fails
	// closed to mutate for anything not positively benign), but we still treat a
	// non-nil error as fail-closed — only a derr==nil, non-mutate verdict is a
	// confident benign that skips the model call; an error, or a mutate verdict,
	// falls through to the model. A zero Verdict (Tier=="") from an unexpected
	// error must NEVER clear the call, which a bare "Tier != mutate" check would.
	// Diagnostic logging below deliberately OMITS facts.Text and facts.Container.
	// facts.Text is the acted-on element's accessible name; actionFacts already
	// drops the el.value fallback and blanks Text for a credential-filled field,
	// so it should not carry a secret — but it is still page-authored content, so
	// keeping it out of INFO logs is defense in depth. role + formAction (a URL
	// PATH only, query-stripped by urlPath) + the tier/probability give the full
	// diagnostic ("which action, what the model decided") with no content sink.
	// Do not add raw page text back here.
	//
	// submits/formHasSubmit ARE included (unlike Text/Container): both are plain
	// structural booleans about the DOM shape (is this element a submit control;
	// does its enclosing form have one), never user-entered content, so they
	// carry no credential risk. They are exactly what distinguishes "the
	// deterministic path didn't clear this because the role wasn't
	// radio/checkbox/option/text" from "it didn't clear because FormHasSubmit was
	// false" — without them, a live-run diagnosis of a gate the fast-path
	// SHOULD have cleared has to guess between those two causes.
	detV, derr := judge.NewDeterministic().Classify(ctx, a, page)
	if derr == nil && detV.Tier != judge.TierMutate {
		slog.Info("browser: consequence classify = benign (deterministic fast-path cleared it; model NOT consulted)",
			"toolbox", t.rtName, "op", facts.Op, "host", facts.Host, "role", facts.Role,
			"formAction", facts.FormAction, "submits", facts.Submits, "formHasSubmit", facts.FormHasSubmit,
			"detTier", string(detV.Tier))
		return false
	}
	if derr != nil {
		slog.Warn("browser: deterministic fast-path errored unexpectedly; consulting the model (fail-closed)",
			"toolbox", t.rtName, "op", t.op, "err", derr.Error())
	}

	v, err := t.judge.Classify(ctx, a, page)
	if err != nil {
		slog.Warn("browser: consequence judge errored; gating acting call as mutate (fail-closed)",
			"toolbox", t.rtName, "op", t.op, "judge", t.judge.Name(), "err", err.Error())
		return true
	}
	// Gate on EITHER the model's chosen tier being mutate OR its P(mutate)
	// reaching the policy threshold. A response of {tier:mutate, probs:{mutate:
	// 0.05}} is internally inconsistent, but its own best label is "mutate", so a
	// prob-only check that cleared it would ignore the model's explicit call.
	// Fail-closed takes the union, never just the probability.
	gated := v.Tier == judge.TierMutate || v.Probs[judge.TierMutate] >= defaultMutateThreshold
	// See the omission note above: no facts.Text / facts.Container (credential sink).
	slog.Info("browser: consequence classify (model consulted)",
		"toolbox", t.rtName, "op", facts.Op, "host", facts.Host, "role", facts.Role,
		"formAction", facts.FormAction, "submits", facts.Submits, "formHasSubmit", facts.FormHasSubmit,
		"judge", t.judge.Name(),
		"modelTier", string(v.Tier), "pMutate", v.Probs[judge.TierMutate],
		"threshold", defaultMutateThreshold, "gated", gated)
	return gated
}

// stringArg reads a string-valued argument, returning "" when absent or of a
// non-string JSON type.
func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// Origin is shared across one toolbox's tools so toolguard's origin-level
// circuit breaker treats them as a single upstream.
func (t *browserTool) Origin() string { return t.origin }

// Cancel tears down the shared browser connection, interrupting any in-flight
// call and releasing its goroutines; the next tool call reconnects lazily.
func (t *browserTool) Cancel(context.Context) error {
	t.sess.Close()
	return nil
}

// Execute parses the tool's flat arguments and dispatches by op. Tool-level
// failures (bad scheme, policy refusal, stale ref) are returned as IsError
// results, never as a Go error — a Go error is reserved for a runner-side
// dispatch fault.
func (t *browserTool) Execute(ctx context.Context, raw json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var a struct {
		URL            string `json:"url"`
		Page           int    `json:"page"`
		Ref            string `json:"ref"`
		Text           string `json:"text"`
		Direction      string `json:"direction"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Filter         string `json:"filter"`
		Host           string `json:"host"`
		Key            string `json:"key"`
		Option         string `json:"option"`
		Kind           string `json:"kind"`
		Prompt         string `json:"prompt"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return errResult("invalid arguments: %v", err), nil
		}
	}
	switch t.op {
	case "navigate":
		if a.URL == "" {
			return errResult("navigate requires a url"), nil
		}
		return t.navigateTo(ctx, a.URL), nil
	case "read":
		return t.read(ctx, a.Page, a.Ref), nil
	case "find":
		return t.find(ctx, a.Text), nil
	case "screenshot":
		return t.screenshot(ctx, a.Ref), nil
	case "follow":
		return t.follow(ctx, a.Ref), nil
	case "scroll":
		return t.scroll(ctx, a.Direction, a.Ref), nil
	case "back":
		return t.back(ctx), nil
	case "wait_for":
		return t.waitFor(ctx, a.Text, a.Ref, a.TimeoutSeconds), nil
	case "query_logins":
		return t.queryLogins(a.Filter, a.Host, a.Page), nil
	case "type":
		return t.afterAct(ctx, t.typeInto(ctx, a.Ref, a.Text)), nil
	case "click":
		return t.afterAct(ctx, t.click(ctx, a.Ref)), nil
	case "press":
		return t.afterAct(ctx, t.press(ctx, a.Key)), nil
	case "select":
		return t.afterAct(ctx, t.selectOption(ctx, a.Ref, a.Option)), nil
	case "submit":
		return t.afterAct(ctx, t.submit(ctx, a.Ref)), nil
	case requestHumanStepOp:
		return t.requestHumanStep(ctx, a.Kind, a.Prompt, a.Ref), nil
	default:
		return errResult("unknown browser tool %q", t.op), nil
	}
}

// StateCapturer is implemented by a browser tool whose toolbox is `persistent`:
// it captures the live cookie jar + localStorage into the per-session state
// Secret. The runner calls it on graceful stop (SIGTERM) so the last login state
// precedes a reap (spec §2.3). Every tool of one toolbox shares one Session, so
// the runner dedupes by Origin and calls this once per toolbox. An `ephemeral`
// toolbox's tool implements it as a no-op (nil store), so a type assertion that
// matches is always safe to invoke.
type StateCapturer interface {
	CaptureState(ctx context.Context) error
}

var _ StateCapturer = (*browserTool)(nil)

// CaptureState implements StateCapturer: it persists the shared Session's state.
// A no-op for an ephemeral toolbox. Returned errors let the runner log-and-continue.
func (t *browserTool) CaptureState(ctx context.Context) error {
	return t.sess.captureAndPersist(ctx)
}

// afterAct persists session state after a successful acting call for a
// `persistent` toolbox (spec §2.3 "after every acting tool call"). Best-effort:
// a capture failure is LOGGED with toolbox + host context and the acting result
// is returned UNCHANGED — a persistence blip must never turn a successful page
// action into a tool error (repo rule: log + continue; the next call retries).
// A no-op for an ephemeral toolbox (nil store), so the common case costs one
// nil-check.
func (t *browserTool) afterAct(ctx context.Context, res agenttool.Result) agenttool.Result {
	if err := t.sess.captureAndPersist(ctx); err != nil {
		slog.Warn("browser: capture after acting call failed; continuing",
			"toolbox", t.rtName, "op", t.op, "host", t.sess.CurrentHost(), "err", err.Error())
	}
	return res
}

// requestHumanStep asks a person to complete a sign-in step the agent cannot do
// itself. It returns a STATUS only — never a code or secret.
//
//   - kind=="confirm": post a login_human_step card with a Done button and BLOCK
//     until the person confirms, declines, or the approval window lapses (a push
//     approval, say). A timeout is fail-closed: the call reports "timed out", it
//     does NOT silently succeed.
//   - no kind (or "captcha"): the agent hit a CAPTCHA or other step it cannot do.
//     Surface a one-way notice and report that human intervention is needed. This
//     does not block — a person takes over out of band.
//
// kind=="code" (ref REQUIRED): post a login_human_step card asking the person to
// type an emailed/SMS code, BLOCK until they answer, then type the code into the
// element `ref` names via the CDP fill path — the same InsertText the type op uses,
// but sourced from the human's code, not a vault credential. The code is typed by
// THIS tool (the runner), never by a model-issued type call, and the result is a
// STATUS only: the code never enters the tool result, the transcript, a log, or the
// model's context. A timeout is fail-closed.
func (t *browserTool) requestHumanStep(ctx context.Context, kind, prompt, ref string) agenttool.Result {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return errResult("request_human_step requires a prompt describing what the person should do")
	}
	if t.humanStep == nil {
		// Hand-built tool with no requester (a unit test), or a session with no
		// human-step wiring. Fail loud rather than nil-panic.
		return errResult("request_human_step is unavailable: no human-step requester is configured for this session")
	}

	switch kind {
	case "confirm":
		outcome, err := t.humanStep.Confirm(ctx, prompt, t.ScreenshotCapturer())
		if err != nil {
			return errResult("request_human_step failed: %v", err)
		}
		switch {
		case outcome.Approved:
			// Status only — the tool never reveals how the step was completed.
			return okResult("the person confirmed the sign-in step; you can continue")
		case outcome.Denied:
			return errResult("request_human_step: the person declined to complete the sign-in step")
		case outcome.TimedOut:
			// Fail-closed: a lapsed deadline is NOT a success.
			return errResult("request_human_step: timed out waiting for the person to complete the sign-in step")
		default:
			return errResult("request_human_step: no decision was recorded")
		}
	case "code":
		return t.requestHumanStepCode(ctx, prompt, ref)
	case "", "captcha":
		// No actionable kind: a CAPTCHA, or any step the agent cannot do. Surface a
		// notice and tell the model a person must step in.
		if err := t.humanStep.Captcha(ctx, prompt, t.ScreenshotCapturer()); err != nil {
			return errResult("request_human_step: could not notify anyone that human intervention is needed: %v", err)
		}
		return errResult("request_human_step: this step needs a person and cannot be completed automatically; a notice has been sent asking someone to take over in the browser")
	default:
		return errResult("request_human_step: unknown kind %q — use \"confirm\" or \"code\", or omit kind to report a CAPTCHA or other step needing a person", kind)
	}
}

// requestHumanStepCode drives the kind:code handoff: block for the person's typed
// code, then type it into the element `ref` names. ref is REQUIRED (the code has to
// go somewhere), checked before blocking a human. The result is a STATUS only; the
// code is never echoed into the result, and the field it was typed into is marked
// for C5 blanking so a later read/screenshot does not surface it either.
func (t *browserTool) requestHumanStepCode(ctx context.Context, prompt, ref string) agenttool.Result {
	if ref == "" {
		return errResult("request_human_step: kind=\"code\" requires a ref naming the input to type the code into — read the page first and pass the code field's ref")
	}
	outcome, err := t.humanStep.RequestCode(ctx, prompt, t.ScreenshotCapturer())
	if err != nil {
		return errResult("request_human_step failed: %v", err)
	}
	switch {
	case outcome.TimedOut:
		// Fail-closed: a lapsed deadline is NOT a success, and nothing is typed.
		return errResult("request_human_step: timed out waiting for the person to provide the code")
	case outcome.Denied:
		return errResult("request_human_step: the person declined to provide the code")
	case outcome.Approved:
		if outcome.Code == "" {
			// Approved but no code supplied — fail closed rather than type nothing
			// and report success.
			return errResult("request_human_step: the person approved but provided no code")
		}
		// The runner types the human's code into the page here — NOT a model-issued
		// type call. The code never enters the tool result below.
		if res := t.typeHumanCode(ctx, ref, outcome.Code); res.IsError {
			return res
		}
		return okResult(fmt.Sprintf("the person provided the code; it was entered into ref %s", ref))
	default:
		return errResult("request_human_step: no decision was recorded")
	}
}

// typeHumanCode focuses the element `ref` names and inserts `code` via the same
// CDP InsertText path typeInto uses for a vault credential — but the source here is
// the human's typed code, and there is no {{login...}} placeholder or password-slot
// gate to apply (the human chose the field by reading the page, and the model never
// saw the value). The filled field is marked for C5 blanking: the code is a secret,
// so a subsequent read/screenshot must not reveal it. The returned Result never
// contains `code`.
func (t *browserTool) typeHumanCode(ctx context.Context, ref, code string) agenttool.Result {
	var backendNodeID int64
	var handled *agenttool.Result
	if runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			r := errResult("ref %s is stale; call read again", ref)
			handled = &r
			return nil
		}
		backendNodeID = e.backendNodeID
		if ae := runAction(c, func(ac context.Context) error {
			return dom.Focus().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ac)
		}); ae != nil {
			return ae
		}
		return runAction(c, func(ac context.Context) error {
			return input.InsertText(code).Do(ac)
		})
	}); runErr != nil {
		// runErr carries only the CDP transport error, never the code value.
		return errResult("entering the code failed: %v", runErr)
	}
	if handled != nil {
		return *handled
	}
	// The code is a secret: blank the field so read/find/screenshot redact it until
	// a real navigation clears the blanking (spec §3.6), exactly as a filled
	// credential field is treated.
	t.sess.markBlanked(backendNodeID)
	return okResult(fmt.Sprintf("entered the code into ref %s", ref))
}

// decide evaluates host against the toolbox's egress policy.
func (t *browserTool) decide(host string) browserpolicy.Decision {
	return browserpolicy.Decide(t.mode, t.sites, host)
}

// policyError renders the verbatim policy-refusal contract text.
func (t *browserTool) policyError(host, rule string) agenttool.Result {
	return errResult("host %s is not allowed by browser toolbox %s (rule: %s)", host, t.rtName, rule)
}

// navigateTo pre-checks scheme + host policy BEFORE touching Chrome, then
// navigates. A proxy-denied load (a redirect to an off-policy host) is reported
// as the policy error rather than the Chrome error page.
func (t *browserTool) navigateTo(ctx context.Context, rawURL string) agenttool.Result {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errResult("only http and https URLs can be opened")
	}
	host := u.Hostname()
	if dec := t.decide(host); !dec.Allowed {
		return t.policyError(host, dec.Rule)
	}

	var out agenttool.Result
	runErr := t.sess.do(ctx, navigateTimeout, func(c context.Context) error {
		t.sess.resetBlocked()
		var navErrText string
		if e := runAction(c, func(ac context.Context) error {
			_, _, et, _, e := page.Navigate(rawURL).Do(ac)
			navErrText = et
			return e
		}); e != nil {
			return e
		}
		if navErrText != "" {
			if isProxyDenial(navErrText) {
				// A host in the (possibly redirected) chain was denied by the
				// gate's egress proxy. Name the real blocked host when the
				// network listener captured it; else fall back to the landed URL.
				bh := t.sess.blockedHost()
				if bh == "" {
					bh = t.landedHost(c, host)
				}
				rule := t.decide(bh).Rule
				if rule == "" {
					rule = "blocked by egress policy"
				}
				out = t.policyError(bh, rule)
				return nil
			}
			out = errResult("navigation failed: %s", navErrText)
			return nil
		}
		finalURL, title := t.locationTitle(c)
		t.sess.clearRefs()
		// A navigate/back leaves the old page behind, and with it any field a
		// credential placeholder was typed into — clear the blanking alongside
		// refs so a fresh page is read/screenshotted normally (spec §3.6).
		t.sess.clearBlanked()
		out = okResult(fmt.Sprintf("loaded %s — title: %s", finalURL, title))
		return nil
	})
	if runErr != nil {
		return errResult("navigation failed: %v", runErr)
	}
	return out
}

// landedHost best-effort reads the host Chrome ended on after a failed
// navigation, for naming the policy error; falls back to the requested host.
func (t *browserTool) landedHost(c context.Context, fallback string) string {
	var loc string
	_ = runAction(c, func(ac context.Context) error { return chromedp.Location(&loc).Do(ac) })
	if lu, e := url.Parse(loc); e == nil && lu.Hostname() != "" {
		return lu.Hostname()
	}
	return fallback
}

// locationTitle reads the current URL and title, ignoring individual errors
// (either may be empty on an odd page without failing the whole navigation).
func (t *browserTool) locationTitle(c context.Context) (string, string) {
	var loc, title string
	_ = runAction(c, func(ac context.Context) error {
		_ = chromedp.Location(&loc).Do(ac)
		_ = chromedp.Title(&title).Do(ac)
		return nil
	})
	return loc, title
}

func (t *browserTool) read(ctx context.Context, pageNum int, ref string) agenttool.Result {
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		var startBackend int64
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			startBackend = e.backendNodeID
		}
		lines, refs, e := snapshot(c, startBackend, t.sess.nextRefGen(), t.sess.blankedSet())
		if e != nil {
			return e
		}
		t.sess.setRefs(refs)
		pages := paginate(renderOutline(lines), maxPageBytes)
		p := pageNum
		if p <= 0 {
			p = 1
		}
		if p > len(pages) {
			out = errResult("page %d is out of range", p)
			return nil
		}
		out = okResult(fmt.Sprintf("%s\n\npage %d of %d", pages[p-1], p, len(pages)))
		return nil
	})
	if runErr != nil {
		return errResult("read failed: %v", runErr)
	}
	return out
}

func (t *browserTool) find(ctx context.Context, text string) agenttool.Result {
	if strings.TrimSpace(text) == "" {
		return errResult("find requires non-empty text")
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		lines, refs, e := snapshot(c, 0, t.sess.nextRefGen(), t.sess.blankedSet())
		if e != nil {
			return e
		}
		t.sess.setRefs(refs)
		needle := strings.ToLower(text)
		matched := make([]string, 0, 50)
		for _, ln := range lines {
			if strings.Contains(strings.ToLower(ln.name), needle) {
				// renderLine (not a bare role/name format) so a blanked field's
				// matched line carries the same redaction marker read's outline
				// does — find must not reveal what read would not (spec §3.6).
				matched = append(matched, renderLine(ln))
				if len(matched) >= 50 {
					break
				}
			}
		}
		if len(matched) == 0 {
			out = okResult(fmt.Sprintf("no matches for %q", text))
			return nil
		}
		out = okResult(strings.Join(matched, "\n"))
		return nil
	})
	if runErr != nil {
		return errResult("find failed: %v", runErr)
	}
	return out
}

func (t *browserTool) follow(ctx context.Context, ref string) agenttool.Result {
	if ref == "" {
		return errResult("follow requires a ref")
	}
	var handled *agenttool.Result
	var href string
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			r := errResult("ref %s is stale; call read again", ref)
			handled = &r
			return nil
		}
		if e.role != "link" {
			r := errResult("ref %s is not a link", ref)
			handled = &r
			return nil
		}
		var node *cdp.Node
		if ae := runAction(c, func(ac context.Context) error {
			var e2 error
			node, e2 = dom.DescribeNode().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			return e2
		}); ae != nil {
			return ae
		}
		base, _ := t.locationTitle(c)
		href = resolveHref(base, attrValue(node, "href"))
		return nil
	})
	if runErr != nil {
		return errResult("follow failed: %v", runErr)
	}
	if handled != nil {
		return *handled
	}
	if href == "" {
		return errResult("ref %s has no link target", ref)
	}
	return t.navigateTo(ctx, href)
}

func (t *browserTool) back(ctx context.Context) agenttool.Result {
	var out agenttool.Result
	runErr := t.sess.do(ctx, navigateTimeout, func(c context.Context) error {
		if e := runAction(c, func(ac context.Context) error { return chromedp.NavigateBack().Do(ac) }); e != nil {
			return e
		}
		finalURL, title := t.locationTitle(c)
		t.sess.clearRefs()
		// A navigate/back leaves the old page behind, and with it any field a
		// credential placeholder was typed into — clear the blanking alongside
		// refs so a fresh page is read/screenshotted normally (spec §3.6).
		t.sess.clearBlanked()
		if lu, e := url.Parse(finalURL); e == nil && lu.Hostname() != "" {
			if dec := t.decide(lu.Hostname()); !dec.Allowed {
				out = t.policyError(lu.Hostname(), dec.Rule)
				return nil
			}
		}
		out = okResult(fmt.Sprintf("loaded %s — title: %s", finalURL, title))
		return nil
	})
	if runErr != nil {
		return errResult("back failed: %v", runErr)
	}
	return out
}

func (t *browserTool) scroll(ctx context.Context, direction, ref string) agenttool.Result {
	if ref == "" && direction != "up" && direction != "down" {
		return errResult(`scroll requires direction "up" or "down" (or a ref to scroll into view)`)
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			if ae := runAction(c, func(ac context.Context) error {
				return dom.ScrollIntoViewIfNeeded().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			}); ae != nil {
				return ae
			}
			out = okResult(fmt.Sprintf("scrolled ref %s into view", ref))
			return nil
		}
		delta := "window.innerHeight"
		if direction == "up" {
			delta = "-window.innerHeight"
		}
		var ignore int
		if ae := runAction(c, func(ac context.Context) error {
			return chromedp.Evaluate("window.scrollBy(0, "+delta+"); 0", &ignore).Do(ac)
		}); ae != nil {
			return ae
		}
		out = okResult("scrolled " + direction)
		return nil
	})
	if runErr != nil {
		return errResult("scroll failed: %v", runErr)
	}
	return out
}

// screenshot captures a PNG (validating ref/page state along the way — the
// box-model lookup above fails loudly on a stale ref) but does NOT return the
// image bytes to the model: agenttool.Result carries only text today, so a
// base64 data-URI would just be an unrenderable, budget-burning text blob. The
// capture is kept — it is still useful as a liveness/validity check — and the
// result is a short text stub instead. acceptsImages (synthesize.go) still
// gates whether this tool is offered at all; that plumbing is correct as-is
// and is exactly what should flip this stub over to real image delivery once
// agenttool.Result grows an image field.
func (t *browserTool) screenshot(ctx context.Context, ref string) agenttool.Result {
	// Refused while ANY credential-filled field is still blanked (spec §3.6) —
	// global, not scoped to ref: a screenshot of one element would still render
	// the rest of the page's pixels including the filled field, so the refusal
	// does not depend on which element ref names. Checked before touching Chrome
	// at all, matching navigateTo's policy pre-check. The blanked set clears only
	// on a real navigation (navigate/back/follow), so after a submit that
	// navigated the agent re-enables screenshot by navigating; until then this may
	// over-refuse — deliberately fail-safe, refusing a screenshot is always the
	// safe direction.
	if t.sess.anyBlanked() {
		return errResult("screenshot refused: a credential-filled field is still on the page and the form " +
			"has not been submitted; submit the form or navigate away before capturing a screenshot")
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		var data []byte
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			var box *dom.BoxModel
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				box, e2 = dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				data, e2 = page.CaptureScreenshot().WithClip(clipFromBox(box)).Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
		} else {
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				data, e2 = page.CaptureScreenshot().Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
		}
		out = okResult(fmt.Sprintf("screenshot captured (%d bytes PNG); image delivery to the model is not yet supported", len(data)))
		return nil
	})
	if runErr != nil {
		return errResult("screenshot failed: %v", runErr)
	}
	return out
}

func (t *browserTool) waitFor(ctx context.Context, text, ref string, seconds int) agenttool.Result {
	if strings.TrimSpace(text) == "" && ref == "" {
		return errResult("wait_for requires one of text or ref")
	}
	d, clamped := clampWaitTimeout(seconds)
	suffix := ""
	if clamped {
		suffix = " (timeout clamped to 30s)"
	}

	// Resolve a ref target once up front; a stale ref is refused immediately.
	var targetBackend int64
	if ref != "" {
		var staleRes *agenttool.Result
		if e := t.sess.do(ctx, shortTimeout, func(context.Context) error {
			entry, ok := t.sess.lookupRef(ref)
			if !ok {
				r := errResult("ref %s is stale; call read again", ref)
				staleRes = &r
				return nil
			}
			targetBackend = entry.backendNodeID
			return nil
		}); e != nil {
			return errResult("wait_for failed: %v", e)
		}
		if staleRes != nil {
			return *staleRes
		}
	}

	deadline := time.Now().Add(d)
	needle := strings.ToLower(text)
	for {
		var found bool
		if e := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
			lines, refs, se := snapshot(c, 0, t.sess.nextRefGen(), t.sess.blankedSet())
			if se != nil {
				return se
			}
			t.sess.setRefs(refs)
			for _, ln := range lines {
				if text != "" && strings.Contains(strings.ToLower(ln.name), needle) {
					found = true
					break
				}
				if ref != "" && ln.backendNodeID == targetBackend {
					found = true
					break
				}
			}
			return nil
		}); e != nil {
			return errResult("wait_for failed: %v", e)
		}
		if found {
			return okResult(fmt.Sprintf("found%s", suffix))
		}
		if time.Now().After(deadline) {
			return okResult(fmt.Sprintf("not found within %ds%s", int(d.Seconds()), suffix))
		}
		select {
		case <-ctx.Done():
			return errResult("wait_for cancelled: %v", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// typeInto types text into the input a prior read's ref names. text may embed
// {{login.<host>.username|password|otp}} placeholders, which the credential
// filler substitutes IN PLACE, strictly inside this Execute — so the transcript
// (which records the model's raw args before dispatch) only ever sees the
// placeholder, never the resolved secret.
//
// The substitution is fail-closed on the three §3.5 rules: the placeholder host
// must be the host the page is actually on, must have a catalog entry in this
// toolbox's identity, and a password may only land in a type=password input. The
// password-input check reads the live element type via dom.DescribeNode BEFORE
// the filler runs, so a misdirected password never even Fetches.
//
// Refs are DELIBERATELY not dropped afterward (unlike click/submit): typing does
// not navigate, and a login flow fills a username, a password and then submits
// from one read — clearing refs after each type would force a re-read between
// fields. A type that DID rebuild the DOM is caught by the next op's stale-ref
// check.
//
// The tool result never echoes text (filled or not): it names the ref only, so a
// successful fill leaves no secret in the result, the transcript, or any log.
func (t *browserTool) typeInto(ctx context.Context, ref, text string) agenttool.Result {
	if ref == "" {
		return errResult("type requires a ref")
	}
	if text == "" {
		return errResult("type requires text")
	}

	// The host the page is actually on — the ONLY host a placeholder may name
	// (rule 1). Read before the ref resolve; empty (about:blank) fails rule 1.
	pageHost := t.sess.CurrentHost()

	// Resolve the ref to a backend node and read whether it is a password input,
	// in one browser round-trip.
	var backendNodeID int64
	var inputIsPassword bool
	var handled *agenttool.Result
	if runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			r := errResult("ref %s is stale; call read again", ref)
			handled = &r
			return nil
		}
		backendNodeID = e.backendNodeID
		var node *cdp.Node
		if ae := runAction(c, func(ac context.Context) error {
			var e2 error
			node, e2 = dom.DescribeNode().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ac)
			return e2
		}); ae != nil {
			return ae
		}
		inputIsPassword = isPasswordInput(node)
		return nil
	}); runErr != nil {
		return errResult("type failed: %v", runErr)
	}
	if handled != nil {
		return *handled
	}

	// Substitute placeholders. Runs OUTSIDE do() (no browser lock held across the
	// vault Fetch) and strictly inside Execute. filler is nil only if a tool was
	// hand-built without one; guard rather than panic.
	if t.filler == nil {
		return errResult("type failed: no credential filler is configured")
	}
	filled, kinds, ferr := t.filler.Fill(ctx, pageHost, inputIsPassword, text)
	if ferr != nil {
		// ferr carries only host/field names, never a credential value.
		return errResult("type failed: %v", ferr)
	}

	// Focus the element and insert the (possibly substituted) text. InsertText
	// fires the page's input events, so onchange/oninput handlers run.
	if runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		if ae := runAction(c, func(ac context.Context) error {
			return dom.Focus().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ac)
		}); ae != nil {
			return ae
		}
		return runAction(c, func(ac context.Context) error {
			return input.InsertText(filled).Do(ac)
		})
	}); runErr != nil {
		return errResult("type failed: %v", runErr)
	}

	// Record the field for C5's blanking pass IFF a credential actually landed in
	// it. A plain-text type (no placeholder) leaves nothing to blank.
	if kinds.any() {
		t.sess.markBlanked(backendNodeID)
	}
	return okResult(fmt.Sprintf("typed into ref %s", ref))
}

// isPasswordInput reports whether node is an <input type="password">. Anything
// else (a text input, a textarea, a non-input element, or a nil node) is false,
// which fail-closes rule 3: a password placeholder is refused unless the target
// is unmistakably a password field.
func isPasswordInput(node *cdp.Node) bool {
	if node == nil || !strings.EqualFold(node.NodeName, "input") {
		return false
	}
	return strings.EqualFold(attrValue(node, "type"), "password")
}

// click dispatches a native click on the element a prior read's ref names,
// firing its onclick handler and any form submission a submit button triggers.
// The snapshot's refs are dropped afterward: a click may navigate or rebuild the
// DOM, so every ref it described must be re-minted by a fresh read.
func (t *browserTool) click(ctx context.Context, ref string) agenttool.Result {
	if ref == "" {
		return errResult("click requires a ref")
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			out = errResult("ref %s is stale; call read again", ref)
			return nil
		}
		threw, err := callOnNode(c, e.backendNodeID, `function(){ this.click(); }`)
		if err != nil {
			return err
		}
		if threw {
			out = errResult("click on ref %s failed", ref)
			return nil
		}
		t.sess.clearRefs()
		out = okResult(fmt.Sprintf("clicked ref %s", ref))
		return nil
	})
	if runErr != nil {
		return errResult("click failed: %v", runErr)
	}
	return out
}

// press dispatches one key to the page. A single character types that character;
// a recognized named key (Enter, Tab, Escape, the arrows, …) presses that key.
// Anything else is refused rather than typed letter-by-letter — chromedp.KeyEvent
// would otherwise send "Enter" as five keystrokes. Refs are dropped afterward: a
// keypress (Enter submitting a form, say) may change the page.
func (t *browserTool) press(ctx context.Context, key string) agenttool.Result {
	if strings.TrimSpace(key) == "" {
		return errResult("press requires a key")
	}
	seq, ok := namedKeys[key]
	if !ok {
		if utf8.RuneCountInString(key) != 1 {
			return errResult("press: unknown key %q; use a single character or a named key "+
				"(Enter, Tab, Escape, Backspace, Delete, ArrowUp/Down/Left/Right, Home, End, PageUp, PageDown, Space)", key)
		}
		seq = key
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		if err := runAction(c, func(ac context.Context) error { return chromedp.KeyEvent(seq).Do(ac) }); err != nil {
			return err
		}
		t.sess.clearRefs()
		out = okResult(fmt.Sprintf("pressed %s", key))
		return nil
	})
	if runErr != nil {
		return errResult("press failed: %v", runErr)
	}
	return out
}

// selectOption sets a <select>'s value to the option whose value, label or text
// matches option, firing input + change so the page's handlers run. A ref that is
// not a <select>, or an option that matches nothing, is a tool error. Refs are
// dropped afterward since a change handler may rebuild the DOM.
func (t *browserTool) selectOption(ctx context.Context, ref, option string) agenttool.Result {
	if ref == "" {
		return errResult("select requires a ref")
	}
	if option == "" {
		return errResult("select requires an option")
	}
	// JSON-encode the option INTO the function body rather than passing it as a
	// call argument: a JSON string literal is also a valid JS string literal, so
	// this is injection-safe, and it sidesteps the CallArgument value encoding.
	optJSON, err := json.Marshal(option)
	if err != nil {
		return errResult("select: encode option: %v", err)
	}
	funcDecl := fmt.Sprintf(`function(){
  var want = %s;
  if (this.options === undefined || this.options === null) { throw new Error("not a select"); }
  for (var i = 0; i < this.options.length; i++) {
    var o = this.options[i];
    if (o.value === want || o.label === want || o.text === want) {
      this.value = o.value;
      this.dispatchEvent(new Event("input", {bubbles:true}));
      this.dispatchEvent(new Event("change", {bubbles:true}));
      return;
    }
  }
  throw new Error("no matching option");
}`, optJSON)
	var out agenttool.Result
	runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			out = errResult("ref %s is stale; call read again", ref)
			return nil
		}
		threw, err := callOnNode(c, e.backendNodeID, funcDecl)
		if err != nil {
			return err
		}
		if threw {
			out = errResult("select failed: ref %s is not a <select>, or has no option matching %q", ref, option)
			return nil
		}
		t.sess.clearRefs()
		out = okResult(fmt.Sprintf("selected %q on ref %s", option, ref))
		return nil
	})
	if runErr != nil {
		return errResult("select failed: %v", runErr)
	}
	return out
}

// submit submits the form the ref belongs to (its own form, or the nearest
// enclosing one), via requestSubmit so the form's submit handler and validation
// run. A ref outside any form is a tool error. Refs are dropped afterward: a
// submit usually navigates. It deliberately does NOT touch the blanked set: a
// submit is not a reliable "the fields are gone" signal — an SPA onsubmit can
// pushState a new route while keeping the password input mounted, and neither a
// URL change nor AX-tree absence distinguishes that from a real navigation. The
// blanked set therefore clears only on a genuine document replacement
// (navigate/back/follow). After a submit that really did navigate, the old
// fields are gone and nothing redacts them on the new page; the stale blanked
// ids just linger harmlessly (at worst over-refusing screenshot) until the next
// navigation clears them (see Session.clearBlanked).
func (t *browserTool) submit(ctx context.Context, ref string) agenttool.Result {
	if ref == "" {
		return errResult("submit requires a ref")
	}
	const funcDecl = `function(){
  var f = this.form || (this.closest ? this.closest("form") : null);
  if (!f) { throw new Error("not in a form"); }
  if (f.requestSubmit) { f.requestSubmit(); } else { f.submit(); }
}`
	var out agenttool.Result
	runErr := t.sess.do(ctx, navigateTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			out = errResult("ref %s is stale; call read again", ref)
			return nil
		}
		threw, err := callOnNode(c, e.backendNodeID, funcDecl)
		if err != nil {
			return err
		}
		if threw {
			out = errResult("submit failed: ref %s is not inside a form", ref)
			return nil
		}
		t.sess.clearRefs()
		out = okResult(fmt.Sprintf("submitted the form containing ref %s", ref))
		return nil
	})
	if runErr != nil {
		return errResult("submit failed: %v", runErr)
	}
	return out
}

// callOnNode resolves a prior snapshot's backend node to a live JS handle and
// invokes funcDecl (a `function(){…}` declaration) on it with the element as
// `this`. It reports whether the function THREW (which the caller renders as a
// tool error), separately from a transport error (a real CDP fault, surfaced as
// a Go error). The remote handle is released so repeated acting calls do not leak
// objects. Must be called from within Session.do (ctx bound to the browser tab).
func callOnNode(ctx context.Context, backendNodeID int64, funcDecl string) (threw bool, err error) {
	var obj *runtime.RemoteObject
	if e := runAction(ctx, func(ac context.Context) error {
		var e2 error
		obj, e2 = dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ac)
		return e2
	}); e != nil {
		return false, e
	}
	if obj == nil || obj.ObjectID == "" {
		return false, fmt.Errorf("element is no longer attached to the page")
	}
	defer func() {
		_ = runAction(ctx, func(ac context.Context) error {
			return runtime.ReleaseObject(obj.ObjectID).Do(ac)
		})
	}()
	var exc *runtime.ExceptionDetails
	if e := runAction(ctx, func(ac context.Context) error {
		_, ed, e2 := runtime.CallFunctionOn(funcDecl).WithObjectID(obj.ObjectID).Do(ac)
		exc = ed
		return e2
	}); e != nil {
		return false, e
	}
	return exc != nil, nil
}

// loginsPerPage bounds query_logins pagination. A fixed entry count rather
// than read's byte budget, since catalog entries are short and uniform in
// shape, unlike an accessibility outline.
const loginsPerPage = 20

// queryLoginsEntry is one catalog entry's query_logins-facing projection:
// {hosts, label} and NOTHING else about the login (spec §3.2) — never the
// catalog's opaque ID, which BrowserLoginRef's own doc comment says callers
// must not parse or infer structure from, and which no downstream consumer
// needs: the credential filler (Phase C Task C4) resolves a stored login by
// HOST (`{{login.<host>.*}}`), never by this id.
type queryLoginsEntry struct {
	Hosts []string `json:"hosts"`
	Label string   `json:"label,omitempty"`
}

// queryLogins lists the bound identity's catalog entries matching filter
// (case-insensitive substring over each entry's hosts and label), paginated,
// and — independently, when host is non-empty — answers whether that
// hostname is within the toolbox's egress policy. Both reuse exactly the
// data query_logins was offered because of: t.catalog (Phase B status) and
// t.decide (browserpolicy.Decide over the same EffectiveSites the gate
// itself enforces against), so this answer can never disagree with what a
// navigate call to that host would actually do.
func (t *browserTool) queryLogins(filter, host string, pageNum int) agenttool.Result {
	var lines []string
	if host != "" {
		dec := t.decide(host)
		if dec.Allowed {
			lines = append(lines, fmt.Sprintf("host %s is allowed by this toolbox's policy", host))
		} else {
			lines = append(lines, fmt.Sprintf("host %s is NOT allowed by this toolbox's policy (rule: %s)", host, dec.Rule))
		}
	}

	needle := strings.ToLower(strings.TrimSpace(filter))
	matched := make([]queryLoginsEntry, 0, len(t.catalog))
	for _, c := range t.catalog {
		if needle != "" && !loginMatches(c, needle) {
			continue
		}
		hosts := make([]string, 0, len(c.Hosts))
		for _, h := range c.Hosts {
			hosts = append(hosts, h.Host)
		}
		matched = append(matched, queryLoginsEntry{Hosts: hosts, Label: c.Label})
	}

	totalPages := (len(matched) + loginsPerPage - 1) / loginsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	p := pageNum
	if p <= 0 {
		p = 1
	}
	if p > totalPages {
		return errResult("page %d is out of range", p)
	}
	start := (p - 1) * loginsPerPage
	end := start + loginsPerPage
	if end > len(matched) {
		end = len(matched)
	}

	page, err := json.Marshal(matched[start:end])
	if err != nil {
		return errResult("query_logins: marshal result: %v", err)
	}
	lines = append(lines, fmt.Sprintf("%s\n\npage %d of %d", string(page), p, totalPages))
	return okResult(strings.Join(lines, "\n\n"))
}

// loginMatches reports whether needle (already lowercased) is a substring of
// c's label or any of its hosts, case-insensitively.
func loginMatches(c spiceboxv1alpha1.BrowserLoginRef, needle string) bool {
	if strings.Contains(strings.ToLower(c.Label), needle) {
		return true
	}
	for _, h := range c.Hosts {
		if strings.Contains(strings.ToLower(h.Host), needle) {
			return true
		}
	}
	return false
}

// clampWaitTimeout clamps a requested wait to [1,30]s. An unset (0) value
// defaults to the 30s maximum and is NOT reported as clamped; anything outside
// the range is clamped and reported so the result can note it.
func clampWaitTimeout(seconds int) (time.Duration, bool) {
	if seconds == 0 {
		return waitDefaultTimeout, false
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d > waitMaxTimeout:
		return waitMaxTimeout, true
	case d < waitMinTimeout:
		return waitMinTimeout, true
	default:
		return d, false
	}
}

// paginate splits s into pages no larger than limit bytes, never splitting a
// line. A single over-long line becomes its own page. Always returns at least
// one page.
func paginate(s string, limit int) []string {
	if s == "" {
		return []string{""}
	}
	var pages []string
	var cur strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		if cur.Len() > 0 && cur.Len()+len(ln)+1 > limit {
			pages = append(pages, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(ln)
	}
	if cur.Len() > 0 {
		pages = append(pages, cur.String())
	}
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

// isProxyDenial reports whether a Chrome navigation errorText is a gate/proxy
// refusal (a CONNECT the gate denied, or a proxy error), as opposed to an
// ordinary load failure.
func isProxyDenial(errText string) bool {
	return strings.Contains(errText, "ERR_TUNNEL_CONNECTION_FAILED") || strings.Contains(errText, "ERR_PROXY")
}

// resolveHref resolves a possibly-relative href against the current page URL.
func resolveHref(base, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	r, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(r).String()
}

// hostOf returns the hostname of a URL, or "" if it cannot be parsed.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// attrValue reads an element attribute from a DOM node's flat [name,value,…]
// attribute array; "" if the node is nil or the attribute is absent.
func attrValue(node *cdp.Node, name string) string {
	if node == nil {
		return ""
	}
	for i := 0; i+1 < len(node.Attributes); i += 2 {
		if node.Attributes[i] == name {
			return node.Attributes[i+1]
		}
	}
	return ""
}

// clipFromBox converts a DOM content box quad into a screenshot clip viewport.
func clipFromBox(box *dom.BoxModel) *page.Viewport {
	if box == nil || len(box.Content) < 8 {
		return &page.Viewport{Scale: 1}
	}
	q := box.Content
	minX, minY := q[0], q[1]
	maxX, maxY := q[0], q[1]
	for i := 0; i+1 < len(q); i += 2 {
		minX, maxX = min(minX, q[i]), max(maxX, q[i])
		minY, maxY = min(minY, q[i+1]), max(maxY, q[i+1])
	}
	return &page.Viewport{X: minX, Y: minY, Width: maxX - minX, Height: maxY - minY, Scale: 1}
}

func errResult(format string, a ...any) agenttool.Result {
	return agenttool.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

func okResult(content string) agenttool.Result {
	return agenttool.Result{Content: content}
}

// runAction runs a single cdproto action against the chromedp-bound context.
func runAction(ctx context.Context, fn func(context.Context) error) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(fn))
}
