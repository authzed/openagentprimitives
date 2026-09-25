//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ToolCall is the harness's projection of one MCP tool invocation
// captured by the MCPStub. Mirrors MCPStub.ToolCallRecord but lives
// in the assertions API so test files don't reach into the stub's
// internals.
type ToolCall struct {
	Name string
	Args map[string]any
}

// ToolPredicate filters a tool call. ExpectToolCall composes multiple
// predicates with AND semantics, same as ExpectAgentReply's
// ReplyPredicate.
type ToolPredicate func(ToolCall) bool

// ArgsEqual matches when the call's args deep-equal want. Uses Go's
// canonical fmt.Sprintf("%v") form for the comparison so map-key order
// doesn't matter — Go prints maps sorted by key. For numeric / time
// values the formatted-string compare may be brittle; ArgContains is
// the right tool for "this arg has this substring."
func ArgsEqual(want map[string]any) ToolPredicate {
	return func(c ToolCall) bool {
		return fmt.Sprintf("%v", c.Args) == fmt.Sprintf("%v", want)
	}
}

// ArgContains matches when args[key]'s stringified form contains s.
// Useful for asserting on free-text arguments (e.g. a "query" field)
// without locking the test to the exact LLM phrasing.
func ArgContains(key, s string) ToolPredicate {
	return func(c ToolCall) bool {
		v, ok := c.Args[key]
		if !ok {
			return false
		}
		return strings.Contains(fmt.Sprintf("%v", v), s)
	}
}

// ExpectToolCall blocks until the MCPStub records a tool call matching
// every predicate, or DefaultTimeout elapses. Returns the matched call
// so the test can chain follow-up assertions on its args. Tracks a
// running watermark so re-invocations of ExpectToolCall in the same
// test pick up the next matching call rather than re-matching prior ones.
//
// On timeout, calls dumpState() so the failure message shows what the
// MCP stub did see, the LLM script's served requests, and any
// AgentSession status that might explain why the runner never reached
// this tool dispatch.
func (h *Harness) ExpectToolCall(name string, preds ...ToolPredicate) ToolCall {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		calls := h.MCP.Calls()
		for i := seen; i < len(calls); i++ {
			if calls[i].Name != name {
				continue
			}
			c := ToolCall{Name: calls[i].Name, Args: calls[i].Args}
			ok := true
			for _, p := range preds {
				if !p(c) {
					ok = false
					break
				}
			}
			if ok {
				return c
			}
		}
		seen = len(calls)
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("ExpectToolCall(%q): timed out after %s\n%s",
		name, h.opts.DefaultTimeout, h.dumpState())
	return ToolCall{}
}

// AssertSpiceDB issues a fully-consistent CheckPermission against the
// harness's SpiceDB and fails the test if the result doesn't match
// `want`. resourceRef is a "<type>:<id>" string (e.g. "crm_company:acme-1");
// subjectRef is a typed SpiceDB subject (e.g. "user:alice@example.com").
// Useful after Approval.Approve() to verify the per-(session, tool, args)
// grant tuple landed before issuing a follow-up SendUserMessage.
func (h *Harness) AssertSpiceDB(resourceRef, permission string, subjectRef identity.Subject, want bool) {
	h.t.Helper()
	h.AssertSpiceDBRaw(resourceRef, permission, subjectRef.String(), want)
}

// AssertSpiceDBRaw is the string-subject form of AssertSpiceDB, for callers
// that already hold the "<type>:<id>" spelling (a bronze bundle's assert.spicedb
// names the escaped/canonicalized object id directly, the same way its
// SpiceDBBootstrap grant does — building an identity.Subject would only
// re-derive a string the author already wrote). AssertSpiceDB delegates here
// after stringifying its typed subject.
func (h *Harness) AssertSpiceDBRaw(resourceRef, permission, subjectRef string, want bool) {
	h.t.Helper()
	rt, rid, ok := strings.Cut(resourceRef, ":")
	if !ok {
		h.t.Fatalf("AssertSpiceDB: bad resourceRef %q (want <type>:<id>)", resourceRef)
	}
	st, sid, ok := strings.Cut(subjectRef, ":")
	if !ok {
		h.t.Fatalf("AssertSpiceDB: bad subjectRef %q (want <type>:<id>)", subjectRef)
	}
	resp, err := h.SpiceDB.CheckPermission(context.Background(), &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: rt, ObjectId: rid},
		Permission: permission,
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{ObjectType: st, ObjectId: sid},
		},
		Consistency: &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		},
	})
	if err != nil {
		h.t.Fatalf("AssertSpiceDB: CheckPermission %s#%s@%s: %v",
			resourceRef, permission, subjectRef, err)
	}
	got := resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	if got != want {
		h.t.Fatalf("AssertSpiceDB: %s#%s@%s: got=%v want=%v",
			resourceRef, permission, subjectRef, got, want)
	}
}

// AssertAllRulesConsumed delegates to the scripted LLM — every
// registered non-Repeating rule must have matched at least once.
// Exposed on the harness so tests can write the more discoverable
// name rather than `h.LLM.AssertAllRulesConsumed()`.
func (h *Harness) AssertAllRulesConsumed() {
	h.t.Helper()
	h.LLM.AssertAllRulesConsumed()
}

// AssertNoStrandedInbox asserts the no-strand invariant on every AgentSession
// in the namespace: an undrained "inbox" turn may NOT coexist with a parked
// session that has no unconsumed wake request.
//
//	stranded  ⇔  heldInbox ∧ WakeEligible ∧ ¬WakePending
//
// That triple is precisely "the user's message is durably accepted, the runner
// pod is gone, and nothing will ever start one" — the silent-strand state. Each
// leg is individually fine: a held turn under a live runner is just a queued
// message, and a held turn with a pending wake is a respawn already in flight.
//
// It is a property, not a timing assertion, which is the point — the race that
// motivated it (channelsd deciding the respawn from a phase snapshot taken
// before the runner idled) is one way to reach the state, and this check
// outlives that particular interleaving.
//
// Call it after the conversation has settled. It is a plain read; no waiting.
func (h *Harness) AssertNoStrandedInbox() {
	h.t.Helper()
	ctx := context.Background()
	ns := h.opts.Namespace

	var list spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
		h.t.Fatalf("AssertNoStrandedInbox: list sessions in %s: %v", ns, err)
	}
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-no-strand-invariant")
	for i := range list.Items {
		sess := &list.Items[i]
		if !spiceboxv1alpha1.WakeEligible(sess) || spiceboxv1alpha1.WakePending(sess) {
			continue
		}
		held := h.heldInboxTexts(readCtx, ns, sess.Name)
		if len(held) > 0 {
			h.t.Fatalf("AssertNoStrandedInbox: %s/%s is parked at phase=%q with no pending wake, "+
				"but holds %d undrained inbox turn(s) %q — the user's message was accepted and will never run\n%s",
				ns, sess.Name, sess.Status.Phase, len(held), held, h.dumpState())
		}
	}
}

// heldInboxTexts returns the text of every "inbox" turn with no paired
// "inbox_done" marker — the runner's own heldInbox predicate, read directly
// from the shared memory facade.
func (h *Harness) heldInboxTexts(ctx context.Context, ns, name string) []string {
	h.t.Helper()
	turns, err := turn.NewAppender(h.MemStore(), pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}).ReadAll(ctx)
	if err != nil {
		h.t.Fatalf("AssertNoStrandedInbox: read memory for %s/%s: %v", ns, name, err)
	}
	drained := map[int]bool{}
	for _, tn := range turns {
		if tn.Role == "inbox_done" {
			drained[tn.Index] = true
		}
	}
	var held []string
	for _, tn := range turns {
		if tn.Role != "inbox" || drained[tn.Index] {
			continue
		}
		text := ""
		for _, b := range tn.Content {
			if b.Type == "text" && b.Text != "" {
				text = b.Text
				break
			}
		}
		held = append(held, text)
	}
	return held
}
