//go:build e2e

// Package e2e — approval API.
//
// The harness's approval API mirrors the user-facing flow for a
// tool-approval round-trip:
//
//   - ExpectApprovalPrompt blocks until the fake sub-channel sender records
//     an envelope matching all predicates. The envelope arrives via:
//     runner.Check → publish a generic interaction_request(tool_approval) →
//     pipeline parks it on PendingInteractions → outbound relay → fake
//     sub-channel sender → Driver.approvalPrompts.
//
//   - Approval.Approve / Approval.Deny publishes a generic
//     interaction_decision envelope on the right NATS subject. The harness's
//     pipeline subscription (wired in harness.go) picks it up and runs
//     HandleInteractionDecision, which validates the decider's standing for
//     the tool_approval category, runs the bound grant handler (writes the
//     SpiceDB grant tuple on approve), and publishes interaction_applied for
//     the runner to resume on.
//
// No real names — tests use "user@example.com", "owner-1@example.com"
// per AGENTS.md / memory/feedback_no_real_names_in_code.md.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ApprovalPrompt is the harness's projection of one captured
// tool_approval_request envelope. RequestID is the opaque ID the
// runner generated; SessionRef is "<namespace>/<name>" of the
// AgentSession the approval pauses.
type ApprovalPrompt struct {
	RequestID    string
	Tool         string
	ResourceType string
	ResourceID   string
	Permission   string
	// Approver is the request envelope's ApproverSubject — a subject-set
	// expression like "group:eng#member". The actual clicker is supplied
	// at Approve / Deny time via AsUser; the pipeline validates clicker
	// ∈ ApproverSubject via SpiceDB.
	Approver   string
	SessionRef string
}

// ApprovalPredicate filters ApprovalPrompts. ExpectApprovalPrompt
// accepts a variadic of these and requires every predicate to match.
type ApprovalPredicate func(ApprovalPrompt) bool

// ForTool matches when the prompt is for the named tool.
func ForTool(name string) ApprovalPredicate {
	return func(p ApprovalPrompt) bool { return p.Tool == name }
}

// ForResource matches the "<type>:<id>" pair against the prompt's
// ResourceType and ResourceID. A malformed ref ("missing colon") will
// never match — there's no point fatalling at predicate-construction
// time when the test author can spot the typo via the matcher loop's
// timeout message.
func ForResource(ref string) ApprovalPredicate {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 {
		return func(_ ApprovalPrompt) bool { return false }
	}
	rt, rid := parts[0], parts[1]
	return func(p ApprovalPrompt) bool {
		return p.ResourceType == rt && p.ResourceID == rid
	}
}

// ForPermission matches when the prompt is for the named permission.
// Useful for tests that exercise multiple-permission flows on the same
// resource (e.g. "read" vs "write" approvals).
func ForPermission(name string) ApprovalPredicate {
	return func(p ApprovalPrompt) bool { return p.Permission == name }
}

// Approval is the handle returned by ExpectApprovalPrompt. Tests call
// Approve or Deny to drive the decision back through the pipeline.
type Approval struct {
	h      *Harness
	prompt ApprovalPrompt
}

// Prompt returns a copy of the captured envelope projection. Useful
// for tests that want to assert on additional fields after matching.
func (a *Approval) Prompt() ApprovalPrompt { return a.prompt }

// ExpectApprovalPrompt blocks until the fake sub-channel sender
// records a tool_approval_request envelope matching every predicate,
// or DefaultTimeout elapses. On timeout, fatals with the count of
// prompts seen — a non-zero count usually means "the approval rendered,
// but the predicate didn't match"; zero means "the runner never
// published a tool_approval_request" (no Check failure, missing
// GrantWriter wiring, etc.).
func (h *Harness) ExpectApprovalPrompt(preds ...ApprovalPredicate) *Approval {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	var candidates []ApprovalPrompt
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectApprovalPrompt")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			// Slice C2: tool_call rides the generic Interaction model, so the
			// prompt is drained from the same InteractionPrompts() queue as
			// content_inspection / identity_choice, filtered by Category.
			prompts := drv.InteractionPrompts()
			for i := seen; i < len(prompts); i++ {
				if prompts[i].Payload.Category != categories.ToolApproval {
					continue
				}
				p := promptFromInteraction(prompts[i])
				if matchesAllApproval(p, preds) {
					return &Approval{h: h, prompt: p}
				}
				// Kept for the timeout message. A tool_approval prompt that
				// arrived and failed the predicates is a COMPLETELY different
				// diagnosis from none arriving, and reporting only a count made
				// the two indistinguishable.
				candidates = append(candidates, p)
			}
			seen = len(prompts)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectApprovalPrompt: timed out after %s\ntool_approval prompts that arrived but did not match: %s\n%s",
		h.opts.DefaultTimeout, describeApprovalCandidates(candidates), h.dumpState())
	return nil
}

// describeApprovalCandidates renders the tool_approval prompts that arrived but
// failed the predicates.
//
// This exists because the old message — a bare count of prompts seen — reported
// "seen 0 prompt(s)" in the one case where the count is most misleading: a
// prompt HAD been published, with the right category, and simply carried a
// different tool or resource than the test asked for. The reader then goes
// hunting for a publication bug that does not exist. ForResource's own doc
// already promises this message is where a mismatch gets spotted; this is it
// keeping that promise.
func describeApprovalCandidates(candidates []ApprovalPrompt) string {
	if len(candidates) == 0 {
		return "none — no tool_approval prompt was published at all (this is a publication " +
			"problem, not a predicate mismatch)"
	}
	var b strings.Builder
	for _, p := range candidates {
		fmt.Fprintf(&b, "\n  tool=%q resource=%s:%s permission=%s requestID=%s",
			p.Tool, p.ResourceType, p.ResourceID, p.Permission, p.RequestID)
	}
	return b.String()
}

// Approve publishes a tool_approval_decision envelope with
// decision="approve". The pipeline subscription picks it up,
// validates the approver, writes the SpiceDB grant tuple, and
// publishes tool_approval_applied for the runner to resume.
// Returns once the envelope is published + flushed; the SpiceDB
// write happens asynchronously in the subscription handler.
func (a *Approval) Approve(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("approve", opts...)
}

// Deny publishes the deny variant.
func (a *Approval) Deny(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("deny", opts...)
}

func (a *Approval) decide(decision string, opts ...SendOption) {
	cfg := sendCfg{user: a.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	ns, name, ok := splitSessionRef(a.prompt.SessionRef)
	if !ok {
		a.h.t.Fatalf("Approval.%s: malformed SessionRef %q", decision, a.prompt.SessionRef)
	}
	// Slice C2: tool_call decisions ride the generic interaction_decision (the
	// pipe's DecideResourceOwners policy validates the clicker server-side; the
	// bound BindToolApprovalHandler writes the grant on approve). The ActionID is
	// "approve"/"deny" — the ids approveDenyActions stamps on the request.
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        categories.ToolApproval,
		RequestRef:      a.prompt.RequestID,
		ActionID:        decision,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return a.h.nc.Publish(subj, b) },
		ns, name, channelevents.KindInteractionDecision, payload,
	); err != nil {
		a.h.t.Fatalf("Approval.%s: publish interaction_decision: %v", decision, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		// Flush failure is rare; log so a downstream timeout has
		// context but don't fatal — the pipeline subscription may
		// still process the in-flight publish.
		a.h.t.Logf("Approval.%s: nats Flush: %v", decision, err)
	}
}

// WaitApplied blocks until a tool_approval_applied envelope for this
// approval's RequestID lands on the runner's IN subject, or
// DefaultTimeout elapses. Useful for tests that want to assert the
// SpiceDB grant write completed before issuing a follow-up
// SendUserMessage / ExpectAgentReply. The runner-resume path subscribes
// to the same envelope; tests that drive the runner end-to-end usually
// don't need this helper.
func (a *Approval) WaitApplied(ctx context.Context) error {
	a.h.t.Helper()
	ns, name, ok := splitSessionRef(a.prompt.SessionRef)
	if !ok {
		return fmt.Errorf("WaitApplied: malformed SessionRef %q", a.prompt.SessionRef)
	}
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionApplied)
	sub, err := a.h.nc.SubscribeSync(subject)
	if err != nil {
		return fmt.Errorf("WaitApplied: subscribe %s: %w", subject, err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	deadline := time.Now().Add(a.h.opts.DefaultTimeout)
	// Honor a caller-supplied earlier ctx deadline: NextMsg below blocks for
	// `remaining`, so without folding the ctx deadline in, a short ctx (the
	// negative "no applied envelope expected" checks) would stall the full
	// DefaultTimeout instead.
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			return fmt.Errorf("WaitApplied: NextMsg: %w", err)
		}
		var env channelevents.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			continue
		}
		var pl channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			continue
		}
		if pl.Category == categories.ToolApproval && pl.RequestRef == a.prompt.RequestID {
			return nil
		}
	}
	return fmt.Errorf("WaitApplied: timed out after %s waiting for requestID=%s",
		a.h.opts.DefaultTimeout, a.prompt.RequestID)
}

func matchesAllApproval(p ApprovalPrompt, preds []ApprovalPredicate) bool {
	for _, pred := range preds {
		if !pred(p) {
			return false
		}
	}
	return true
}

// promptFromInteraction projects a recorded interaction_request (category
// tool_approval) into an ApprovalPrompt. Slice C2: the tool/permission/resource
// the predicates match on now live in the request's Details (ToolApprovalDetails)
// + Resources + Fields rather than the legacy typed ToolApprovalRequestPayload.
func promptFromInteraction(r fakekind.InteractionPrompt) ApprovalPrompt {
	p := ApprovalPrompt{
		RequestID:  r.Payload.RequestRef,
		SessionRef: r.SessionRef.Namespace + "/" + r.SessionRef.Name,
	}
	var det channelevents.ToolApprovalDetails
	if len(r.Payload.Details) > 0 {
		_ = json.Unmarshal(r.Payload.Details, &det)
	}
	p.Permission = det.Permission
	p.ResourceType = det.ResourceType
	p.ResourceID = det.ResourceID
	// Tool name comes from the DETAILS, which carry wire identity. The visible
	// "Tool" field holds the tool's description — human copy, rewordable at any
	// time — so matching on it coupled every approval scenario to card wording.
	// The field is kept as a fallback only for a payload written before
	// ToolApprovalDetails.ToolName existed.
	p.Tool = det.ToolName
	if p.Tool == "" {
		for _, f := range r.Payload.Fields {
			if f.Label == "Tool" {
				p.Tool = strings.Trim(f.Value, "`")
			}
		}
	}
	// Approver routing subject. Production fans the resource-owner UNION out into
	// Audience.Approvers (individual users), but the standing is derived from the
	// request's Resources — so the routing subject the prompt is gated on is the
	// resource's #owner set. Mirror that: when the request names a resource (in the
	// tool Details, else Resources[0]), the approver is "<type>:<id>#owner"; only a
	// resource-less request (session-set gate) falls back to the addressed approver.
	switch {
	case det.ResourceType != "" && det.ResourceID != "":
		p.Approver = det.ResourceType + ":" + det.ResourceID + "#owner"
	case len(r.Payload.Resources) > 0:
		p.Approver = r.Payload.Resources[0].Type + ":" + r.Payload.Resources[0].ID + "#owner"
	case len(r.Payload.Audience.Approvers) > 0:
		p.Approver = r.Payload.Audience.Approvers[0].Subject.String()
	}
	return p
}

func splitSessionRef(ref string) (ns, name string, ok bool) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// SessionJoinPrompt is the harness's projection of one captured
// interaction_request envelope (category permission_request) — the
// multiplayer-session-join equivalent of ApprovalPrompt. Distinct from
// ApprovalPrompt because the approver-resolution path is independent of the
// tool-approval flow: tool-approval validates the clicker against an
// ApproverSubject set, session-join requires the clicker to be the session's
// owner (started_by).
//
// Tasks 9/10 flipped the session-join publisher off the bespoke
// KindPermissionRequest envelope (which carried Requester/StartedBy/Preview
// as structured fields) onto the generic InteractionRequestPayload, which
// addresses the OWNER via Audience.Approvers but carries no structured
// requester identity — StartedBy is read from Audience.Approvers[0] (email
// falling back to ExternalID via identityHandle); Requester and Preview are
// read from the AgentSession's status.pendingRequesters entry keyed by
// RequestRef, the exact same source decidePermission itself reads (see
// pkg/channels/channelsd/pipeline/permission_interaction.go and
// sessionJoinPromptFromInteraction below).
type SessionJoinPrompt struct {
	AgentSessionRef channelevents.SessionRef
	// RequestRef identifies the pending request this prompt decides — the
	// generic interaction_decision path (unlike the retired
	// KindPermissionDecision) resolves the PendingRequesters entry by this
	// ref, not by requester identity, so it must round-trip to decide().
	RequestRef     string
	Requester      string
	StartedBy      string
	Preview        string
	AgentClassName string
}

// JoinPredicate filters SessionJoinPrompts. ExpectSessionJoinPrompt
// accepts a variadic of these and requires every predicate to match.
type JoinPredicate func(SessionJoinPrompt) bool

// JoinFromRequester matches when the requester's email equals email.
// The fake channel kind uses email-as-external-id (see
// SendUserMessage's InboundEvent construction), so this matches the
// AsUser email passed to the second SendUserMessage.
func JoinFromRequester(email string) JoinPredicate {
	return func(p SessionJoinPrompt) bool { return p.Requester == email }
}

// JoinToStartedBy matches when the prompt's StartedBy email equals
// email — the user who started the session (and whose DM the request
// is destined for).
func JoinToStartedBy(email string) JoinPredicate {
	return func(p SessionJoinPrompt) bool { return p.StartedBy == email }
}

// SessionJoinApproval is the handle returned by ExpectSessionJoinPrompt.
// Tests call Approve or Deny to drive the decision back through the
// pipeline's HandleInteractionDecision (subscribed in harness.go), which
// invokes decidePermission — the bound handler for categories.PermissionRequest
// (pkg/channels/channelsd/pipeline/permission_interaction.go). The retired
// KindPermissionDecision / HandleDecision path this used to drive was removed
// when Tasks 9/10 migrated session-join onto the generic Interaction model.
type SessionJoinApproval struct {
	h          *Harness
	prompt     SessionJoinPrompt
	sessionRef channelkinds.SessionInfo
}

// Prompt returns a copy of the captured envelope projection. Useful
// for tests that want to assert on Preview / AgentClassName after
// matching.
func (a *SessionJoinApproval) Prompt() SessionJoinPrompt { return a.prompt }

// ExpectSessionJoinPrompt blocks until the fake "interaction" sub-channel
// sender records a KindInteractionRequest envelope with Category ==
// categories.PermissionRequest matching every predicate, or DefaultTimeout
// elapses. On timeout, fatals with the count of prompts seen — a non-zero
// count usually means "the request rendered, but the predicate didn't
// match"; zero means "the pipeline never published a permission_request
// interaction_request" (most commonly: authz.session.interactPermission unset
// on the AgentClass so the pipeline silently routes the second user through
// the broad-grant path, OR the second user's Check is somehow passing).
func (h *Harness) ExpectSessionJoinPrompt(preds ...JoinPredicate) *SessionJoinApproval {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectSessionJoinPrompt")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			prompts := drv.InteractionPrompts()
			for i := seen; i < len(prompts); i++ {
				if prompts[i].Payload.Category != categories.PermissionRequest {
					seen = i + 1
					continue
				}
				p, ok := h.sessionJoinPromptFromInteraction(prompts[i])
				if !ok {
					// The AgentSession's status.pendingRequesters entry
					// hasn't landed in the harness's cached client yet — the
					// pipeline writes status BEFORE publishing and verifies
					// the readback server-side, so this is a transient
					// client-cache lag, not a real absence. Retry this same
					// index next iteration instead of skipping it forever.
					break
				}
				seen = i + 1
				if matchesAllJoin(p, preds) {
					return &SessionJoinApproval{
						h:          h,
						prompt:     p,
						sessionRef: prompts[i].SessionRef,
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectSessionJoinPrompt: timed out after %s (seen %d prompt(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return nil
}

// sessionJoinPromptFromInteraction projects a recorded interaction_request
// (category permission_request) into a SessionJoinPrompt, reading the
// requester identity + preview off the AgentSession's
// status.pendingRequesters entry matched by RequestRef — see the
// SessionJoinPrompt doc comment for why the envelope itself doesn't carry
// them. ok is false when no matching entry is found yet.
func (h *Harness) sessionJoinPromptFromInteraction(r fakekind.InteractionPrompt) (SessionJoinPrompt, bool) {
	var sess spiceboxv1alpha1.AgentSession
	if err := h.K8s.Get(context.Background(), client.ObjectKey{
		Namespace: r.SessionRef.Namespace, Name: r.SessionRef.Name,
	}, &sess); err != nil {
		return SessionJoinPrompt{}, false
	}
	var pr *spiceboxv1alpha1.PendingRequester
	for i := range sess.Status.PendingRequesters {
		if sess.Status.PendingRequesters[i].RequestRef == r.Payload.RequestRef {
			pr = &sess.Status.PendingRequesters[i]
			break
		}
	}
	if pr == nil {
		return SessionJoinPrompt{}, false
	}
	startedBy := ""
	if len(r.Payload.Audience.Approvers) > 0 {
		startedBy = identityHandle(r.Payload.Audience.Approvers[0])
	}
	requester := pr.Email
	if requester == "" {
		requester = pr.ExternalID
	}
	return SessionJoinPrompt{
		AgentSessionRef: r.Payload.AgentSessionRef,
		RequestRef:      r.Payload.RequestRef,
		Requester:       requester,
		StartedBy:       startedBy,
		Preview:         pr.MessageText,
		AgentClassName:  sess.Spec.Class,
	}, true
}

// Approve publishes an interaction_decision envelope with actionId="approve"
// for this prompt's request. HandleInteractionDecision validates the decider
// has approver standing (the session's owner — see categories.PermissionRequest's
// DecideOwner policy), then decidePermission grants the requester `interact`,
// clears the matching PendingRequester, and replays the requester's stashed
// message through the inbound pipeline so the original message is processed
// without the user having to retype.
func (a *SessionJoinApproval) Approve(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("approve", opts...)
}

// Deny publishes the deny variant. On deny, decidePermission writes a
// denied tuple so subsequent posts from the same requester are
// silently dropped; the requester's stashed message is NOT replayed.
func (a *SessionJoinApproval) Deny(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("deny", opts...)
}

// DecideFromForeignSubject publishes the SAME decision envelope Approve/Deny
// publishes — same payload, same env.Session (this prompt's session) — on
// ANOTHER session's inbound subject: "ap.session.<ns>.<foreignName>.in.
// interaction_decision".
//
// It is the attack the inbound subject-authority gate exists to refuse. A
// runner's per-session NATS JWT permits publishing under exactly one
// "ap.session.<ns>.<own-name>.>" tree, and that tree covers ".in." as well as
// ".out.", so a compromised session can publish anything it likes on its OWN
// inbound subject — including an envelope labelled with a victim session. If a
// consumer routes on env.Session without cross-checking the subject, that
// publish resolves the victim's parked approval.
//
// The only difference from Approve is the subject, so a scenario using both
// isolates exactly the property under test. Kept beside decide so the payload
// can never drift between the honest call and the forged one.
func (a *SessionJoinApproval) DecideFromForeignSubject(foreignSessionName, decision string, opts ...SendOption) {
	a.h.t.Helper()
	env, err := channelevents.BuildEnvelope(
		a.sessionRef.Namespace, a.sessionRef.Name,
		channelevents.KindInteractionDecision, a.decisionPayload(decision, opts))
	if err != nil {
		a.h.t.Fatalf("SessionJoinApproval.DecideFromForeignSubject(%s): build envelope: %v", decision, err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		a.h.t.Fatalf("SessionJoinApproval.DecideFromForeignSubject(%s): marshal envelope: %v", decision, err)
	}
	subject := channelevents.SubjectIn(
		channelevents.SubjectPrefix(a.sessionRef.Namespace, foreignSessionName),
		channelevents.KindInteractionDecision)
	if err := a.h.nc.Publish(subject, b); err != nil {
		a.h.t.Fatalf("SessionJoinApproval.DecideFromForeignSubject(%s): publish on %s: %v", decision, subject, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		a.h.t.Logf("SessionJoinApproval.DecideFromForeignSubject(%s): nats Flush: %v", decision, err)
	}
}

// decide publishes a channelevents.KindInteractionDecision envelope on the
// `.in.interaction_decision` subject — mirroring IdentityChoice.Choose
// (identity_choice.go), the harness's equivalent of a channel-surface
// Approve/Deny click on a generic-Interaction prompt.
func (a *SessionJoinApproval) decide(decision string, opts ...SendOption) {
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return a.h.nc.Publish(subj, b) },
		a.sessionRef.Namespace, a.sessionRef.Name,
		channelevents.KindInteractionDecision, a.decisionPayload(decision, opts),
	); err != nil {
		a.h.t.Fatalf("SessionJoinApproval.%s: publish interaction_decision: %v", decision, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		// Flush failure is rare; log so a downstream timeout has
		// context but don't fatal — the pipeline subscription may
		// still process the in-flight publish.
		a.h.t.Logf("SessionJoinApproval.%s: nats Flush: %v", decision, err)
	}
}

// decisionPayload builds the decision payload both the honest publish and
// DecideFromForeignSubject send. The decider identity defaults to the harness
// DefaultUser; AsUser overrides it.
func (a *SessionJoinApproval) decisionPayload(decision string, opts []SendOption) channelevents.InteractionDecisionPayload {
	cfg := sendCfg{user: a.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	return channelevents.InteractionDecisionPayload{
		AgentSessionRef: a.prompt.AgentSessionRef,
		Category:        categories.PermissionRequest,
		RequestRef:      a.prompt.RequestRef,
		ActionID:        decision,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
}

func matchesAllJoin(p SessionJoinPrompt, preds []JoinPredicate) bool {
	for _, pred := range preds {
		if !pred(p) {
			return false
		}
	}
	return true
}

// LeakageApprovalPrompt is the harness's projection of one captured
// info_leakage interaction_request. Mirrors ApprovalPrompt but
// for the data-sharing approval flow rather than tool-call approval.
//
// LeakedTo is the set of channel-side external identities the proposed
// response would expose tainted data to; tests assert on it to verify
// the gate computed the right "audience minus permitted" subset.
// AccessedResources is the SpiceDB-keyed list of resources the runner
// read into context; tests assert on it to verify the runner's
// post-execute taint recording is feeding the gate correctly.
//
// Slice C2: the generic renderer names recipients in a "Would share with" render
// Field rather than as typed identities, so LeakedTo is left empty on this path;
// WouldShareWith exposes those named recipients (the bare canonical id the host
// stamped, `user:` prefix stripped — see infoLeakageWouldShareWithFields). The
// render shows the canonical as plain TEXT, not a Slack @mention; restoring the
// @mention needs structured leaked-to in Details plus a renderer enhancement
// (tracked follow-up), so tests assert the recipient is NAMED here.
type LeakageApprovalPrompt struct {
	RequestID         string
	Approver          string
	What              string
	LeakedTo          []channelevents.ExternalIdentity
	WouldShareWith    []string
	AccessedResources []channelevents.InteractionResourceRef
	SessionRef        string
}

// LeakageApprovalPredicate filters LeakageApprovalPrompts.
type LeakageApprovalPredicate func(LeakageApprovalPrompt) bool

// ForLeakageRequestID matches the prompt's RequestID. Useful when a test
// publishes a synthetic envelope and wants to disambiguate from other
// prompts in the driver's queue across the test process.
func ForLeakageRequestID(id string) LeakageApprovalPredicate {
	return func(p LeakageApprovalPrompt) bool { return p.RequestID == id }
}

// ForLeakageResource matches when the prompt's AccessedResources include
// the named "<type>:<id>" pair. Tests use this to assert the runner's
// taint flowed through to the approval envelope.
func ForLeakageResource(ref string) LeakageApprovalPredicate {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 {
		return func(_ LeakageApprovalPrompt) bool { return false }
	}
	rt, rid := parts[0], parts[1]
	return func(p LeakageApprovalPrompt) bool {
		for _, r := range p.AccessedResources {
			if r.Type == rt && r.ID == rid {
				return true
			}
		}
		return false
	}
}

// LeakageApproval is the handle returned by ExpectLeakageApprovalPrompt.
// Tests call Approve or Deny to drive the decision back through the pipeline
// as a generic interaction_decision(info_leakage). The pipeline's
// HandleInteractionDecision validates the decider's data-owner standing,
// runs the bound handler, clears the matching PendingInteractions entry, and
// publishes interaction_applied on both IN (runner resume) and OUT (kind
// sender ack).
type LeakageApproval struct {
	h      *Harness
	prompt LeakageApprovalPrompt
}

// Prompt returns a copy of the captured envelope projection.
func (a *LeakageApproval) Prompt() LeakageApprovalPrompt { return a.prompt }

// ExpectLeakageApprovalPrompt blocks until the fake info_leakage_approval
// sub-channel sender records an envelope matching every predicate, or
// DefaultTimeout elapses. Mirrors ExpectApprovalPrompt for the
// data-sharing approval flow.
func (h *Harness) ExpectLeakageApprovalPrompt(preds ...LeakageApprovalPredicate) *LeakageApproval {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectLeakageApprovalPrompt")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			// Slice C2: info_leakage rides the generic Interaction model, so the
			// prompt is drained from InteractionPrompts() filtered by Category.
			prompts := drv.InteractionPrompts()
			for i := seen; i < len(prompts); i++ {
				if prompts[i].Payload.Category != categories.InfoLeakage {
					continue
				}
				p := leakagePromptFromInteraction(prompts[i])
				if matchesAllLeakage(p, preds) {
					return &LeakageApproval{h: h, prompt: p}
				}
			}
			seen = len(prompts)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectLeakageApprovalPrompt: timed out after %s (seen %d prompt(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return nil
}

// Approve publishes an info_leakage_approval_decision envelope with
// decision="approve". The pipeline subscription validates the approver,
// clears the pending entry, and publishes Applied on both IN and OUT.
// The runner consumes Applied and writes the infoleakage_grant tuple.
func (a *LeakageApproval) Approve(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("approve", opts...)
}

// Deny publishes the deny variant. The runner consumes Applied with
// Approved=false and returns the leakage-gate error to the LLM.
func (a *LeakageApproval) Deny(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("deny", opts...)
}

func (a *LeakageApproval) decide(decision string, opts ...SendOption) {
	cfg := sendCfg{user: a.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	ns, name, ok := splitSessionRef(a.prompt.SessionRef)
	if !ok {
		a.h.t.Fatalf("LeakageApproval.%s: malformed SessionRef %q", decision, a.prompt.SessionRef)
	}
	// Slice C2: info_leakage decisions ride the generic interaction_decision (the
	// pipe's DecideResourceOwners policy gates on the taint data #owner; the pure
	// ApprovalDecisionHandler records the decision, the runner writes the grant).
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        categories.InfoLeakage,
		RequestRef:      a.prompt.RequestID,
		ActionID:        decision,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return a.h.nc.Publish(subj, b) },
		ns, name, channelevents.KindInteractionDecision, payload,
	); err != nil {
		a.h.t.Fatalf("LeakageApproval.%s: publish interaction_decision: %v", decision, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		a.h.t.Logf("LeakageApproval.%s: nats Flush: %v", decision, err)
	}
}

func matchesAllLeakage(p LeakageApprovalPrompt, preds []LeakageApprovalPredicate) bool {
	for _, pred := range preds {
		if !pred(p) {
			return false
		}
	}
	return true
}

// leakagePromptFromInteraction projects a recorded interaction_request (category
// info_leakage) into a LeakageApprovalPrompt. Slice C2: the accessed resources
// the predicates match on now live in the request's Resources; the recipients are
// named in a "Would share with" Field (unstructured render copy, not typed
// identities), so LeakedTo is left empty and WouldShareWith carries those named
// recipients instead — tests assert the recipient is NAMED there.
func leakagePromptFromInteraction(r fakekind.InteractionPrompt) LeakageApprovalPrompt {
	accessed := append([]channelevents.InteractionResourceRef(nil), r.Payload.Resources...)
	approver := ""
	if len(r.Payload.Audience.Approvers) > 0 {
		approver = r.Payload.Audience.Approvers[0].Subject.String()
	}
	// The host stamps recipients into a single "Would share with" Field, comma-
	// joined (infoLeakageWouldShareWithFields). Canonical ids are base64 and carry
	// no commas, so a ", " split recovers the individual named recipients.
	var wouldShareWith []string
	for _, f := range r.Payload.Fields {
		if f.Label != "Would share with" {
			continue
		}
		for _, name := range strings.Split(f.Value, ", ") {
			if name = strings.TrimSpace(name); name != "" {
				wouldShareWith = append(wouldShareWith, name)
			}
		}
	}
	return LeakageApprovalPrompt{
		RequestID:         r.Payload.RequestRef,
		Approver:          approver,
		What:              r.Payload.Lead,
		WouldShareWith:    wouldShareWith,
		AccessedResources: accessed,
		SessionRef:        r.SessionRef.Namespace + "/" + r.SessionRef.Name,
	}
}

// ContentInspectionApprovalPrompt is the harness's projection of one captured
// content_inspection interaction_request — the content-guard approve-action
// prompt. Slice C1 migrated content_inspection off the legacy
// KindContentInspectionApprovalRequest onto the generic Interaction model, so
// the inspector-supplied metadata (tool, inspector, score, threshold, i/o
// point) is now authored into the request Lead by the runner host's
// contentInspectionLead rather than carried as separate structured fields;
// tests assert on it via Lead. RequestID is the interaction RequestRef; the
// actual clicker (decider) is supplied at Approve/Deny time.
type ContentInspectionApprovalPrompt struct {
	RequestID       string
	Lead            string
	SessionRef      string
	AgentSessionRef channelevents.SessionRef
}

// ContentInspectionApprovalPredicate filters ContentInspectionApprovalPrompts.
type ContentInspectionApprovalPredicate func(ContentInspectionApprovalPrompt) bool

// ForContentInspectionTool matches when the prompt's Lead names the tool. The
// runner host authors the tool as a backtick-quoted token in the Lead
// (contentInspectionLead), so we match on that exact rendering.
func ForContentInspectionTool(name string) ContentInspectionApprovalPredicate {
	return func(p ContentInspectionApprovalPrompt) bool { return strings.Contains(p.Lead, "`"+name+"`") }
}

// ForContentInspectionInspector matches when the prompt's Lead names the
// inspector id (e.g. "prompt-injection"), authored as a backtick-quoted token.
func ForContentInspectionInspector(id string) ContentInspectionApprovalPredicate {
	return func(p ContentInspectionApprovalPrompt) bool { return strings.Contains(p.Lead, "`"+id+"`") }
}

// ContentInspectionApproval is the handle returned by
// ExpectContentInspectionApprovalPrompt. Tests call Approve or Deny to drive
// the decision back through the pipeline as a generic interaction_decision.
// HandleInteractionDecision validates the decider against the session
// approve-set (content_inspection's DecideApprovers policy), invokes
// ApprovalDecisionHandler, and publishes Applied on both IN (pending cleanup)
// and OUT (runner resume, via subscribeFactoryInteractionApplied).
type ContentInspectionApproval struct {
	h          *Harness
	prompt     ContentInspectionApprovalPrompt
	sessionRef channelkinds.SessionInfo
}

// Prompt returns a copy of the captured envelope projection.
func (a *ContentInspectionApproval) Prompt() ContentInspectionApprovalPrompt { return a.prompt }

// ExpectContentInspectionApprovalPrompt blocks until the fake "interaction"
// sub-channel sender records a KindInteractionRequest envelope with Category ==
// categories.ContentInspection matching every predicate, or DefaultTimeout
// elapses. Slice C1: content_inspection rides the generic Interaction model, so
// the prompt is drained from the same InteractionPrompts() queue as
// permission_request / identity_choice, filtered by Category.
func (h *Harness) ExpectContentInspectionApprovalPrompt(preds ...ContentInspectionApprovalPredicate) *ContentInspectionApproval {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectContentInspectionApprovalPrompt")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			prompts := drv.InteractionPrompts()
			for i := seen; i < len(prompts); i++ {
				if prompts[i].Payload.Category != categories.ContentInspection {
					continue
				}
				p := contentInspectionPromptFromInteraction(prompts[i])
				if matchesAllContentInspection(p, preds) {
					return &ContentInspectionApproval{h: h, prompt: p, sessionRef: prompts[i].SessionRef}
				}
			}
			seen = len(prompts)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectContentInspectionApprovalPrompt: timed out after %s (seen %d prompt(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return nil
}

// Approve publishes an interaction_decision envelope with actionId="approve".
// The runner consumes Applied with Approved=true and the executor lets the
// flagged tool I/O through unchanged.
func (a *ContentInspectionApproval) Approve(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("approve", opts...)
}

// Deny publishes the deny variant (actionId="deny"). The runner consumes
// Applied with Approved=false; the executor overwrites the flagged tool result
// with an IsError so the content never reaches the model.
func (a *ContentInspectionApproval) Deny(opts ...SendOption) {
	a.h.t.Helper()
	a.decide("deny", opts...)
}

// decide publishes a channelevents.KindInteractionDecision envelope on the
// `.in.interaction_decision` subject — mirroring SessionJoinApproval.decide
// (the harness's equivalent of a channel-surface Approve/Deny click on a
// generic-Interaction prompt). The Decider mirrors the acting user into both
// ExternalID and Email (InteractionDecisionPayload.Validate requires a non-empty
// Kind+ExternalID), matching the CLI's decider construction.
func (a *ContentInspectionApproval) decide(decision string, opts ...SendOption) {
	cfg := sendCfg{user: a.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: a.prompt.AgentSessionRef,
		Category:        categories.ContentInspection,
		RequestRef:      a.prompt.RequestID,
		ActionID:        decision,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return a.h.nc.Publish(subj, b) },
		a.sessionRef.Namespace, a.sessionRef.Name,
		channelevents.KindInteractionDecision, payload,
	); err != nil {
		a.h.t.Fatalf("ContentInspectionApproval.%s: publish interaction_decision: %v", decision, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		a.h.t.Logf("ContentInspectionApproval.%s: nats Flush: %v", decision, err)
	}
}

func matchesAllContentInspection(p ContentInspectionApprovalPrompt, preds []ContentInspectionApprovalPredicate) bool {
	for _, pred := range preds {
		if !pred(p) {
			return false
		}
	}
	return true
}

func contentInspectionPromptFromInteraction(r fakekind.InteractionPrompt) ContentInspectionApprovalPrompt {
	return ContentInspectionApprovalPrompt{
		RequestID:       r.Payload.RequestRef,
		Lead:            r.Payload.Lead,
		SessionRef:      r.SessionRef.Namespace + "/" + r.SessionRef.Name,
		AgentSessionRef: r.Payload.AgentSessionRef,
	}
}

// identityHandle returns the most user-meaningful handle for a
// channel-side ExternalIdentity: Email if set, else ExternalID. The
// pipeline's PermissionRequestPayload populates StartedBy.ExternalID
// from the session annotation and leaves Email empty (the annotation
// doesn't store email), so callers matching on email-like strings need
// the fallback. The fake channel kind sets ExternalID==Email on
// inbound, making the fallback consistent for the requester side too.
func identityHandle(id channelevents.ExternalIdentity) string {
	if id.Email != "" {
		return id.Email.String()
	}
	return id.ExternalID.String()
}

// PublishedApprovalPrompts returns every interaction prompt the bound channel
// has been asked to show, without waiting for one.
//
// The Expect* helpers all BLOCK for a prompt to arrive, which cannot express
// "and nothing was published" — the defining property of the plan gate's
// logging mode. This is the non-blocking read that can.
// Safe to call from a background goroutine: it resolves the channel itself
// rather than via a t.Fatalf-ing helper. A Fatalf outside the test goroutine
// calls runtime.Goexit and silently kills the caller, which is exactly how the
// first version of bronzethread's auto-approver died without a word.
func (h *Harness) PublishedApprovalPrompts() []fakekind.InteractionPrompt {
	var channels spiceboxv1alpha1.ChannelList
	if err := h.K8s.List(context.Background(), &channels); err != nil {
		return nil
	}
	// Every kind=fake Channel, merged, rather than "the one Channel". A fixture
	// with a second Channel — a github input paired with an output surface — is
	// not an ambiguity here the way it is for singleChannel: prompts are drained
	// from a driver, and a driver that recorded nothing contributes nothing.
	// Requiring exactly one made this return nil for such a fixture, which reads
	// downstream as "nothing was published" — so a PublishedNothing assertion
	// would have passed vacuously and the auto-approver would have watched an
	// empty list forever.
	var out []fakekind.InteractionPrompt
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.Kind != "fake" {
			continue
		}
		if drv := fakekind.DriverFor(ch.Namespace, ch.Name); drv != nil {
			out = append(out, drv.InteractionPrompts()...)
		}
	}
	return out
}

// ApproveInteraction publishes an approve decision for an arbitrary interaction
// prompt, whatever its category.
//
// The Expect*/Approve pair is per-category and BLOCKS for a prompt to arrive;
// this clears whichever prompt the caller already has. Bronzethread's
// background approver needs that: it watches every category a bundle opted into
// and cannot know in advance which will appear, or when.
// opts set the decider; without them the harness DefaultUser decides. A
// scenario passes AsUser when the approver is deliberately NOT the requester —
// the ordinary case for a tool approval, where the person with standing on the
// resource is someone else entirely.
func (h *Harness) ApproveInteraction(prompt fakekind.InteractionPrompt, opts ...SendOption) {
	h.t.Helper()
	h.decideInteraction(prompt, "approve", opts...)
}

// DenyInteraction is ApproveInteraction's negative, for a scenario whose point
// is that the request was refused.
func (h *Harness) DenyInteraction(prompt fakekind.InteractionPrompt, opts ...SendOption) {
	h.t.Helper()
	h.decideInteraction(prompt, "deny", opts...)
}

func (h *Harness) decideInteraction(prompt fakekind.InteractionPrompt, actionID string, opts ...SendOption) {
	h.t.Helper()
	ns, name := prompt.SessionRef.Namespace, prompt.SessionRef.Name
	if ns == "" || name == "" {
		h.t.Logf("decideInteraction: prompt has no session ref; skipping")
		return
	}
	cfg := sendCfg{user: h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        prompt.Payload.Category,
		RequestRef:      prompt.Payload.RequestRef,
		ActionID:        actionID,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return h.nc.Publish(subj, b) },
		ns, name, channelevents.KindInteractionDecision, payload,
	); err != nil {
		h.t.Logf("decideInteraction(%s): publish: %v", actionID, err)
		return
	}
	if err := h.nc.Flush(); err != nil {
		h.t.Logf("decideInteraction(%s): nats Flush: %v", actionID, err)
	}
}
