// Package toolcheck is the SpiceDB implementation of the tool-call
// authorization gate. It owns everything wire-shaped about that decision —
// v1.CheckPermissionRequest construction, consistency-mode selection, the
// caveat context for a per-session grant — so pkg/authz's root stays the
// backend-neutral vocabulary (Permission, Inputs, Result) a second backend
// could implement against.
//
// Checker satisfies engine.ToolChecker and hooks.ToolCallChecker
// structurally; production wiring passes it a *spicedb.Client.
package toolcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// Client is the SpiceDB CheckPermission surface Checker uses.
// Satisfied by *spicedb.Client and by test fakes.
type Client interface {
	// CheckPermission evaluates one SpiceDB permission check exactly as the v1
	// API defines it — the request carries its own consistency requirement and
	// caveat context. Callers here treat any error, and any permissionship
	// other than HAS_PERMISSION, as a denial (fail-CLOSED).
	CheckPermission(ctx context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error)
}

// Checker binds a SpiceDB connection and an optional per-session ZedToken
// cache to the tool-call gate. The zero value denies every check that needs
// SpiceDB (nil Cli is fail-CLOSED); a nil Cache disables the freshness floor
// and cache writes, falling back to MinimizeLatency. The zero value of
// RepromptEscalation ({Enabled: false, Judge: nil}) is likewise the
// escape-safe default — see its doc comment.
//
// Copy freely: every field is a reference or a plain value, so a copy shares
// the same connection, cache and escalation config.
type Checker struct {
	Cli   Client
	Cache *ZedTokenCache

	// RepromptEscalation is the OPTIONAL, OFF-BY-DEFAULT model-escalation knob
	// for a web_action changed-page re-prompt (Phase E's E10 task). See
	// RepromptEscalationConfig.
	RepromptEscalation RepromptEscalationConfig
}

// RepromptJudge is the OPTIONAL model-escalation seam a web_action
// changed-page re-prompt may consult (Phase E's E10 task): given a web_action
// instance whose caveated grant check just failed because the live page no
// longer matches what a human approved, ConfirmBenign answers whether that
// change is benign enough to skip asking again.
//
// Consulted ONLY when a grant for this instance already EXISTS — never for a
// first-time ask, and never as a substitute for the deterministic caveat,
// which remains the sole authorization decision whenever this interface
// returns anything but a confirmed "benign" (an error, or benign=false, both
// fall through to the ordinary re-prompt).
//
// toolcheck deliberately defines no implementation: a real backend (e.g. an
// adapter over pkg/agent/tool/browser/judge's Haiku ConsequenceJudge) lives
// above pkg/authz in pkg/agent/runner, which is where the Checker gets
// constructed for production (see pipeline_wiring.go's engineChecker) — this
// package stays below pkg/agent in the dependency order (AGENTS.md's
// package-group table) and never imports it.
type RepromptJudge interface {
	// ConfirmBenign reports whether the live page's change since approval is
	// benign enough to skip the re-prompt for the web_action instance named by
	// (resourceID, agentSessionRef). An error, or a false benign, means "no
	// evidence of benign" — the call proceeds through the ordinary deny →
	// re-prompt path, fail-closed, exactly as a judge.ConsequenceJudge error
	// maps to TierMutate rather than to an allow.
	ConfirmBenign(ctx context.Context, resourceID, agentSessionRef string) (benign bool, err error)
}

// RepromptEscalationConfig is the Checker's model-escalation knob for a
// web_action changed-page re-prompt (E10). The ZERO VALUE — Enabled: false,
// Judge: nil (a true nil interface, never a typed-nil pointer — AGENTS.md's
// "nil interfaces" rule) — is the escape-safe default and is what every
// production Checker gets today: nothing wires this field, so a caveat
// failure ALWAYS re-prompts and the model is NEVER consulted. See
// pkg/agent/runner/pipeline_wiring.go's engineChecker.CheckToolCall for the
// documented seam where a future config path would construct a non-zero
// value.
//
// Enabled is a separate bool rather than "non-nil Judge means on" so a future
// config path can carry a Judge handle around (e.g. for a dry-run / shadow
// mode that logs what the judge WOULD have done) without that alone flipping
// live behavior — the explicit flag is what an operator actually opts into.
type RepromptEscalationConfig struct {
	Enabled bool
	Judge   RepromptJudge
}

// CheckToolCall resolves the permission spec and asks SpiceDB. When the
// effective PermissionCheck declares EnforceMode == EnforceAlways, the
// returned Result is stamped with EnforceOverride = EnforceAlways so
// the runner's permissive bypass honors the deny.
func (c Checker) CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	res := c.check(ctx, p, in)
	if p.Check != nil && p.Check.EnforceMode == authz.EnforceAlways {
		res.EnforceOverride = authz.EnforceAlways
	}
	return res
}

// check is the body CheckToolCall wraps so EnforceMode stamping applies
// uniformly across every return path.
func (c Checker) check(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	// Layer 2 pre-pass: SessionScope policy doc. Empty Scope is identity.
	// Hard-deny (Layer-3 SpiceDB disallow) is retired — the Scope hook
	// (pkg/authz/hooks/scope.go) is the sole dispatch-time enforcer.
	if r := scope.CheckScope(in.SessionScope, p.ToolName, jsonArgs(in.Args)); !r.OK {
		return authz.Result{
			Outcome: authz.OutcomeDenied,
			Message: r.Message,
		}
	}

	switch p.StateImpact {
	case authz.Stateless, authz.Passthrough:
		return authz.Result{Outcome: authz.OutcomeAllowed}
	case authz.External:
		// External is satisfied by a SLOT GRANT this session holds, and by
		// nothing else. Anything unsatisfied denies, and the deny routes the call
		// to a human.
		//
		// The grant leg is checked SPECIFICALLY, not the composed permission.
		// That permission is `slot_grant_<p>->interact + owner`, which unions two
		// very different claims: one Check cannot tell "a human approved this
		// instance for this session" from "the requester happens to own it".
		// Consulting it was tried and reverted — it let ambient ownership
		// authorize an irreversible outbound call nobody had seen, which the
		// gh-api-is-not-a-bypass bundle caught (a seeded `owner` tuple, no slot
		// at all, and `gh api -X POST .../pulls` went from refused to allowed).
		//
		// Asking about the SESSION is what makes this an approval rather than a
		// standing capability: the grant was written from a card a human cleared
		// that named this instance AND this permission, it expires, it is scoped
		// to this session, and it is revocable.
		return c.checkExternalSlotGrant(ctx, p, in)
	case authz.Readonly, authz.Readwrite:
		// fall through to check logic below
	default:
		return authz.Result{
			Outcome: authz.OutcomeDenied,
			Message: fmt.Sprintf("internal: unknown stateImpact %q", p.StateImpact),
		}
	}

	if p.Check == nil {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: "internal: stateImpact requires a check but none was supplied"}
	}

	resourceID, err := authz.ResolveResourceID(*p.Check, in.Args)
	if err != nil {
		return authz.Result{
			Outcome:            authz.OutcomeDenied,
			Message:            resolutionDenyMessage(*p.Check, err),
			UnresolvedResource: true,
		}
	}
	// An EMPTY id is a resolution failure too. Letting it through would
	// authorize against `git_repo:`, an object nobody granted anything on — a
	// denial, but an inscrutable one, and one an approver could be asked to
	// grant.
	//
	// The CEL branch does not reach here: EvalString rejects an empty result as
	// an error, so a guard expression written to refuse a call — `has(args.remote)
	// && args.remote.startsWith("https://") ? args.remote : ""` — lands on the
	// error path above. This guards the TEMPLATE branch, where a substitution
	// can legitimately produce "" without failing. Both are the same denial and
	// both set UnresolvedResource; only the route differs.
	if resourceID == "" {
		return authz.Result{
			Outcome:            authz.OutcomeDenied,
			Message:            resolutionDenyMessage(*p.Check, nil),
			UnresolvedResource: true,
		}
	}

	// AFTER resolution, deliberately. Resolving the id reads only the call's
	// own arguments, so an unresolvable one is the CALLER's error and is
	// reportable without any infrastructure. Checking the client first reported
	// "no SpiceDB client wired" for a call that simply failed to name its
	// resource — pointing the reader at the cluster when the fix was in the
	// arguments.
	if c.Cli == nil {
		return authz.Result{
			Outcome: authz.OutcomeDenied,
			Message: "authz: no SpiceDB client wired — refusing to evaluate Readonly/Readwrite check (fail-closed)",
		}
	}

	subjects := in.Subjects
	if len(subjects) == 0 {
		subjects = []string{in.Subject}
	}

	// An EMPTY subject is a malformed request, not a denial, and the two are
	// not interchangeable. SpiceDB's object-id regex is `{1,}`, so a check
	// built with one comes back InvalidArgument: the log line names a regex
	// rather than a principal, an operator reading it has nothing to grant
	// anything to, and under toolCalls.mode=permissive the "would deny in
	// enforcing mode" note is a lie — no check evaluated, so nothing is known
	// about what enforcing would have done. Refuse here, in words.
	//
	// Any empty element denies the whole call, rather than being dropped from
	// the list. In "both" mode every subject must allow, so silently dropping
	// one would WIDEN the check by removing a requirement — the one direction a
	// fail-closed gate must never move.
	for _, s := range subjects {
		if s == "" {
			return authz.Result{
				Outcome: authz.OutcomeDenied,
				Message: "authz: no acting subject for this session, so there is nothing to authorize this call as " +
					"(fail-closed) — a session whose inbound carries no human needs its input channel to declare a service subject",
			}
		}
	}

	// The routeViaSessionGrant branch that used to sit here is gone. It skipped
	// the direct Check entirely and resolved through an agentsession-grant walk,
	// which only ever worked because the resource's permission carried a
	// wildcard leaf. Approvals now write the grant on the RESOURCE pointing at
	// the session, so the direct Check below resolves the session's member set
	// and passes post-approval on its own — for the actual requester, honouring
	// `denied`, with no wildcard anywhere in the schema.

	consistency := &v1.Consistency{
		Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true},
	}
	cacheEmpty := true
	if c.Cache != nil {
		tok := c.Cache.Get(p.Check.ResourceType, resourceID)
		if tok == "" {
			tok = c.Cache.Latest()
		}
		if tok != "" {
			cacheEmpty = false
			consistency = &v1.Consistency{
				Requirement: &v1.Consistency_AtLeastAsFresh{
					AtLeastAsFresh: &v1.ZedToken{Token: tok},
				},
			}
		}
	}
	// A cache with nothing in it yet means this is the FIRST Check this
	// process has ever made against SpiceDB for this session — there is no
	// prior ZedToken to float a MinimizeLatency read on. That is exactly the
	// shape of a same-turn plan-gate approval: narrowToApproved writes a
	// session-only-standing slot grant synchronously, in-process, and the very
	// next dispatch (the call the grant exists to authorize) is this Check.
	// MinimizeLatency with no floor is free to serve ANY snapshot at or before
	// the current revision, including one that predates a write this same
	// goroutine made moments ago — the call the human's approval just
	// authorized comes back denied. Cost is bounded to once per session: the
	// response below stamps the cache regardless of outcome, so every later
	// Check has a real floor and goes back to MinimizeLatency/AtLeastAsFresh.
	if c.Cache != nil && cacheEmpty {
		consistency = &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		}
	}
	if in.RequireFreshest {
		// The post-approval re-check. A slot grant was written seconds ago by
		// ANOTHER PROCESS (channelsd handling the decision), and neither
		// MinimizeLatency nor an AtLeastAsFresh token this process happens to
		// hold is guaranteed to include it — both can serve a pre-write
		// snapshot, and the user sees "permission denied" on the call they
		// just approved.
		//
		// This guarantee is not new; it MOVED. It used to live on the
		// session-grant re-check below, which is where the original production
		// incident was fixed. Retiring routeViaSessionGrant took the re-check
		// off that path and onto this one, which would have silently inherited
		// MinimizeLatency. Cost is bounded: once per approve cycle, not per
		// tool call.
		consistency = &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		}
	}

	type call struct {
		subj string
		resp *v1.CheckPermissionResponse
		err  error
	}
	results := make([]call, len(subjects))
	var wg sync.WaitGroup
	for i, s := range subjects {
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &v1.CheckPermissionRequest{
				Resource: &v1.ObjectReference{
					ObjectType: p.Check.ResourceType,
					ObjectId:   resourceID,
				},
				Permission:  p.Check.Permission,
				Subject:     &v1.SubjectReference{Object: subjectObject(s)},
				Consistency: consistency,
			}
			resp, callErr := c.Cli.CheckPermission(ctx, req)
			results[i] = call{subj: s, resp: resp, err: callErr}
		}()
	}
	wg.Wait()

	var firstFail *call
	for i := range results {
		r := &results[i]
		if r.err != nil {
			return authz.Result{Outcome: authz.OutcomeDenied, Message: fmt.Sprintf("authz: SpiceDB error: %v", r.err)}
		}
		if r.resp.GetPermissionship() != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			if firstFail == nil {
				firstFail = r
			}
		}
	}

	// Stash post-check ZedToken regardless of outcome (the read happened).
	if c.Cache != nil {
		for _, r := range results {
			if r.resp != nil && r.resp.CheckedAt != nil && r.resp.CheckedAt.Token != "" {
				c.Cache.Set(p.Check.ResourceType, resourceID, r.resp.CheckedAt.Token)
				break
			}
		}
	}

	if firstFail == nil {
		return authz.Result{Outcome: authz.OutcomeAllowed}
	}
	// The session-grant fallback that used to sit here is gone. It re-checked
	// `agentsession:<ref>#check_<perm>_<resType>` under the call's args-hash
	// caveat — the tuple the OLD approval flow wrote. Approvals write a slot
	// grant on the RESOURCE now, which the primary check above resolves, and
	// nothing writes a grant-pair tuple any more. So the fallback could no
	// longer allow anything; it only spent a FullyConsistent round-trip on
	// every post-approval re-check before returning the same deny.
	return authz.Result{
		Outcome: authz.OutcomeDenied,
		Message: formatDeny(firstFail.subj, p.Check.Permission, p.Check.ResourceType, resourceID),
	}
}

// subjectObject renders one check subject as the SpiceDB object it names.
//
// The gate's subject is a canonical user id, and canonicals are base64url
// (of an email, or of the synthetic kind:teamScope:externalID tuple), whose
// alphabet contains no ':' — so a bare value is a `user` and a value carrying
// a colon is already a fully-qualified reference of its own type. A session
// whose inbound supplied no starting user acts as exactly such a reference:
// its input Channel's declared "service:<id>", assigned verbatim (see
// identity.CanonicalUserID.SubjectRef, which owns this discriminator).
//
// Prefixing "user:" unconditionally would turn that into object_id
// "service:<id>", whose colon SpiceDB's object-id regex rejects — an
// InvalidArgument in place of an allow or a deny. Widening is not a risk in
// the other direction: a subject that names its own type is checked AS that
// type, and a type the composed schema does not declare simply errors, which
// this package's caller already treats as a denial.
func subjectObject(subject string) *v1.ObjectReference {
	// The caller resolved this canonical; this function only shapes it into an
	// object reference and makes no trust decision of its own.
	canon := identity.CanonicalFromTrusted(subject, "canonical resolved by the caller before the check")
	typ, id, found := strings.Cut(string(canon.SubjectRef()), ":")
	if !found {
		// Unreachable: SubjectRef returns "" only for an empty canonical, which
		// the caller has already refused, and otherwise guarantees a "<type>:"
		// prefix. Fail closed on a subject SpiceDB can only reject anyway.
		return &v1.ObjectReference{ObjectType: "user", ObjectId: subject}
	}
	return &v1.ObjectReference{ObjectType: typ, ObjectId: id}
}

// CheckWithVariants resolves the effective Permission by trying each
// variant's When against in.Args in order, then falling back to the
// passed-in p. Empty variants slice is allowed and equivalent to
// CheckToolCall(ctx, p, in).
func (c Checker) CheckWithVariants(ctx context.Context, p authz.Permission, variants []authz.PermissionVariant, in authz.Inputs) authz.Result {
	if len(variants) > 0 {
		got, matched, err := authz.ResolveVariant(variants, in.Args)
		if err != nil {
			return authz.Result{Outcome: authz.OutcomeDenied, Message: fmt.Sprintf("variant resolution failed: %v", err)}
		}
		if matched {
			return c.CheckToolCall(ctx, got, in)
		}
	}
	return c.CheckToolCall(ctx, p, in)
}

// formatDeny returns the standard "permission denied" message.
//
// The subject is rendered as SpiceDB was asked about it (see subjectObject),
// not as "user:"+subject: a session acting as its Channel's "service:<id>"
// would otherwise be reported as "user:service:<id>", a principal that exists
// nowhere, sending anyone who went looking for its grants after a subject that
// was never checked.
func formatDeny(subject, permission, resourceType, resourceID string) string {
	// Denial text only; no trust decision is made here.
	canon := identity.CanonicalFromTrusted(subject, "canonical from the denied check, for the message")
	return fmt.Sprintf("permission denied: %s does not have %s on %s:%s",
		canon.SubjectRef(), permission, resourceType, resourceID)
}

// jsonArgs converts in.Args to json.RawMessage for scope.CheckScope.
// Empty / unmarshallable args produce "{}" (best-effort).
func jsonArgs(args map[string]any) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// resolutionDenyMessage renders the denial for a resource id that could not be
// resolved from the call's arguments.
//
// Prefers the check author's hint, because it is the only part the agent can
// act on: the underlying error names a CEL expression or a template variable,
// which describes the SPEC rather than the call. Falls back to the raw error so
// a check with no hint is no worse off than before.
//
// err is nil when the expression resolved cleanly to an empty string — a guard
// clause refusing the call rather than a fault.
func resolutionDenyMessage(c authz.PermissionCheck, err error) string {
	if c.ResourceIDHint != "" {
		return fmt.Sprintf("this call did not identify which %s it acts on, so it cannot be authorized: %s",
			c.ResourceType, c.ResourceIDHint)
	}
	if err != nil {
		return fmt.Sprintf("internal: %v", err)
	}
	return fmt.Sprintf("this call did not identify which %s it acts on, so it cannot be authorized", c.ResourceType)
}

// checkExternalSlotGrant answers an EXTERNAL permission from the slot-grant leg
// alone: does this SESSION hold a grant a human wrote for this exact instance
// and permission?
//
// Denies — routing to human approval — on every path that is not an unambiguous
// yes: no session threaded through, an id that will not resolve, a SpiceDB
// error, or simply no grant. External is the tier whose effects leave the
// session and cannot be undone, so an uncertain answer must cost a prompt
// rather than an assumption.
func (c Checker) checkExternalSlotGrant(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	const needsApproval = "external tool: human approval required"

	if p.Check == nil || in.AgentSessionRef == "" || c.Cli == nil {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: needsApproval}
	}
	// Only a DECLARED slot type can carry a grant. Asking SpiceDB about the
	// grant relation on a type the class never declared is not merely a wasted
	// round trip: ComposeSlots never wrote that relation into the schema, so the
	// call errors, and the arm below would report a SpiceDB failure for what is
	// really the ordinary case of an external tool with no instance axis.
	if !slices.Contains(in.SlotResourceTypes, p.Check.ResourceType) {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: needsApproval}
	}
	resourceID, err := authz.ResolveResourceID(*p.Check, in.Args)
	if err != nil || resourceID == "" {
		// The call never said which resource it means, so no grant could name it.
		// Flagged as unresolved so the caller routes it back to the agent as a
		// tool result rather than showing a human a card with an empty object id.
		return authz.Result{
			Outcome:            authz.OutcomeDenied,
			Message:            needsApproval,
			UnresolvedResource: err != nil || resourceID == "",
		}
	}

	// LAYER 1 — the coarse capability grant: web_host#act for a browser acting
	// call (git_repo#push for a git tool, etc.). The fingerprint is presented only
	// for a web_action PRIMARY (keyed on the resource TYPE — web_host is uncaveated,
	// so its sibling check must carry no caveat context); the browser acting tool's
	// primary is web_host, so this is normally uncaveated. E2's integration tests
	// drive this path directly with p.Check.ResourceType == web_action.
	if res := c.checkOneSlotGrant(ctx, p.Check.ResourceType, p.Check.Permission, resourceID,
		in.AgentSessionRef, in.PresentedPageFingerprint,
		p.Check.ResourceType == browserschema.WebActionType); res.Outcome != authz.OutcomeAllowed {
		return res
	}

	// LAYER 2 — the per-action revalidation grant (Phase E's E8 gate). A MUTATE
	// must hold BOTH the coarse web_host grant (LAYER 1, above) AND a per-action
	// web_action grant whose pagestate_matches caveat still matches the live page.
	// Consulted ONLY when the browser tool classified this call a mutate
	// (in.WebActionRequired) AND the class DECLARED a web_action slot. Gating on
	// SlotResourceTypes is not optional: a class that declares only web_host has no
	// web_action#slot_grant_act relation in its composed schema, so asking about it
	// could only error — the same reason LAYER 1 is gated. observe/navigate/session
	// calls leave WebActionRequired false and are authorized by LAYER 1 alone,
	// exactly as before Phase E. The web_action deny routes to a human just like a
	// host deny.
	if !in.WebActionRequired || !slices.Contains(in.SlotResourceTypes, browserschema.WebActionType) {
		return authz.Result{Outcome: authz.OutcomeAllowed}
	}
	waID, err := authz.NewObjectID(in.WebActionKey, browserschema.WebActionValueTransforms())
	if err != nil {
		// The tool stamped no (or an unescapable) key: the mutate cannot name the
		// action it acts on, so no grant could match. Route it back to the agent
		// (unresolved) rather than show a human a card with an empty object id.
		return authz.Result{Outcome: authz.OutcomeDenied, Message: needsApproval, UnresolvedResource: true}
	}
	res := c.checkOneSlotGrant(ctx, browserschema.WebActionType, p.Check.Permission, waID.String(),
		in.AgentSessionRef, in.PresentedPageFingerprint, true /* present the pagestate_matches fingerprint */)
	if res.Outcome == authz.OutcomeAllowed {
		return res
	}

	// E10 — distinguish a CHANGED-PAGE RE-PROMPT (a web_action grant for this
	// instance already exists; only its caveat failed) from an ordinary
	// FIRST-TIME ASK (no grant at all). Advisory only: this never changes the
	// Outcome above, which has already denied. See webActionGrantExistsForReprompt.
	res.ChangedPageReprompt = c.webActionGrantExistsForReprompt(ctx, p.Check.Permission, waID.String(), in.AgentSessionRef)

	// Optional model-escalation (E10, OFF BY DEFAULT — c.RepromptEscalation's
	// zero value is {Enabled: false, Judge: nil}). Consulted ONLY when this IS a
	// changed-page re-prompt (a human already approved this exact action once)
	// AND the operator explicitly turned the knob on AND a judge is wired.
	// Everything else — the default Checker, a first-time ask
	// (ChangedPageReprompt false), a judge that errors, or a judge that does not
	// affirmatively confirm benign — falls straight through to the ordinary
	// deny below, which is what raises the re-prompt. This is the ONLY place a
	// model can ever suppress a re-prompt, and only for an action a human has
	// already approved before: the escape guarantee ("a caveat failure always
	// re-prompts") never depends on the model unless an operator has opted in.
	if res.ChangedPageReprompt && c.RepromptEscalation.Enabled && c.RepromptEscalation.Judge != nil {
		benign, jerr := c.RepromptEscalation.Judge.ConfirmBenign(ctx, waID.String(), in.AgentSessionRef)
		if jerr != nil {
			// Fail CLOSED: a judge failure never suppresses a re-prompt, mirroring
			// judge.ConsequenceJudge's own "an error is not how ambiguity is
			// expressed" contract — an error here is not evidence of benign-ness.
			return res
		}
		if benign {
			return authz.Result{Outcome: authz.OutcomeAllowed}
		}
	}
	return res
}

// webActionGrantExistsForReprompt answers, BEST-EFFORT and ADVISORY ONLY,
// whether a web_action grant already exists for (resourceID, agentSessionRef)
// — regardless of whether its pagestate_matches caveat currently passes. It
// is what lets checkExternalSlotGrant tell a changed-page RE-PROMPT (a human
// already approved this exact action; the live page just no longer matches)
// apart from an ordinary FIRST-TIME ask (no grant has ever been written).
//
// The result is NEVER consulted for the authorization decision itself —
// checkOneSlotGrant's HAS_PERMISSION-only mapping already decided allow/deny
// before this is ever called — so a wrong answer here can only mislabel a
// card's copy, never change who gets approved.
//
// The probe omits the pagestate_matches context entirely (no Context on the
// request). SpiceDB partial-evaluates a caveated relationship with no context
// supplied: a grant that EXISTS comes back CONDITIONAL_PERMISSION (the caveat
// could not finish evaluating because presented_fingerprint was never
// supplied, but the relationship IS there); no grant at all comes back
// NO_PERMISSION (there is nothing to partial-evaluate). Any other outcome — an
// RPC error, a nil response, or anything but CONDITIONAL — is treated as "no
// evidence of an existing grant" (false), the fail-QUIET direction for a
// cosmetic signal.
func (c Checker) webActionGrantExistsForReprompt(ctx context.Context, permission, resourceID, agentSessionRef string) bool {
	if c.Cli == nil {
		return false
	}
	req := &v1.CheckPermissionRequest{
		Resource: &v1.ObjectReference{ObjectType: browserschema.WebActionType, ObjectId: resourceID},
		// The slot_grant_<permission> RELATION, same as checkOneSlotGrant's main
		// check — never the composed permission it feeds.
		Permission: authz.SlotGrantRelationName(permission),
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: agentSessionRef},
		},
		Consistency: externalGrantConsistency(c.Cache, browserschema.WebActionType, resourceID),
		// No Context, deliberately: see the doc comment above for why an absent
		// pagestate_matches context is exactly what distinguishes "a grant exists"
		// (CONDITIONAL_PERMISSION) from "no grant at all" (NO_PERMISSION).
	}
	resp, err := c.Cli.CheckPermission(ctx, req)
	if err != nil || resp == nil {
		return false
	}
	return resp.Permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION
}

// checkOneSlotGrant answers a SINGLE slot-grant leg: does the session named by
// agentSessionRef hold a grant a human wrote for exactly this
// (resourceType, resourceID, permission)? It allows ONLY on an unambiguous
// HAS_PERMISSION; everything else (NO_PERMISSION, CONDITIONAL, a SpiceDB error,
// a nil response) denies and routes the call to a human. External is the tier
// whose effects leave the session and cannot be undone, so an uncertain answer
// must cost a prompt rather than an assumption.
//
// When presentFingerprint is set the request carries fingerprint as the
// pagestate_matches caveat's check-time context — used for the web_action
// (revalidated) grant, whose slot_grant_act is the ONLY caveated one. ALWAYS
// supplying the arg (empty or not) keeps the decision a crisp HAS/NO: the
// caveat's own `presented_fingerprint != ""` guard turns an empty value into a
// definitive NO_PERMISSION, where an ABSENT arg would partial-evaluate to
// CONDITIONAL — which the HAS_PERMISSION-only mapping below also denies, but
// presenting the arg keeps that off the hot path. An uncaveated leg (web_host and
// every non-browser external type) passes presentFingerprint=false and carries
// no Context.
func (c Checker) checkOneSlotGrant(ctx context.Context, resourceType, permission, resourceID, agentSessionRef, fingerprint string, presentFingerprint bool) authz.Result {
	const needsApproval = "external tool: human approval required"

	req := &v1.CheckPermissionRequest{
		Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
		// The slot_grant_<permission> RELATION, never the permission it feeds.
		Permission: authz.SlotGrantRelationName(permission),
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: agentSessionRef},
		},
		// Freshness matters most here: narrowToApproved writes the grant
		// synchronously and the very next dispatch is this Check, so a snapshot
		// predating that write would refuse the call the approval exists to
		// authorize. Float on the cache when it has a token, and demand the
		// freshest read when it does not.
		Consistency: externalGrantConsistency(c.Cache, resourceType, resourceID),
	}
	if presentFingerprint {
		fpCtx, ctxErr := structpb.NewStruct(map[string]any{
			"presented_fingerprint": fingerprint,
		})
		if ctxErr != nil {
			// A string value cannot fail structpb serialization, so this is
			// unreachable — but deny LOUDLY rather than drop the caveat context and
			// silently widen a revalidated acting check into an uncaveated one.
			return authz.Result{
				Outcome: authz.OutcomeDenied,
				Message: fmt.Sprintf("authz: building pagestate_matches caveat context: %v", ctxErr),
			}
		}
		req.Context = fpCtx
	}

	resp, callErr := c.Cli.CheckPermission(ctx, req)
	if callErr != nil {
		// Deny either way, but never silently: the human still gets a card, and
		// an operator reading logs gets the reason it cost one. Same shape as
		// the multi-subject arm above.
		return authz.Result{
			Outcome: authz.OutcomeDenied,
			Message: fmt.Sprintf("authz: SpiceDB error: %v", callErr),
		}
	}
	if resp == nil {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: needsApproval}
	}
	// SECURITY INVARIANT — allow ONLY on an unambiguous HAS_PERMISSION. Everything
	// else denies and routes to a human: NO_PERMISSION, and, critically,
	// PERMISSIONSHIP_CONDITIONAL_PERMISSION. CONDITIONAL is what a caveated grant
	// (web_action's pagestate_matches) returns when a required caveat arg was not
	// supplied — SpiceDB could not finish evaluating the caveat. Treating it as
	// allow would authorize an irreversible acting call whose page-state
	// revalidation never actually ran, reopening the exact TOCTOU hole E1 closed.
	//
	// Keep this `== HAS_PERMISSION`. A future refactor to `!= NO_PERMISSION` (or
	// `!= DENIED`) would silently let CONDITIONAL through — a security regression,
	// not a style change. The web_action path above always supplies the arg so a
	// CONDITIONAL should not arise in practice, but this mapping is the fail-closed
	// backstop if it ever does.
	if resp.Permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
		if c.Cache != nil && resp.CheckedAt != nil {
			c.Cache.Set(resourceType, resourceID, resp.CheckedAt.Token)
		}
		return authz.Result{Outcome: authz.OutcomeAllowed}
	}
	return authz.Result{Outcome: authz.OutcomeDenied, Message: needsApproval}
}

// externalGrantConsistency floors the grant-leg read on whatever the cache
// knows, and demands the freshest read when it knows nothing.
//
// An empty cache is the exact shape of a same-turn approval: the grant was
// written synchronously moments ago and this is the first Check since. A
// MinimizeLatency read with no floor may serve a snapshot from before that
// write and refuse the call the human just approved.
func externalGrantConsistency(cache *ZedTokenCache, resourceType, resourceID string) *v1.Consistency {
	if cache != nil {
		tok := cache.Get(resourceType, resourceID)
		if tok == "" {
			tok = cache.Latest()
		}
		if tok != "" {
			return &v1.Consistency{Requirement: &v1.Consistency_AtLeastAsFresh{
				AtLeastAsFresh: &v1.ZedToken{Token: tok},
			}}
		}
	}
	return &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
}
