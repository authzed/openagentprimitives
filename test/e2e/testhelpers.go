//go:build e2e

// Shared assertion/polling helpers for e2e tests.
//
// These live in a NON-test file on purpose. The e2e tests are split across
// sibling packages (test/e2e/scenarios/*) so `go test -p` can run them
// concurrently — one serial package was 94% of the tier's wall time — and a
// sibling package cannot import identifiers defined in a _test.go. Anything
// more than one test package needs therefore has to live here.
//
// PollSession is exported because sibling packages build their own
// waiters on top of it (waitForSessionFailed, waitForEffectiveMode, ...).

package e2e

import (
	"context"
	"strconv"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/nats-io/nats.go"
)

// E2EHarnessSource identifies helpers in this package that write their own
// fixture tuples directly (WriteRel and friends) — standing in for no
// production writer, unlike every OTHER relsource.Source this in-process
// harness uses: those are declared exactly once in the library package that
// owns the write they mirror (spicedb.BootstrapSource, relwrites.Source,
// guardian/grants.Source, guardian/approval.LeakageSource,
// pttagmint.Source, channelsd/pipeline.GrantSource) and referenced here and
// at their production wiring site, never retyped.
//
// Claims deliberately EMPTY. WriteRel takes a caller-supplied
// resourceType/relation, used across scenarios to seed whatever fixture a
// test needs (including, deliberately, standing in for an out-of-tree
// connector — see leakage_schema_test.go) — an unenumerable set, same shape
// as relwrites/spicedbbootstrap. Leaving it unclaimed also means a fixture
// that tries to seed a relation a REAL source now owns (e.g. pt_tag#session,
// or one of the slack sync claims) is refused instead of silently
// bypassing the writer whose behavior it is supposed to be exercising.
var E2EHarnessSource = relsource.Source{Name: "e2eharness"}

func init() {
	relsource.Register(E2EHarnessSource)
}

// Eventually polls fn every 100ms until it returns true or the timeout
// elapses; fatals with msg on timeout. Used for controller-cache
// convergence assertions.
func Eventually(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("eventually: %s (timed out after %s)", msg, timeout)
}

// FindOnlyAgentSession returns the single, non-delegated AgentSession in ns,
// waiting up to 10s for exactly one to exist. Fatals if none or several
// appear.
//
// Filtered the same way singleSession is (conversation.go): a delegated
// child — including an ATTENDED one, which copies the root's own channel
// binding verbatim and so is indistinguishable from a second top-level
// session by kind or key — carries Spec.Parent, stamped by the
// SubagentRequest controller on every child it creates regardless of mode
// (pkg/controllers/subagentrequest's buildChild). Excluding it keeps this
// helper answering the question its callers actually want: is there exactly
// one TOP-LEVEL session, not exactly one AgentSession CR in the namespace. A
// genuine second top-level session (no Parent) is NOT filtered out and still
// trips the "many" fatal below — that ambiguity is what this helper exists
// to catch.
func FindOnlyAgentSession(t *testing.T, h *Harness, ns string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list, client.InNamespace(ns)); err == nil {
			topLevel := make([]spiceboxv1alpha1.AgentSession, 0, len(list.Items))
			for i := range list.Items {
				if _, delegated := list.Items[i].ParentRef(); delegated {
					continue
				}
				topLevel = append(topLevel, list.Items[i])
			}
			if len(topLevel) == 1 {
				return &topLevel[0]
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("FindOnlyAgentSession(%s): no/many sessions within deadline", ns)
	return nil
}

// CanonicalForFakeEmail returns the SpiceDB-safe canonical id the
// pipeline uses for an email-typed AsUser. Matches identity.Principal.Canonical():
// base64(lower(email)). The raw email contains '@' which SpiceDB rejects
// for object_id; the canonical encoding sidesteps that.
func CanonicalForFakeEmail(email string) identity.CanonicalUserID {
	c, err := identity.FromExternal("fake", "", identity.RawExternalID(email), identity.Email(email)).Canonical()
	if err != nil {
		panic(err)
	}
	return c
}

// WriteRel writes a single SpiceDB relationship via the harness's
// SpiceDB client. Used to seed approver-validation chains in tests.
// subjectRelation is "" for a direct subject reference.
func WriteRel(t *testing.T, h *Harness, resourceType, resourceID, relation, subjectType, subjectID, subjectRelation string) {
	t.Helper()
	req := &spicedbv1.WriteRelationshipsRequest{
		Updates: []*spicedbv1.RelationshipUpdate{
			{
				Operation: spicedbv1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &spicedbv1.Relationship{
					Resource: &spicedbv1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
					Relation: relation,
					Subject: &spicedbv1.SubjectReference{
						Object:           &spicedbv1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID},
						OptionalRelation: subjectRelation,
					},
				},
			},
		},
	}
	_, err := h.SpiceDB.Writer(E2EHarnessSource).WriteRelationships(context.Background(), req)
	require.NoError(t, err, "WriteRel %s:%s#%s@%s:%s", resourceType, resourceID, relation, subjectType, subjectID)
}

// FirstText returns the first non-empty text block's text, or "".
func FirstText(blocks []pkgmemory.ContentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// DumpTurns renders a turn list for a failure message.
func DumpTurns(turns []pkgmemory.Turn) string {
	s := ""
	for _, tn := range turns {
		s += "\n  [" + strconv.Itoa(tn.Index) + "] " + tn.Role + ": " + FirstText(tn.Content)
	}
	return s
}

// WaitForSessionIdle waits for the namespace's single AgentSession to reach
// the Idle phase.
func WaitForSessionIdle(t *testing.T, h *Harness) {
	t.Helper()
	ns := h.opts.Namespace
	sess := FindOnlyAgentSession(t, h, ns)
	Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle
	}, "session reaches Idle")
}

// WaitForAnySession waits for at least one AgentSession to exist anywhere and
// returns its namespace/name.
func WaitForAnySession(t *testing.T, ctx context.Context, h *Harness) (string, string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(ctx, &list); err != nil {
			t.Fatalf("list AgentSessions: %v", err)
		}
		if len(list.Items) >= 1 {
			s := &list.Items[0]
			return s.Namespace, s.Name
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WaitForAnySession: ctx done: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("WaitForAnySession: no AgentSession within 30s")
	return "", ""
}

// WaitForSessionPhase waits for the named session to report the given phase.
func WaitForSessionPhase(t *testing.T, h *Harness, ns, name, phase string) {
	t.Helper()
	PollSession(t, h, ns, name, "phase="+phase, func(s *spiceboxv1alpha1.AgentSession) bool {
		return s.Status.Phase == phase
	})
}

// PollSession polls the session until pred is satisfied, failing with what and
// the harness's state dump on timeout.
//
// It deliberately does NOT poke the AgentSession to force a reconcile. Every
// transition these waiters observe is produced by a trigger production also
// has — the session's own status writes, a watched dependency, a RequeueAfter,
// or the runner Pod's lifecycle. A poke here would substitute a test-side
// write for the very trigger under test; it did, and it hid a missing
// ownerReference on the harness's placeholder runner Pod for the whole
// identity tier (see markRunnerPodExited in inprocess_runner_factory.go).
func PollSession(t *testing.T, h *Harness, ns, name, what string, pred func(*spiceboxv1alpha1.AgentSession) bool) {
	t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			if pred(&sess) {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("PollSession: %s/%s never satisfied %q within %s\n%s",
		ns, name, what, h.opts.DefaultTimeout, h.dumpState())
}

// --- Exported Harness accessors for sibling test packages ---
//
// The e2e tests live in sibling packages (test/e2e/scenarios/*) so the tier can
// run them in parallel, and a sibling package cannot reach an unexported field.
// These expose exactly what the tests already used when they lived inside this
// package — nothing more, so the harness's internals stay free to change.

// Namespace is the namespace this harness applies manifests into.
func (h *Harness) Namespace() string { return h.opts.Namespace }

// DefaultTimeout is the harness's default Expect*/poll deadline.
func (h *Harness) DefaultTimeout() time.Duration { return h.opts.DefaultTimeout }

// NATS is the harness's in-process NATS connection.
func (h *Harness) NATS() *nats.Conn { return h.nc }

// RunnerFactory is the in-process runner factory backing this harness.
func (h *Harness) RunnerFactory() *InProcessRunnerFactory { return h.runnerFactory }

// SlackSource is the fake Slack socket source backing kind: slack Channels.
func (h *Harness) SlackSource() *fakeslack.SocketSource { return h.slackSource }

// SingleChannel returns the one Channel in the harness namespace, failing the
// harness's test if there is not exactly one. caller names the calling helper
// in that failure.
func (h *Harness) SingleChannel(caller string) *spiceboxv1alpha1.Channel {
	return h.singleChannel(caller)
}

// SingleSession returns the namespace/name of the one AgentSession, failing the
// harness's test if there is not exactly one.
func (h *Harness) SingleSession(caller string) (ns, name string) {
	return h.singleSession(caller)
}

// DumpState renders the harness's human-readable diagnostic snapshot.
func (h *Harness) DumpState() string { return h.dumpState() }
