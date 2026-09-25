package lifecycle_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	kind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// eventRoundTripCases is package-level (not local to TestEventRoundTrip) so
// TestEventTableIsExhaustive can check it for completeness against
// pkg/agent/session/lifecycle/event.go's actual declared types, in addition
// to TestEventRoundTrip exercising each row's Marshal/Unmarshal round-trip.
var eventRoundTripCases = []struct {
	name  string
	event lc.Event
	check func(t *testing.T, got lc.Event)
}{
	{
		name:  "SettingsAccepted round-trips",
		event: lc.SettingsAccepted{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.SettingsAccepted)
			require.True(t, ok, "expected SettingsAccepted")
		},
	},
	{
		name:  "PodReady round-trips",
		event: lc.PodReady{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.PodReady)
			require.True(t, ok)
		},
	},
	{
		name:  "Unschedulable round-trips",
		event: lc.Unschedulable{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.Unschedulable)
			require.True(t, ok)
		},
	},
	{
		name:  "ProvablyUnschedulable round-trips",
		event: lc.ProvablyUnschedulable{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.ProvablyUnschedulable)
			require.True(t, ok)
		},
	},
	{
		// The Message is the point of the event — it is the only durable copy
		// of the refusal's own words — so this asserts the payload, not just
		// the type.
		name:  "RunnerPodRefused round-trips with its message",
		event: lc.RunnerPodRefused{Message: "failed quota: must specify limits.cpu"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.RunnerPodRefused)
			require.True(t, ok)
			require.Equal(t, "failed quota: must specify limits.cpu", ev.Message)
		},
	},
	{
		name:  "CredsMissing round-trips",
		event: lc.CredsMissing{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.CredsMissing)
			require.True(t, ok)
		},
	},
	{
		name:  "CredsLinked round-trips",
		event: lc.CredsLinked{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.CredsLinked)
			require.True(t, ok)
		},
	},
	{
		name:  "CredsTimeout round-trips",
		event: lc.CredsTimeout{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.CredsTimeout)
			require.True(t, ok)
		},
	},
	{
		name:  "IdentityChoicePending round-trips",
		event: lc.IdentityChoicePending{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.IdentityChoicePending)
			require.True(t, ok)
		},
	},
	{
		// Mode AND ConfirmedBy (the audit "who chose" fact) must both survive
		// the round-trip — a dropped codec field here silently loses the
		// signed record of who confirmed the identity.
		name:  "IdentityChoiceResolved round-trips with mode and confirmer",
		event: lc.IdentityChoiceResolved{Mode: "userPassthrough", ConfirmedBy: "U-clicker"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.IdentityChoiceResolved)
			require.True(t, ok)
			assert.Equal(t, "userPassthrough", ev.Mode)
			assert.Equal(t, "U-clicker", ev.ConfirmedBy, "ConfirmedBy must survive round-trip")
		},
	},
	{
		name:  "IdentityChoiceCancelled round-trips with canceller",
		event: lc.IdentityChoiceCancelled{CancelledBy: "U-clicker"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.IdentityChoiceCancelled)
			require.True(t, ok)
			assert.Equal(t, "U-clicker", ev.CancelledBy, "CancelledBy must survive round-trip")
		},
	},
	{
		name:  "IdentityChoiceTimeout round-trips",
		event: lc.IdentityChoiceTimeout{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.IdentityChoiceTimeout)
			require.True(t, ok)
		},
	},
	{
		name:  "RunnerClaimed round-trips",
		event: lc.RunnerClaimed{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.RunnerClaimed)
			require.True(t, ok)
		},
	},
	{
		name: "RunnerTerminal round-trips with payload",
		event: lc.RunnerTerminal{
			Phase: lc.PhaseFailed, Reason: "oom",
			Message: "the container ran out of memory",
		},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.RunnerTerminal)
			require.True(t, ok)
			assert.Equal(t, lc.PhaseFailed, ev.Phase)
			assert.Equal(t, "oom", ev.Reason)
			// Message is what a reader holding only the log learns the failure
			// from, so it must survive the round trip, not just the Reason.
			assert.Equal(t, "the container ran out of memory", ev.Message)
		},
	},
	{
		name:  "RunnerCrash round-trips",
		event: lc.RunnerCrash{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.RunnerCrash)
			require.True(t, ok)
		},
	},
	{
		name:  "Stopped round-trips",
		event: lc.Stopped{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.Stopped)
			require.True(t, ok)
		},
	},
	{
		name:  "TurnCompleted round-trips",
		event: lc.TurnCompleted{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.TurnCompleted)
			require.True(t, ok)
		},
	},
	{
		name:  "AgentWorkComplete round-trips with kubectl=true",
		event: lc.AgentWorkComplete{Kubectl: true},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.AgentWorkComplete)
			require.True(t, ok)
			assert.True(t, ev.Kubectl)
		},
	},
	{
		name:  "HookDeny round-trips with post=true",
		event: lc.HookDeny{Post: true},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.HookDeny)
			require.True(t, ok)
			assert.True(t, ev.Post)
		},
	},
	{
		name:  "HookHalt round-trips with reason",
		event: lc.HookHalt{Reason: "ToolGuardHalt"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.HookHalt)
			require.True(t, ok)
			assert.Equal(t, "ToolGuardHalt", ev.Reason)
		},
	},
	{
		// DecisionAsked carries RequestID and Kind; verify both survive the round-trip.
		name:  "DecisionAsked round-trips: RequestID and Kind preserved",
		event: lc.DecisionAsked{RequestID: "r1", Kind: lc.DecisionToolCall},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.DecisionAsked)
			require.True(t, ok, "decodes to the concrete event type")
			assert.Equal(t, "r1", ev.RequestID)
			assert.Equal(t, lc.DecisionToolCall, ev.Kind)
		},
	},
	{
		name:  "DecisionResolved round-trips with timeout=true",
		event: lc.DecisionResolved{RequestID: "r2", Approved: false, TimedOut: true},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.DecisionResolved)
			require.True(t, ok)
			assert.Equal(t, "r2", ev.RequestID)
			assert.False(t, ev.Approved)
			assert.True(t, ev.TimedOut)
		},
	},
	{
		name:  "ProviderError round-trips",
		event: lc.ProviderError{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.ProviderError)
			require.True(t, ok)
		},
	},
	{
		name:  "RetryRequested round-trips",
		event: lc.RetryRequested{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.RetryRequested)
			require.True(t, ok)
		},
	},
	{
		name:  "RetryTTLExpired round-trips",
		event: lc.RetryTTLExpired{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.RetryTTLExpired)
			require.True(t, ok)
		},
	},
	{
		name:  "WakeRequested round-trips",
		event: lc.WakeRequested{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.WakeRequested)
			require.True(t, ok)
		},
	},
	{
		name:  "ArchiveSweep round-trips",
		event: lc.ArchiveSweep{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.ArchiveSweep)
			require.True(t, ok)
		},
	},
	{
		// Expired had no row here (and no typeNameOf/decodeEvent case) from
		// the day lc.Expired was added: every session-expiration write from
		// pkg/controllers/agentsession/expiration.go's
		// applyEvent(..., lifecyclecore.Expired{}) failed with "unknown event
		// type lifecycle.Expired", requeued, and retried forever. Found by
		// TestEventTableIsExhaustive below while adding it for Task 13's
		// Held/Released follow-up -- the exact class of gap that test exists
		// to catch, caught before this row was even written.
		name:  "Expired round-trips",
		event: lc.Expired{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.Expired)
			require.True(t, ok)
		},
	},
	{
		name:  "Sleep round-trips",
		event: lc.Sleep{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.Sleep)
			require.True(t, ok)
		},
	},
	{
		name:  "AwaitYieldEntered round-trips",
		event: lc.AwaitYieldEntered{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.AwaitYieldEntered)
			require.True(t, ok)
		},
	},
	{
		name:  "AwaitResumed round-trips",
		event: lc.AwaitResumed{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.AwaitResumed)
			require.True(t, ok)
		},
	},
	{
		name:  "IdleYield round-trips",
		event: lc.IdleYield{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.IdleYield)
			require.True(t, ok)
		},
	},
	{
		name:  "ShareDeniedYield round-trips",
		event: lc.ShareDeniedYield{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.ShareDeniedYield)
			require.True(t, ok)
		},
	},
	{
		name:  "Revoked round-trips",
		event: lc.Revoked{Kind: "tool-origin", Key: "mcpserver/example"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			r, ok := got.(lc.Revoked)
			require.True(t, ok)
			assert.Equal(t, "tool-origin", r.Kind, "Revoked.Kind must survive round-trip")
			assert.Equal(t, "mcpserver/example", r.Key, "Revoked.Key must survive round-trip")
		},
	},
	{
		name:  "ScopeMutated round-trips",
		event: lc.ScopeMutated{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.ScopeMutated)
			require.True(t, ok)
		},
	},
	{
		name:  "RestartRequested round-trips",
		event: lc.RestartRequested{},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			_, ok := got.(lc.RestartRequested)
			require.True(t, ok)
		},
	},
	// Held and Released were absent from this table (and from
	// typeNameOf/decodeEvent) from the day pkg/agent/session/lifecycle
	// gained them: MarshalWithOrder returned "unknown event type
	// lifecycle.Held" for every append, which pkg/controllers/agentsession's
	// reconcileHold surfaced as an error from applyEvent, requeued, and
	// retried forever — the SessionHold CR tripped and stamped
	// TrippedAt/SnapshotHandle, but AgentSession.status.phase never
	// actually reached Held. Neither this package's own unit tests (no
	// case existed to catch the gap) nor pkg/agent/session/lifecycle's
	// (deliberately memory-free per AGENTS.md's "Lifecycle purity", so it
	// cannot reach this codec at all) could see it; only an end-to-end run
	// through the real signed-log write path did. Found and fixed while
	// building the Task 13 e2e coverage for the forensic-hold feature.
	{
		name:  "Held round-trips with reason, trippedBy and at",
		event: lc.Held{Reason: "3 consecutive plan-gate denials in phase 0", TrippedBy: "tripper/plangate-denial-streak", At: "2026-08-24T00:00:00Z"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.Held)
			require.True(t, ok)
			assert.Equal(t, "3 consecutive plan-gate denials in phase 0", ev.Reason)
			assert.Equal(t, "tripper/plangate-denial-streak", ev.TrippedBy, "TrippedBy must survive round-trip")
			assert.Equal(t, "2026-08-24T00:00:00Z", ev.At)
		},
	},
	{
		name:  "Released round-trips with approvedBy and at",
		event: lc.Released{ApprovedBy: "user:owner@example.com", At: "2026-08-24T01:00:00Z"},
		check: func(t *testing.T, got lc.Event) {
			t.Helper()
			ev, ok := got.(lc.Released)
			require.True(t, ok)
			assert.Equal(t, "user:owner@example.com", ev.ApprovedBy, "ApprovedBy must survive round-trip")
			assert.Equal(t, "2026-08-24T01:00:00Z", ev.At)
		},
	},
}

// TestEventRoundTrip verifies that every concrete Event type survives a
// Marshal → Unmarshal round-trip with its payload fields intact.
func TestEventRoundTrip(t *testing.T) {
	for _, tc := range eventRoundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := kind.Marshal(tc.event)
			require.NoError(t, err)
			out, err := kind.Unmarshal(b)
			require.NoError(t, err)
			tc.check(t, out)
		})
	}
}

// TestEventTableIsExhaustive guards eventRoundTripCases against going stale
// the way it already had for Held, Released, and Expired (see the comments
// on those cases above): it parses pkg/agent/session/lifecycle/event.go
// directly and derives the authoritative set of lifecycle.Event implementers
// -- every exported struct type embedding baseEvent, the unexported field
// whose isEvent() method is what makes Event a sealed interface only
// implementable from within that package (see event.go's Event doc) -- then
// requires eventRoundTripCases to name exactly that set, no more and no
// fewer. A newly added Event type with no row here fails this test
// immediately, instead of surfacing as an "unknown event type" write failure
// discovered only through a real end-to-end run, as happened for all three
// cases above.
//
// Go has no runtime reflection over "every type declared in package X" --
// only over values already in hand -- so this reaches for go/ast against the
// one file that declares the sealed union, the same technique
// pkg/agent/session/lifecycle/imports_test.go's TestNoIODeps already uses to
// check its own package's source from within a _test.go file.
func TestEventTableIsExhaustive(t *testing.T) {
	implementers := eventImplementersInSource(t)
	require.NotEmpty(t, implementers, "sanity: event.go must declare at least one lifecycle.Event implementer")

	tableNames := make([]string, 0, len(eventRoundTripCases))
	for _, tc := range eventRoundTripCases {
		tableNames = append(tableNames, strings.TrimPrefix(fmt.Sprintf("%T", tc.event), "lifecycle."))
	}

	assert.ElementsMatch(t, implementers, tableNames,
		"eventRoundTripCases must have exactly one row per exported lifecycle.Event implementer in event.go -- "+
			"add a row (and a typeNameOf/decodeEvent case in marshal.go) for any name only on the left, "+
			"and remove any stale row for a name only on the right")
}

// eventImplementersInSource parses pkg/agent/session/lifecycle/event.go and
// returns the exported struct type names that embed baseEvent -- see
// TestEventTableIsExhaustive for why that embed is the implementer set.
func eventImplementersInSource(t *testing.T) []string {
	t.Helper()
	// go test always runs with the working directory set to this package's
	// own source directory (pkg/memory/kinds/lifecycle); event.go lives three
	// levels up and back down into agent/session/lifecycle.
	const eventGoPath = "../../../agent/session/lifecycle/event.go"

	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, eventGoPath, nil, 0)
	require.NoError(t, err, "parse %s -- if pkg/agent/session/lifecycle/event.go moved, update eventGoPath alongside it", eventGoPath)

	var names []string
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name == "baseEvent" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			for _, f := range st.Fields.List {
				if len(f.Names) != 0 {
					continue // a named field, not an embed
				}
				if ident, ok := f.Type.(*ast.Ident); ok && ident.Name == "baseEvent" {
					names = append(names, ts.Name.Name)
					break
				}
			}
		}
	}
	return names
}

func TestStillAppendOnly(t *testing.T) {
	assert.True(t, kind.New().Retention().AppendOnly, "lifecycle stays append-only")
}
