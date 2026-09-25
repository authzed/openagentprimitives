package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxWhyRunes is spec.why's cap, enforced here (not just left to the CRD's
// own +kubebuilder:validation:MaxLength=280) so the meta tool never attempts
// a Create the apiserver would reject. Counted in RUNES, not bytes: a naive
// byte slice can split a multi-byte UTF-8 rune in half and write invalid
// UTF-8 into the CR.
const maxWhyRunes = 280

// CredentialUpdateConfig wires the request_credential_update meta tool to
// the operator client and to the runner's own tool table.
//
// ToolLookup is deliberately NOT a []tool.Tool snapshot: capability.Ordered()
// sorts credential_update mid-alphabet while RunnerEnv.AllToolsSoFar is filled
// only just before the last-sorted introspection capability runs, so a snapshot
// here would always be nil. The runner late-binds ToolLookup once the full tool
// table exists. A nil ToolLookup is graceful — Execute refuses inline with a
// clear message rather than panicking.
type CredentialUpdateConfig struct {
	Client       client.Client
	ToolLookup   func(name string) (tool.Tool, bool)
	PollInterval time.Duration
	MaxWait      time.Duration
}

// NewCredentialUpdate constructs the request_credential_update meta tool.
func NewCredentialUpdate(cfg CredentialUpdateConfig) tool.Tool {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.MaxWait <= 0 {
		// A human has to notice a card, click it, and enter a credential --
		// far longer than a render or artifact wait. Callers (the
		// credential_update capability) override this with the session's own
		// IdleTTL; this default only guards against a zero value reaching
		// Execute directly (e.g. in a test) and either spinning forever or
		// timing out unreasonably fast.
		cfg.MaxWait = 10 * time.Minute
	}
	return &credentialUpdateTool{cfg: cfg}
}

type credentialUpdateTool struct {
	cfg CredentialUpdateConfig
}

func (*credentialUpdateTool) Name() string    { return "request_credential_update" }
func (*credentialUpdateTool) Kind() tool.Kind { return tool.KindMeta }
func (*credentialUpdateTool) Permission() authz.Permission {
	// The CR write is gated by the reconciler's own independent
	// re-verification (pkg/controllers/credentialupdaterequest), not by
	// per-tool authz -- there is no resource-level permission that models
	// "may this agent claim its own credential is dead."
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*credentialUpdateTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*credentialUpdateTool) Description() string {
	return "Ask a human to replace a credential -- but ONLY when a tool call just failed with an authentication " +
		"error (e.g. 401/403, \"unauthorized\", \"invalid token\", \"invalid_grant\"), never for an empty result, a " +
		"business-logic error, or a hunch. Name the failing `tool` and give a short `why`. The platform independently " +
		"re-verifies the credential before asking anyone -- it does not take your word for it -- and refuses outright " +
		"if the credential still authenticates. A refusal almost always means the real problem is missing permissions " +
		"or scope, not an expired credential; do not call this again for the same failure. This call blocks until a " +
		"human acts or the request times out, so use it once you're sure, not speculatively."
}

func (*credentialUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"tool": {"type": "string"},
			"why":  {"type": "string", "maxLength": 280}
		},
		"required": ["tool", "why"]
	}`)
}

type credentialUpdateArgs struct {
	// Tool is the LLM-facing name of the failing tool, resolved to its Origin().
	Tool string `json:"tool"`
	// Why is the agent's evidence, capped at maxWhyRunes and shown to the human.
	Why string `json:"why"`
}

func (t *credentialUpdateTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args credentialUpdateArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"tool": "github_create_issue", "why": "the last three calls returned 401 Unauthorized"}`); !ok {
		return res, nil
	}

	// Reattach, don't recreate: a non-terminal request already parking this
	// session takes priority over anything named in args. A runner restart
	// mid-park re-executes the same still-blocked tool_use, and creating a
	// SECOND request here would silently burn an ask from the
	// 2-per-credential lifetime budget for a request that already exists.
	existing, err := t.findOpenRequest(ctx, sess)
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: checking for an existing request: %v", t.Name(), err),
			IsError: true, Trusted: true,
		}, nil
	}

	crName := ""
	if existing != nil {
		crName = existing.Name
	} else {
		name, refuse := t.createRequest(ctx, sess, args)
		if refuse != nil {
			return *refuse, nil
		}
		crName = name
	}

	return t.poll(ctx, sess, crName)
}

// createRequest performs the cheap, local, catalog-free checks (unknown
// tool, origin-less tool) before ever writing a CR -- writing a CR for a
// typo'd tool name would burn budget for nothing -- then fetches the
// AgentSession (both to stamp a genuine ownerReference and to read the
// session starter's canonical subject for spec.requestedBy) and creates the
// request. Returns either the new CR's name, or a non-nil refusal Result to
// return directly.
func (t *credentialUpdateTool) createRequest(ctx context.Context, sess *tool.SessionContext, args credentialUpdateArgs) (string, *tool.Result) {
	if t.cfg.ToolLookup == nil {
		return "", &tool.Result{
			Content: fmt.Sprintf("%s: not available in this session (no tool lookup wired)", t.Name()),
			IsError: true, Trusted: true,
		}
	}
	named, ok := t.cfg.ToolLookup(args.Tool)
	if !ok {
		return "", &tool.Result{
			Content: fmt.Sprintf("%s: unknown tool %q -- check the exact tool name (as it appears in your tool list) and retry", t.Name(), args.Tool),
			IsError: true, Trusted: true,
		}
	}
	// The type assertion alone is not enough: sandbox.SandboxTool implements
	// tool.OriginTool unconditionally but deliberately returns "" when it has no
	// toolkit, which the runner's toolGuardLookup treats the same way. Naming
	// such a tool must refuse HERE, before any CR write — credupdate.SplitOrigin
	// would eventually refuse it too, but only after a wasted CR plus a
	// poll/reconcile round trip.
	originTool, ok := named.(tool.OriginTool)
	if !ok || originTool.Origin() == "" {
		return "", &tool.Result{
			Content: fmt.Sprintf("%s: tool %q uses no managed credential (it belongs to no shared upstream), so there is nothing here for a human to update", t.Name(), args.Tool),
			IsError: true, Trusted: true,
		}
	}

	// Setting only spec.sessionRef is not enough: the reconciler also
	// requires a genuine ownerReference whose UID matches the resolved
	// AgentSession's UID (ownedBySession, pkg/controllers/credentialupdaterequest),
	// so every request without one refuses NoCredential immediately. Fetching
	// the AgentSession fresh (rather than trusting sess.AgentSessionUID alone)
	// also gives us its started-by annotation for spec.requestedBy.
	var agentSession spiceboxv1alpha1.AgentSession
	if gerr := t.cfg.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &agentSession); gerr != nil {
		return "", &tool.Result{
			Content: fmt.Sprintf("%s: could not resolve this session's AgentSession: %v", t.Name(), gerr),
			IsError: true, Trusted: true,
		}
	}

	tval := true
	cr := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{
			// The apiserver names it. A suffix picked here can collide, and a
			// collision comes back as AlreadyExists on the ask the agent just
			// spent -- which it cannot retry differently, since the name was
			// never something it chose. GenerateName makes uniqueness the
			// apiserver's problem, and it resolves one itself.
			GenerateName: fmt.Sprintf("cur-%s-", sess.Name),
			Namespace:    sess.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "AgentSession",
				Name:               agentSession.Name,
				UID:                agentSession.UID,
				Controller:         &tval,
				BlockOwnerDeletion: &tval,
			}},
		},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name},
			Origin:      originTool.Origin(),
			ToolName:    named.Name(),
			Why:         capWhyRunes(args.Why),
			RequestedBy: spiceboxv1alpha1.StartedBySubject(&agentSession),
		},
	}
	if cerr := t.cfg.Client.Create(ctx, cr); cerr != nil {
		return "", &tool.Result{
			Content: fmt.Sprintf("%s: creating the request: %v", t.Name(), cerr),
			IsError: true, Trusted: true,
		}
	}
	return cr.Name, nil
}

// findOpenRequest returns the non-terminal CredentialUpdateRequest already
// owned by this AgentSession, if any. The blocking contract of Execute means
// an agent can never issue a second call while a first is still parked, but
// this is a List against live state rather than a stored counter, so it
// survives a runner restart mid-park. At most one is expected; if more than
// one somehow exists, the first found is used (the invariant this guards is
// "never abandon an ask that is already in flight and reach for a fresh one",
// not tie-breaking -- deliberately not phrased in terms of the budget, since a
// still-Pending request is non-terminal and reattachable but has not been
// determined and so has spent nothing).
func (t *credentialUpdateTool) findOpenRequest(ctx context.Context, sess *tool.SessionContext) (*spiceboxv1alpha1.CredentialUpdateRequest, error) {
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := t.cfg.Client.List(ctx, &list, client.InNamespace(sess.Namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if !tool.OwnedBySession(item.OwnerReferences, sess) {
			continue
		}
		if !spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(item.Status.Phase) {
			return item, nil
		}
	}
	return nil, nil
}

// poll waits for the named CredentialUpdateRequest to reach a terminal
// phase, cfg.MaxWait, or ctx.Done(), whichever comes first. Every returned
// Result sets Trusted: true -- the content is entirely platform-authored
// (the agent's own `why` is never echoed back).
func (t *credentialUpdateTool) poll(ctx context.Context, sess *tool.SessionContext, name string) (tool.Result, error) {
	deadline := time.Now().Add(t.cfg.MaxWait)
	// fresh is hoisted above the loop (rather than re-declared each iteration)
	// so its last-fetched value survives loop exit: the post-loop MaxWait
	// branch below needs fresh.Status.InteractionRef to tell a genuine timeout
	// apart from a request that was never delivered to a human at all (Task 7
	// review, New-Important-1).
	var fresh spiceboxv1alpha1.CredentialUpdateRequest
	// lastErr holds the most recent non-NotFound Get failure, cleared by the
	// next successful read. It exists because dropping that error is not
	// merely lossy here -- it INVERTS the give-up message. A Get that keeps
	// failing (a missing RBAC rule -> 403, an apiserver blip) leaves `fresh`
	// zero-valued, and a zero InteractionRef is indistinguishable from "no
	// card was ever published"; without lastErr the tool would state, as
	// fact, that nobody has been asked -- a confident claim manufactured out
	// of an unknown.
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tool.Result{Content: fmt.Sprintf("%s: %v", t.Name(), err), IsError: true, Trusted: true}, nil
		}
		if err := t.cfg.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: name}, &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return tool.Result{
					Content: fmt.Sprintf("%s: the request vanished before a decision was reached", t.Name()),
					IsError: true, Trusted: true,
				}, nil
			}
			lastErr = err
			slog.Info("request_credential_update: reading the request failed; retrying until MaxWait",
				"namespace", sess.Namespace, "name", name, "err", err.Error())
			time.Sleep(t.cfg.PollInterval)
			continue
		}
		lastErr = nil // a successful read supersedes any earlier failure
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled:
			return tool.Result{
				Content: fmt.Sprintf("%s: the credential was updated. Retry your original tool call now.", t.Name()),
				Trusted: true,
			}, nil
		case spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused:
			// status.reason is surfaced VERBATIM -- it is platform-authored and
			// is the agent's only signal about why. Do not paraphrase or wrap it
			// in additional explanation beyond a tool-name prefix.
			return tool.Result{
				Content: fmt.Sprintf("%s: refused -- %s", t.Name(), fresh.Status.Reason),
				IsError: true, Trusted: true,
			}, nil
		case spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired:
			reason := fresh.Status.Reason
			if reason == "" {
				reason = "the request expired without a human decision"
			}
			return tool.Result{
				Content: fmt.Sprintf("%s: expired -- %s", t.Name(), reason),
				IsError: true, Trusted: true,
			}, nil
		}
		time.Sleep(t.cfg.PollInterval)
	}
	// Four genuinely different give-up outcomes, in decreasing order of what we
	// actually know. Ordering matters: positive evidence that a human WAS asked
	// beats a later read failure, and a read failure beats the absence-of-
	// evidence branch -- which may only look like absence because we went
	// blind.
	switch {
	case fresh.Status.InteractionRef != "":
		// A card WAS delivered (InteractionRef is stamped by channelsd's
		// CredentialUpdateWatcher in the SAME patch as CardDelivered=True) and
		// the human simply hasn't acted yet. The plain timeout is accurate.
		return tool.Result{
			Content: fmt.Sprintf("%s: timed out waiting for a human decision; the request is still open and will keep being processed. "+
				"Report the credential problem in your response rather than calling this again.", t.Name()),
			IsError: true, Trusted: true,
		}, nil
	case fresh.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed || fresh.Status.CollapsedInto != nil:
		// This request is a FOLLOWER: another session's request already holds the
		// ask for the identical credential, so this one rides on that answer.
		//
		// It sits above the read-failure branch because what we know about the
		// SHARED ask outranks a later read failure. A follower is never delivered
		// a card of its own, so the InteractionRef branch cannot catch it, and
		// without this branch it reaches `default` — which would claim nobody has
		// been asked and the request is still Open. Both clauses are false for a
		// follower. This is routine, not a corner: MaxWait is well short of the
		// ask window a human gets, so a follower whose canonical is still live
		// times out here regularly.
		//
		// CollapsedInto is checked alongside the phase so any disagreement
		// between them still enters this branch — `default` is the arm that makes
		// the false claim, so widening the guard fails safe.
		//
		// The STRONG claim, "a human has already been asked", is gated on the
		// CANONICAL having been delivered, which nothing on this request can tell
		// us: a canonical is Open the moment the operator determines it, but
		// publication is channelsd's job in another process with its own silent
		// skip paths. A follower of a never-published canonical has had nobody
		// asked on its behalf. The weaker wording below is true either way, so
		// the gate fails closed onto it.
		if t.canonicalDelivered(ctx, fresh.Status.CollapsedInto) {
			return tool.Result{
				Content: fmt.Sprintf("%s: timed out waiting for a human decision. A human has already been asked to replace this "+
					"credential -- for another session's request covering the same credential -- and this request is waiting on "+
					"that one rather than asking a second person. It is still being processed and will settle when that request "+
					"does. Report the credential problem in your response rather than calling this again.", t.Name()),
				IsError: true, Trusted: true,
			}, nil
		}
		return tool.Result{
			Content: fmt.Sprintf("%s: timed out waiting for a decision. Another session already raised a request to replace this "+
				"same credential, and this request is waiting on that one rather than asking a second person -- but whether that "+
				"request has actually reached a human could not be confirmed, so it may not have been shown to anyone yet. It is "+
				"still being processed and will settle when that request does. Report the credential problem in your response "+
				"rather than calling this again.", t.Name()),
			IsError: true, Trusted: true,
		}, nil
	case lastErr != nil:
		// We lost visibility. Saying "nobody was asked" here would be a false
		// claim made out of a swallowed error -- the request may well have been
		// published and even fulfilled while these reads were failing.
		return tool.Result{
			Content: fmt.Sprintf("%s: gave up waiting -- the request was created, but its status could not be read "+
				"(last error: %v), so whether a human was ever asked is UNKNOWN. Do not assume nobody was asked. "+
				"Report the credential problem in your response rather than calling this again.", t.Name(), lastErr),
			IsError: true, Trusted: true,
		}, nil
	default:
		// Reads worked, InteractionRef stayed empty, and this request is not
		// riding on anybody else's card: no card was ever delivered (a silent
		// publication skip -- no InputChannel, no started-by subject, no
		// ResolvedCredential, or channelsd's signing key missing). "Timed out
		// waiting for a human decision" would be false; say so plainly rather
		// than implying a human saw and ignored this.
		return tool.Result{
			Content: fmt.Sprintf("%s: gave up waiting -- no card was ever delivered to a human, so nobody has been asked yet. "+
				"The request is still open and may still be delivered once the platform can reach a channel. "+
				"Report the credential problem in your response rather than calling this again.", t.Name()),
			IsError: true, Trusted: true,
		}, nil
	}
}

// canonicalDelivered reports whether the request a follower collapsed onto has
// really had a card put in front of somebody -- the ONE discriminator that
// makes "a human has already been asked" a fact rather than an inference from
// this request's phase.
//
// It reads the canonical, because a follower's own status deliberately carries
// none of delivery's marks (collapseOnto leaves InteractionRef and the
// CardDelivered condition untouched precisely so the expiry wording cannot
// claim a human was shown something). InteractionRef is the discriminator:
// channelsd stamps it in the SAME patch as CardDelivered=True, so the two can
// never disagree.
//
// It FAILS CLOSED -- false on a nil pointer, a canonical that no longer exists,
// and any read failure -- because the caller's true branch makes a positive
// claim about a human having been asked, while its false branch says only that
// a shared ask is in flight and its delivery is unconfirmed. That weaker
// sentence is true under every one of those uncertainties. A read failure is
// logged rather than dropped: it is the difference between the two messages the
// agent gets, and an operator asking "why did the agent say delivery was
// unconfirmed for a card I can see" needs the line.
func (t *credentialUpdateTool) canonicalDelivered(ctx context.Context, ref *spiceboxv1alpha1.NamespacedRef) bool {
	if ref == nil {
		// Phase says Collapsed but nothing records onto what. The reconciler
		// writes both in one status update, so this is the disagreement the
		// branch guard above deliberately admits -- and with no name to read,
		// delivery cannot be established.
		return false
	}
	var canonical spiceboxv1alpha1.CredentialUpdateRequest
	if err := t.cfg.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &canonical); err != nil {
		slog.Info("request_credential_update: could not read the request this one is waiting on; "+
			"reporting its delivery as unconfirmed rather than claiming a human was asked",
			"namespace", ref.Namespace, "name", ref.Name, "err", err.Error())
		return false
	}
	return canonical.Status.InteractionRef != ""
}

// capWhyRunes truncates why to at most maxWhyRunes UNICODE RUNES. A
// byte-based slice (why[:280]) can split a multi-byte rune in half and write
// invalid UTF-8 into spec.why.
func capWhyRunes(why string) string {
	r := []rune(why)
	if len(r) <= maxWhyRunes {
		return why
	}
	return string(r[:maxWhyRunes])
}
