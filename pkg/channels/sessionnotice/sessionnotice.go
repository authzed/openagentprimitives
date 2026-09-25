// Package sessionnotice derives the user-facing statements a session's STATUS
// implies — "starting up", "waiting for capacity", "paused, retry to continue".
//
// It exists because those statements had exactly one delivery path, and that
// path cannot reach every surface. channelsd's sessionWatcher publishes them,
// and it skips every CLIENT-HOSTED session (clientHostedHere) — `browser` among
// them, since webd surfaces that transport, not channelsd. So a webchat user
// saw none of it: the transcript simply stopped and they sat looking at it,
// with the explanation sitting in an AgentSession condition nobody rendered.
//
// The derivation is pure status -> statement, deliberately holding no delivery
// logic: channelsd keeps its publish/dedup/annotation machinery, webd reads the
// same answers and renders them inline. One place decides what a state MEANS,
// two decide how to say it.
package sessionnotice

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Kind names what a Notice is about, so a surface can style or suppress one
// class without string-matching prose.
type Kind string

const (
	KindStartup       Kind = "startup"
	KindScheduling    Kind = "scheduling"
	KindAwaitingRetry Kind = "awaiting_retry"
	KindRestartDenied Kind = "restart_denied"
	KindFailed        Kind = "failed"
)

// Tone is the emphasis a surface should render with. Kept to the three the
// derivation can actually justify: a bounded wait is routine, a state needing
// the user is degraded, and a terminal failure the agent never closed is a
// failure.
type Tone string

const (
	ToneRoutine  Tone = "routine"
	ToneDegraded Tone = "degraded"
	ToneFailed   Tone = "failed"
)

// Notice is one user-facing statement about a session's current state.
//
// Body carries the machine-generated detail (a condition Message) and may be
// empty; Lead always stands alone, because a surface that shows only the lead
// must still be honest.
type Notice struct {
	Kind Kind   `json:"kind"`
	Tone Tone   `json:"tone"`
	Lead string `json:"lead"`
	Body string `json:"body,omitempty"`
	// NextStep is what the user can DO. Empty when there is nothing for them to
	// do but wait — never filled with advice a surface cannot honour.
	NextStep string `json:"nextStep,omitempty"`
}

// Text constants, shared so the two surfaces cannot drift into describing one
// state two ways.
const (
	GenericStartupLead = "Starting up — your request will run once everything's ready…"
	SchedulingLead     = "Waiting for capacity to run your request…"
	AwaitingRetryLead  = "This conversation is paused — the model connection dropped mid-turn."
	AwaitingRetryNext  = "Send a message to retry; it picks up where it left off."
	RestartDeniedLead  = "This session could not be restarted."
	GenericFailedLead  = "This request stopped before finishing."
)

// terminalFailureLeads maps a session's FailureReason token to friendly,
// user-facing text. A Failed session died WITHOUT the agent delivering a
// closing message, so its reason token (e.g. "BundleFailed") is the only thing
// naming what happened — and a raw CamelCase token is not a sentence a user
// should read. An unmapped reason falls back to GenericFailedLead rather than
// leaking the token, so a newly-added failure reason degrades to a plain-but-
// honest statement instead of going silent (no-silent-errors) or exposing a
// code word. The detailed condition Message still travels in the notice Body.
var terminalFailureLeads = map[string]string{
	spiceboxv1alpha1.ReasonAgentSessionBundleFail:      "This request couldn't run — the tools it needed failed to start.",
	spiceboxv1alpha1.ReasonAgentSessionMemoryDown:      "This request stopped — the memory service was unavailable.",
	spiceboxv1alpha1.ReasonAgentSessionRunnerCrash:     "This request stopped — the agent process crashed.",
	spiceboxv1alpha1.ReasonAgentSessionRetryTimeout:    "This request stopped — it couldn't recover after repeated retries.",
	spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail:  "This request couldn't start — an authorization write failed.",
	spiceboxv1alpha1.ReasonCredentialLinkTimeout:       "This request couldn't run — it timed out waiting for its credentials to be linked.",
	spiceboxv1alpha1.ReasonPodStartFailed:              "This request couldn't run — its workload failed to start.",
	spiceboxv1alpha1.ReasonWorkspaceProvisioningFailed: "This request couldn't run — its workspace storage couldn't be provisioned.",
}

// StartupWaitReasons enumerates the RunnerReady=False reasons that represent a
// benign, bounded "still bringing up the session" wait. The single extension
// point: a new startup dependency is wired up by adding its reason and text.
// Reasons NOT listed are deliberately absent so they keep their normal
// watchdog/failure handling rather than reading as a routine wait.
var StartupWaitReasons = map[string]string{
	spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector: "Starting the security scanner — your request will run once it's ready…",
}

// StartupWait reports whether the session is in a benign startup wait, and the
// notice to show. Pure over status.
func StartupWait(sess *spiceboxv1alpha1.AgentSession) (string, bool) {
	c := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		return "", false
	}
	text, ok := StartupWaitReasons[c.Reason]
	if ok && text == "" {
		text = GenericStartupLead
	}
	return text, ok
}

// Derive returns every notice this session's status currently implies, most
// actionable first.
//
// A terminal SUCCESS yields nothing: a surface showing a transcript already
// ends it with the session's own closing message, and repeating it as a banner
// says the same thing twice. A terminal FAILURE is the exception — it died
// WITHOUT the agent delivering any closing message (an eviction, a memory
// outage, a crash at boot), so the transcript just stops and nothing else names
// what happened. That one gets a notice.
func Derive(sess *spiceboxv1alpha1.AgentSession) []Notice {
	if sess == nil {
		return nil
	}
	var out []Notice

	// A terminal failure the agent never closed: surface it. Nothing else in this
	// function applies to a Failed phase (scheduling/startup are gated to
	// pending/running, AwaitingRetry to its own phase), so this returns on its own.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed {
		lead := terminalFailureLeads[sess.Status.FailureReason]
		if lead == "" {
			lead = GenericFailedLead
		}
		return []Notice{{
			Kind: KindFailed, Tone: ToneFailed,
			Lead: lead,
			Body: conditionMessage(sess, spiceboxv1alpha1.AgentSessionConditionFailed),
		}}
	}

	// AwaitingRetry first: it is the only one the user can act on, and a session
	// waiting on a person should not have that buried under a wait notice.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry {
		out = append(out, Notice{
			Kind: KindAwaitingRetry, Tone: ToneDegraded,
			Lead:     AwaitingRetryLead,
			Body:     conditionMessage(sess, spiceboxv1alpha1.AgentSessionConditionAwaitingRetry),
			NextStep: AwaitingRetryNext,
		})
	}

	if c := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRestartDenied); c != nil && c.Status == metav1.ConditionTrue {
		out = append(out, Notice{
			Kind: KindRestartDenied, Tone: ToneDegraded,
			Lead: RestartDeniedLead, Body: c.Message,
		})
	}

	if text, ok := StartupWait(sess); ok {
		out = append(out, Notice{Kind: KindStartup, Tone: ToneRoutine, Lead: text})
	}

	// Scheduling is only meaningful while a session is still bootstrapping or
	// working a live turn — a parked or terminal session has nothing in flight
	// for the user to be waiting on.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending ||
		sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning {
		if c := meta.FindStatusCondition(sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionSandboxScheduling); c != nil && c.Status == metav1.ConditionFalse {
			out = append(out, Notice{
				Kind: KindScheduling, Tone: ToneRoutine,
				Lead: SchedulingLead, Body: c.Message,
			})
		}
	}
	return out
}

func conditionMessage(sess *spiceboxv1alpha1.AgentSession, condType string) string {
	if c := meta.FindStatusCondition(sess.Status.Conditions, condType); c != nil {
		return c.Message
	}
	return ""
}
