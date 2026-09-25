// pkg/controllers/monitoring/targets.go
//
// Targets is the declarative table that drives the monitoring watchers:
// one row per (CR type, status condition) the framework reports on. Add
// a row to monitor a new condition; no other code changes.
package monitoring

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Rule maps one status condition on one CR type to a monitoring
// classification. BadStatus is the metav1.ConditionStatus value that
// means "this is a failure" — False for Refresh/Valid/Connected, True
// for Failed.
type Rule struct {
	ConditionType string
	BadStatus     metav1.ConditionStatus
	Level         string // channelevents.MonitoringLevel*
	Category      string // "credential" | "reconcile" | "session" | "transport"
	Hint          string // optional; "{name}" is replaced with the object name
	// Terminal marks a condition that records a PAST INCIDENT rather than
	// current health: once it goes bad it never recovers, and the object keeps
	// carrying it forever.
	//
	// It suppresses only the cold-start re-announce. detectTransition treats a
	// first observation that is already failing as none→failed so an operator
	// restart re-surfaces open problems (a failure that began during downtime
	// is not missed) — correct for every live-health row here, whose trigger
	// clears itself once fixed. A terminal row has no such bound: its objects
	// accumulate, so a cold sweep would re-announce the entire history on every
	// restart. Live transitions observed while running still emit.
	Terminal bool
}

// Target binds one CR type to the rules watched on it. New returns a
// fresh empty object for controller-runtime's For() and the per-reconcile
// Get; GetConditions extracts that object's status conditions.
type Target struct {
	GVKName       string
	New           func() client.Object
	GetConditions func(client.Object) []metav1.Condition
	Rules         []Rule
}

// Targets returns the monitored-condition table.
//
// This list is hand-maintained and NOT verified for completeness against
// every CRD that carries a Valid (or equivalent health) condition — see
// TestTargets_Shape's doc comment for why an automated completeness check
// isn't wired up. When you add a new CRD with a status condition that
// signals reconcile failure, add a row here in the same reconcile pass;
// don't rely on a later audit to catch the gap.
func Targets() []Target {
	return []Target{
		{
			GVKName: "AgentIdentity",
			New:     func() client.Object { return &spiceboxv1alpha1.AgentIdentity{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.AgentIdentity).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.AgentIdentityConditionRefresh,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelError,
					Category:      "credential",
					Hint:          "re-run `oap identity refresh {name}`",
				},
				{
					ConditionType: spiceboxv1alpha1.AgentIdentityConditionValid,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelError,
					Category:      "credential",
				},
			},
		},
		{
			GVKName: "AgentSession",
			New:     func() client.Object { return &spiceboxv1alpha1.AgentSession{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.AgentSession).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.AgentSessionConditionFailed,
					BadStatus:     metav1.ConditionTrue,
					Level:         channelevents.MonitoringLevelError,
					Category:      "session",
					// The one terminal row: every writer sets Failed=True, none
					// ever clears it, and the CR outlives its pods by design
					// (reapSessionPods keeps the session for debugging) with no
					// GC sweeper. Without this, each operator restart re-posts
					// every failure the cluster has ever had.
					Terminal: true,
				},
			},
		},
		{
			GVKName: "AgentClass",
			New:     func() client.Object { return &spiceboxv1alpha1.AgentClass{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.AgentClass).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.AgentClassConditionValid,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "reconcile",
				},
			},
		},
		{
			GVKName: "AgentUI",
			New:     func() client.Object { return &spiceboxv1alpha1.AgentUI{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.AgentUI).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.AgentUIConditionValid,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "reconcile",
				},
				// ToolsGranted is deliberately NOT monitored here: it goes False
				// when an operator grants only a subset of the UI's requested
				// tools, which is a legitimate deployment choice (see
				// AgentUIConditionToolsGranted's doc comment), not a fault.
				// Alerting on it would be noise, not a diagnosable failure.
			},
		},
		{
			GVKName: "MCPServer",
			New:     func() client.Object { return &spiceboxv1alpha1.MCPServer{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.MCPServer).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.MCPServerConditionValid,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "reconcile",
				},
				// PinDrift condition: True=healthy (no drift), False=problem.
				// BadStatus=False fires on PinDrifted and PinVerifyFailed;
				// recovery fires when the condition flips back to True (PinMatch).
				{
					ConditionType: spiceboxv1alpha1.PinDriftCondition,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "pinning",
				},
			},
		},
		{
			GVKName: "Channel",
			New:     func() client.Object { return &spiceboxv1alpha1.Channel{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.Channel).Status.Conditions
			},
			Rules: []Rule{
				{
					ConditionType: spiceboxv1alpha1.ChannelConditionConnected,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelError,
					Category:      "transport",
				},
				// Deliverable is stamped by channelsd's outbound relay from
				// real send outcomes; Connected cannot see this failure (the
				// socket attaches fine while the bot is not a member of the
				// destination). NOT Terminal: a successful send flips it back
				// True, so a cold re-announce on operator restart correctly
				// re-surfaces a channel that STILL cannot deliver.
				{
					ConditionType: spiceboxv1alpha1.ChannelConditionDeliverable,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelError,
					Category:      "transport",
					Hint:          "outbound delivery through Channel {name} is failing; check the destination exists and the bot has been invited to it",
				},
			},
		},
		{
			GVKName: "RelationshipSource",
			New:     func() client.Object { return &spiceboxv1alpha1.RelationshipSource{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.RelationshipSource).Status.Conditions
			},
			Rules: []Rule{
				// A directory sync that cannot run at all: a revoked token, an
				// unregistered kind, a parked source (two CRs claiming one
				// spec.kind), or an enumeration that produced nothing. Error,
				// because group membership silently stops tracking the
				// directory and every authorization decision downstream keeps
				// using whatever was last written.
				//
				// NOT Terminal. Terminal is for a condition that records a past
				// incident and never recovers (AgentSessionConditionFailed's
				// case — see Rule.Terminal). Ready flips back True on the next
				// good pass, and a source that is STILL broken after an
				// operator restart is exactly what the cold-start re-announce
				// exists to re-surface.
				{
					ConditionType: spiceboxv1alpha1.RelationshipSourceConditionReady,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelError,
					Category:      "reconcile",
					Hint:          "directory sync {name} is not running; check `oap directory` and the credential on its spec.auth",
				},
				// Partial failure: the pass RAN and some of it failed. This is
				// the row that exists because Ready cannot express it — a
				// GitHub source whose every per-repository team fetch answered
				// 403 reported Ready=True/Synced with 156 scopes processed, and
				// nothing reached anybody. Warning, not Error: per-scope
				// failures are non-fatal by design (relsync.Pass's own doc) and
				// the rest of the directory did sync.
				//
				// True-is-bad, unlike every other row here — the condition's
				// polarity, not an inversion invented at this table.
				//
				// NOT Terminal, for the same reason as above and one more: the
				// condition is rewritten from each completed pass, so it clears
				// itself the moment the directory is repaired.
				{
					ConditionType: spiceboxv1alpha1.RelationshipSourceConditionPartialFailure,
					BadStatus:     metav1.ConditionTrue,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "reconcile",
					Hint:          "part of directory sync {name} is failing; see status.sync.lastPass.scopeErrorSamples for which scopes and why",
				},
			},
		},
		{
			GVKName: "SidecarToolbox",
			New:     func() client.Object { return &spiceboxv1alpha1.SidecarToolbox{} },
			GetConditions: func(o client.Object) []metav1.Condition {
				return o.(*spiceboxv1alpha1.SidecarToolbox).Status.Conditions
			},
			Rules: []Rule{
				// PinDrift condition: True=healthy (no drift), False=problem.
				// BadStatus=False fires on PinDrifted and PinVerifyFailed;
				// recovery fires when the condition flips back to True (PinMatch).
				{
					ConditionType: spiceboxv1alpha1.PinDriftCondition,
					BadStatus:     metav1.ConditionFalse,
					Level:         channelevents.MonitoringLevelWarning,
					Category:      "pinning",
				},
			},
		},
	}
}
