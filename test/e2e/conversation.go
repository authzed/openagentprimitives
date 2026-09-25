//go:build e2e

package e2e

import (
	"context"
	"regexp"
	"strings"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SendOption customizes how SendUserMessage shapes the synthetic inbound.
// The defaults (DefaultUser, "default-thread") are what a single-turn
// ping/pong test wants; multi-thread scenarios use InThread, and
// permission scenarios use AsUser to inject a non-owner identity.
type SendOption func(*sendCfg)

type sendCfg struct {
	user, thread string
	attachments  []channelkinds.InboundAttachment
}

// AsUser overrides the requester identity for this inbound — both the
// channel-side ExternalID and the email that drives the SpiceDB
// canonical-user computation. Pass "alice@example.com" etc; never echo a
// real name (see AGENTS.md "no real names in code").
func AsUser(email string) SendOption { return func(c *sendCfg) { c.user = email } }

// InThread overrides the channel-key (the pipeline's
// channelKey→sessionName index). Default is "default-thread"; tests that
// want to exercise multi-session correlation pass distinct keys per send.
func InThread(key string) SendOption { return func(c *sendCfg) { c.thread = key } }

// WithAttachments makes this inbound carry files, exactly as a real listener
// records them: ungated, with no capability check and no Channel lookup, since
// noticing a file exists is always allowed. Whether any bytes are fetched is
// the pipeline's decision downstream, which is the point of driving it from
// here rather than stubbing the gate.
//
// The bytes themselves come from whatever the caller installed with
// fake.SetAttachmentSource, keyed by each attachment's ExternalID.
func WithAttachments(atts ...channelkinds.InboundAttachment) SendOption {
	return func(c *sendCfg) { c.attachments = atts }
}

// SendUserMessage injects an inbound user message into the harness's
// (single) fake Channel. Fire-and-forget; the next ExpectAgentReply call
// blocks until a matching outbound envelope reaches the fake driver, so
// the typical test pattern is:
//
//	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong"))
//	h.SendUserMessage("ping")
//	h.ExpectAgentReply(e2e.Contains("pong"))
//
// Assumes one Channel CR in the namespace. Multi-channel scenarios will
// need an explicit channel selector — out of scope for T9.
//
// Send only AFTER a barrier that implies the guardian's composed SpiceDB
// schema is live — WaitForAgentClassValid takes that barrier, and every
// sending scenario currently goes through it. Sending earlier creates the
// AgentSession before `definition agentsession` exists, and channelsd's
// started_by write dies with Failed/AuthzWriteFailed. See
// Harness.WaitForAuthzSchema.
func (h *Harness) SendUserMessage(text string, opts ...SendOption) {
	h.t.Helper()
	cfg := sendCfg{user: h.opts.DefaultUser, thread: "default-thread"}
	for _, o := range opts {
		o(&cfg)
	}

	ch := h.singleChannel("SendUserMessage")
	// The fake listener registers its Driver lazily on Start (a goroutine), so
	// a Channel applied shortly before this send races the listener-starter.
	// Poll for the driver — mirroring ExpectAgentReply's poll — instead of a
	// one-shot check: a one-shot fataled spuriously under suite contention (the
	// "listener may not have started yet" flake). Only fatal if the listener
	// never registers within the timeout, which is a real failure (silently
	// dropping the inject would otherwise surface as a much-later
	// ExpectAgentReply timeout with no link back to the missing driver).
	var drv *fakekind.Driver
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	for {
		if drv = fakekind.DriverFor(ch.Namespace, ch.Name); drv != nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("SendUserMessage: no fake driver registered for %s/%s after %s — listener never started",
				ch.Namespace, ch.Name, h.opts.DefaultTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	drv.Inject(channelkinds.InboundEvent{
		Channel: ch,
		ExternalIDs: channelkinds.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
		ChannelKey:  cfg.thread,
		MessageText: text,
		Attachments: cfg.attachments,
	})
}

// ChannelMessage is the harness's view of an outbound envelope captured
// by the fake driver. Only Text is exposed today; structured fields
// (blocks, attachments) can be lifted from
// channelevents.OutboundUserMessagePayload when a scenario needs them.
type ChannelMessage struct {
	Text string
}

// ReplyPredicate matches a single ChannelMessage. Composed by passing
// multiple predicates to ExpectAgentReply (all must match).
type ReplyPredicate func(ChannelMessage) bool

// Contains matches when the message text contains every substring (AND).
// Empty substrs is a no-op match (returns true). The most common use is
// a single substring: e2e.Contains("pong").
func Contains(substrs ...string) ReplyPredicate {
	return func(m ChannelMessage) bool {
		for _, s := range substrs {
			if !strings.Contains(m.Text, s) {
				return false
			}
		}
		return true
	}
}

// NotContains matches when the message text contains NONE of the substrings.
// The negative of Contains: use it to assert something a person must NOT see in
// the reply — provenance delimiters, a leaked nonce, a raw envelope marker that
// should have been stripped before the text reached them. Empty substrs is a
// no-op match (returns true).
func NotContains(substrs ...string) ReplyPredicate {
	return func(m ChannelMessage) bool {
		for _, s := range substrs {
			if strings.Contains(m.Text, s) {
				return false
			}
		}
		return true
	}
}

// Matches matches when the message text matches the regexp. Nil regexp
// matches everything (cheaper than the caller threading a sentinel).
func Matches(re *regexp.Regexp) ReplyPredicate {
	return func(m ChannelMessage) bool {
		if re == nil {
			return true
		}
		return re.MatchString(m.Text)
	}
}

// ExactText matches when the message text is exactly s. Useful for
// "this reply MUST be the canned response and nothing else" assertions.
func ExactText(s string) ReplyPredicate {
	return func(m ChannelMessage) bool { return m.Text == s }
}

// ExpectAgentReply blocks until the fake channel records an outbound
// user_message envelope satisfying every predicate, or the harness's
// DefaultTimeout elapses. Returns the matched ChannelMessage so the
// caller can chain follow-up assertions on the actual text. On timeout,
// t.Fatalf is called with the number of outbounds seen — a non-zero
// count usually means "the agent replied, but the text didn't match the
// predicate," and a zero count usually means "the pipeline never reached
// respond_to_user" (LLM rule miss, runner panic, missing tool wiring).
func (h *Harness) ExpectAgentReply(preds ...ReplyPredicate) ChannelMessage {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectAgentReply")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			sent := drv.Sent()
			for i := seen; i < len(sent); i++ {
				m := ChannelMessage{Text: sent[i].Text}
				if matchesAll(m, preds) {
					return m
				}
			}
			seen = len(sent)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectAgentReply: timed out after %s (seen %d outbound message(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return ChannelMessage{}
}

// DeliveredArtifactIDs returns the artifact ID of every attachment the bound
// fake output channel actually received, in send order, with duplicates kept.
//
// Reads the transport's own record rather than the transcript: ExpectAgentReply
// projects an outbound envelope down to its Text and drops Attachments, so a
// reply that delivered nothing and one that delivered a report are
// indistinguishable through it. That is the blind spot this closes.
//
// A snapshot, not a wait. Callers assert it after the delivery they care about
// has already been observed — polling here would only mask an ordering bug by
// waiting for an attachment that was never going to arrive.
func (h *Harness) DeliveredArtifactIDs() []string {
	h.t.Helper()
	ch := h.singleChannel("DeliveredArtifactIDs")
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	if drv == nil {
		return nil
	}
	var out []string
	for _, sent := range drv.Sent() {
		for _, att := range sent.Attachments {
			out = append(out, att.ArtifactID)
		}
	}
	return out
}

// ExpectNotification blocks until the fake channel records a
// KindNotification envelope (a runner-side Notify(...) call — mid-turn
// status pings, the toolAuthDisabled warning, the post-session cost report)
// satisfying every predicate, or the harness's DefaultTimeout elapses.
// Returns the matched ChannelMessage. Distinct from ExpectAgentReply, which
// only observes KindUserMessage (respond_to_user) envelopes — Notify(...)
// text rides a different envelope kind and is captured separately by the
// fake driver's Notifications().
func (h *Harness) ExpectNotification(preds ...ReplyPredicate) ChannelMessage {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectNotification")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			notes := drv.Notifications()
			for i := seen; i < len(notes); i++ {
				m := ChannelMessage{Text: notes[i].Text}
				if matchesAll(m, preds) {
					return m
				}
			}
			seen = len(notes)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectNotification: timed out after %s (seen %d notification(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return ChannelMessage{}
}

func matchesAll(m ChannelMessage, preds []ReplyPredicate) bool {
	for _, p := range preds {
		if !p(m) {
			return false
		}
	}
	return true
}

// singleChannel returns the (one) CONVERSATIONAL Channel CR in the harness
// namespace, or fatals with a clear message naming the calling site if none
// exists. T9's conversation API assumes one channel per test; richer
// multi-channel scenarios will need explicit channel selectors.
//
// role=monitoring Channels are skipped rather than counted. A monitoring
// Channel is a framework-event SINK, not a conversation surface: it carries no
// AgentClass (the Channel controller skips those reference checks for it
// entirely), no user message can arrive on it, and no agent reply is addressed
// to it. Counting one would make "does this cluster have an admin surface
// configured" — which several scenarios must answer YES to in order to have a
// recipient for an agent-owned credential card at all — collide with "which
// channel is this test talking on", two unrelated questions.
//
// Agent-to-agent Channels are skipped for the same reason, and it is a
// different one from "there are now two surfaces": a conversational delegation
// gets its own Channel joining the parent to its child
// (pkg/controllers/subagentrequest's buildAgentChannel), and no human is on
// either end of it. A user cannot send on it and no reply a test waits for is
// addressed to it, so counting it turned "the delegation is conversing" into
// "this test is ambiguous about which channel it talks on".
//
// The loud multi-channel guard below is preserved for the ambiguity it was
// written for: two channels a user could actually be talking on.
func (h *Harness) singleChannel(caller string) *spiceboxv1alpha1.Channel {
	h.t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	if err := h.K8s.List(context.Background(), &channels); err != nil {
		h.t.Fatalf("%s: list Channel CRs: %v", caller, err)
	}
	conversational := make([]*spiceboxv1alpha1.Channel, 0, len(channels.Items))
	for i := range channels.Items {
		if channels.Items[i].Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
			continue
		}
		if h.reachesAnotherSession(caller, channels.Items[i].Spec.Kind) {
			continue
		}
		conversational = append(conversational, &channels.Items[i])
	}
	if len(conversational) == 0 {
		h.t.Fatalf("%s: no conversational Channel CR found (%d monitoring-only) — did the AgentDir apply complete?",
			caller, len(channels.Items))
	}
	// Defensive: tests with multiple Channels should be opting out of
	// this helper rather than getting an arbitrary pick. Fail loudly so
	// the test author updates the API to pass a selector.
	if len(conversational) > 1 {
		names := make([]string, 0, len(conversational))
		for _, ch := range conversational {
			names = append(names, ch.Name)
		}
		h.t.Fatalf("%s: multiple conversational Channel CRs found (%v); T9 assumes one channel per test",
			caller, names)
	}
	return conversational[0]
}

// singleSession returns the (ns, name) of the (one) channel-attached
// AgentSession CR in the harness namespace, or fatals naming the calling
// site. Mirrors singleChannel: tool-session helpers that infer the target
// assume one active TOP-LEVEL session per test — the one the bundle's
// userTurns/agentReplyContains transcript is actually about — not one CR
// total; multi-session scenarios beyond delegation will need explicit
// selectors.
//
// Filtered to sessions bound to a HUMAN surface before the uniqueness check,
// because a delegated child (pkg/controllers/subagentrequest's buildChild) is
// a SECOND, real AgentSession CR in the same namespace and there are three
// shapes of it:
//
//   - single_turn: headless, no InputChannel at all, so it never had a
//     conversation surface to be confused with — excluded by the InputChannel
//     nil check below, independent of delegation.
//   - task/chat: bound to the `agent` Channel joining it to its parent, which
//     IS an InputChannel — so "channel-attached" alone stopped separating the
//     two the moment conversational delegation existed.
//   - attended: copies the ROOT's own outbound binding verbatim
//     (attendedChildBinding) so the person keeps talking on the surface they
//     started on. Its Kind and Key are therefore IDENTICAL to the root's own
//     binding, not the `agent` kind — a kind-based test cannot tell it apart
//     from a second, genuinely independent human-facing session.
//
// All three are excluded by asking whether the CR is a delegated child at
// all — Spec.Parent != nil, which the SubagentRequest controller stamps on
// every child it creates (buildChild) independent of mode or channel kind —
// rather than inferring it from the binding's kind. This used to ask the
// kind's own registry (chregistry.AllowsSessionCounterparty), which correctly
// caught task/chat's `agent` kind but had no way to catch attended, since
// attended deliberately reaches a human, not another session; asking the kind
// was a proxy for "is this a delegated child" that broke once a delegation
// mode existed whose binding does not name a session-to-session kind. Without
// this, any bundle whose transcript delegates would trip the ">1" guard below
// on the child it legitimately created, even though nothing about the child is
// what SessionRef/checkGoldenTrace/checkAuthz care about — they want the
// top-level session's own transcript and audit logs. A bundle with no
// delegation still resolves to exactly the one session it always did, and a
// genuine SECOND top-level session (no Parent, its own independent binding)
// still trips the ">1" guard, because it is exactly the ambiguity that guard
// exists to catch.
func (h *Harness) singleSession(caller string) (ns, name string) {
	h.t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(context.Background(), &sessions); err != nil {
		h.t.Fatalf("%s: list AgentSession CRs: %v", caller, err)
	}
	attached := make([]spiceboxv1alpha1.AgentSession, 0, len(sessions.Items))
	for i := range sessions.Items {
		b := sessions.Items[i].Spec.InputChannel
		if b == nil {
			continue
		}
		if _, delegated := sessions.Items[i].ParentRef(); delegated {
			continue
		}
		attached = append(attached, sessions.Items[i])
	}
	if len(attached) == 0 {
		h.t.Fatalf("%s: no channel-attached AgentSession CR found — has the pipeline created a session yet?", caller)
	}
	if len(attached) > 1 {
		names := make([]string, 0, len(attached))
		for i := range attached {
			names = append(names, attached[i].Namespace+"/"+attached[i].Name)
		}
		h.t.Fatalf("%s: multiple channel-attached AgentSession CRs found (%v); helper assumes one top-level session per test",
			caller, names)
	}
	return attached[0].Namespace, attached[0].Name
}

// reachesAnotherSession reports whether a Channel of this kind carries a
// conversation between two AgentSessions rather than between an agent and a
// person — the `agent` kind a conversational delegation is given.
//
// It asks the KIND through the registry, never a kind name, so a second
// session-to-session transport is excluded from singleChannel with no edit
// here (AGENTS.md: registry, not branching). singleSession no longer calls
// this — it excludes a delegated child directly via ParentRef(), see its
// own doc for why a kind-based check stopped being sufficient there. This
// is the same question pkg/agent/tool/meta/capability asks when deciding
// whether such a session may be offered respond_to_user, so the harness and
// the runner cannot disagree about what "reaches another agent" means.
//
// An unregistered kind answers NO — count it — rather than being skipped on a
// guess: skipping is the direction that could hide a genuinely ambiguous test
// by discarding a surface it should have tripped over. The loud multi-channel
// guard then reports the ambiguity, and the lookup failure is logged rather
// than swallowed so the wiring bug behind it is visible in the test output.
func (h *Harness) reachesAnotherSession(caller, kind string) bool {
	h.t.Helper()
	if kind == "" {
		return false
	}
	toASession, err := chregistry.AllowsSessionCounterparty(kind)
	if err != nil {
		h.t.Logf("%s: channel kind %q is not registered in this binary, so it is counted as a human surface: %v",
			caller, kind, err)
		return false
	}
	return toASession
}

// NOTE: there is deliberately NO SeedSessionCoOwner-style helper here. An
// earlier revision seeded the gated resource's owner as an agentsession#owner
// so the (since-removed) `session owners ∩ resource owners` approver
// intersection had a member — a tuple NO production code path writes, which
// let the e2e suite stay green while the production owner-approval flow was
// structurally unapprovable ("no one has standing", 2026-07-02). Approval
// scenarios must run the production shape: the requester is the sole session
// owner and the approver's standing comes from resource ownership alone. If a
// scenario seems to need a session co-owner to have a valid approver, the
// authz model regressed — fix it there (see authz.ResolveApprovers), not here.

// SessionRef returns the (namespace, name) of the one AgentSession in the
// harness namespace.
//
// Exported (rather than living in export_test.go) because sibling test
// packages — bronzethread's driver — need it, and an export_test.go shim is
// only visible to this package's own tests. The whole package is build-tagged
// e2e, so this is not production surface.
func (h *Harness) SessionRef() (ns, name string) {
	return h.singleSession("SessionRef")
}

// AgentClassNames lists every AgentClass currently in the cluster.
//
// For the driver's "wait for the OTHER classes too" pass: a fixture that
// declares a delegation target has more than one class, and only the one the
// bundle starts gets an explicit wait.
func (h *Harness) AgentClassNames() []string {
	h.t.Helper()
	var list spiceboxv1alpha1.AgentClassList
	if err := h.K8s.List(context.Background(), &list); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names
}
