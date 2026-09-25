package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"

	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newlyReadySidecars returns the separate-pod SidecarToolbox entries in
// `resolved` needing (re-)synthesis: RunMode=="separate-pod", !AwaitingSecret,
// SidecarPodIP!="", and either absent from `prevSynthed` or recorded against a
// DIFFERENT SidecarPodIP. It is the pure core of mid-session re-synthesis, and
// covers both first readiness (a producer tool emitted the gating secret and
// the sidecar pod has now come up) and pod replacement (a new secret value made
// the operator recreate the pod, so the stale tools must be replaced).
//
// Only separate-pod sidecars participate: an in-pod sidecar's port is injected
// as env at boot, so the boot pass either synthesizes it or fails the session —
// it never becomes ready mid-session. prevSynthed is keyed by
// ResolvedSidecarToolbox.Name (the LLM-facing prefix, unique per AgentClass
// sidecar ref) and valued by the SidecarPodIP last synthesized for that ref, so
// a pod replacement (new IP)
// re-triggers synthesis while an unchanged pod does not.
func newlyReadySidecars(prevSynthed map[string]string, resolved []spiceboxv1alpha1.ResolvedSidecarToolbox) []spiceboxv1alpha1.ResolvedSidecarToolbox {
	var out []spiceboxv1alpha1.ResolvedSidecarToolbox
	for _, rt := range resolved {
		if rt.RunMode != "separate-pod" {
			continue
		}
		if rt.AwaitingSecret || rt.SidecarPodIP == "" {
			continue
		}
		// Same pod IP → nothing to do. A different one means the pod was
		// replaced, so fall through and re-synthesize against the new one.
		if prevIP, ok := prevSynthed[rt.Name]; ok && prevIP == rt.SidecarPodIP {
			continue
		}
		out = append(out, rt)
	}
	return out
}

// terminallyFailedSidecars returns the separate-pod entries whose
// operator-recorded terminal PodFailure fingerprint differs from the one in
// prevNotified (keyed by ResolvedSidecarToolbox.Name) — i.e. not yet reported.
//
// It is the counterpart to newlyReadySidecars, and the two are mutually
// exclusive by construction: a pod that never becomes Ready never receives a
// PodIP, so it can never appear there. Without this the failure is silent
// and the agent finished its turn without the toolset it was promised, never
// told why.
//
// The dedup key is the FAILURE, not the sidecar. The refresher runs at the top
// of every turn, so keying on name alone would either repeat one crash on every
// turn or permanently mask a different later failure on the same sidecar.
func terminallyFailedSidecars(prevNotified map[string]string, resolved []spiceboxv1alpha1.ResolvedSidecarToolbox) []spiceboxv1alpha1.ResolvedSidecarToolbox {
	var out []spiceboxv1alpha1.ResolvedSidecarToolbox
	for _, rt := range resolved {
		if rt.RunMode != "separate-pod" || rt.PodFailure == nil {
			continue
		}
		if prev, ok := prevNotified[rt.Name]; ok && prev == podFailureFingerprint(rt.PodFailure) {
			continue
		}
		out = append(out, rt)
	}
	return out
}

// sidecarFailureReportedAnnotation holds, as a JSON object, the sidecar-name →
// failure-fingerprint map the runner has already reported. It is the DURABLE
// half of the once-per-failure contract: podFailure persists for as long as the
// sidecar stays broken, so a process-local record alone would re-report the same
// failure on every runner restart.
//
// Fingerprint-valued rather than presence-valued, so a genuinely different later
// failure of the same sidecar still reaches the user.
//
// An annotation and deliberately not status: status here is the operator's
// observation of the pods, which the runner does not own.
const sidecarFailureReportedAnnotation = "agentprimitives.authzed.com/sidecar-failure-reported"

// decodeNotifiedFailures parses sidecarFailureReportedAnnotation. A malformed
// or absent value yields an empty map — the cost of failing open here is one
// duplicate notice, whereas failing closed would silently suppress a real
// failure report the user is waiting on.
func decodeNotifiedFailures(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// persistNotifiedFailures mirrors the reported set onto the session. Encoding
// the whole map (rather than one key per sidecar) keeps a healed sidecar's
// entry from lingering: the annotation is always the complete current set.
// json.Marshal sorts object keys, so an unchanged set re-encodes byte-identically
// and the patch is a no-op.
func persistNotifiedFailures(ctx context.Context, c client.Client, key client.ObjectKey, notified map[string]string) error {
	value := ""
	if len(notified) > 0 {
		encoded, err := json.Marshal(notified)
		if err != nil {
			return fmt.Errorf("encode reported failures: %w", err)
		}
		value = string(encoded)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{sidecarFailureReportedAnnotation: value},
		},
	})
	if err != nil {
		return fmt.Errorf("build annotation patch: %w", err)
	}
	return c.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		},
		client.RawPatch(types.MergePatchType, patch),
	)
}

// podFailureFingerprint identifies one distinct failure of one sidecar, so a
// repeat of the SAME failure is suppressed while a genuinely different one is
// reported. The exit code is deliberately excluded: it adds no diagnostic
// signal beyond the reason+message pair and would, on a container whose exit
// code varies between restarts, defeat the dedup it participates in.
func podFailureFingerprint(f *spiceboxv1alpha1.SidecarPodFailure) string {
	return f.Reason + "|" + f.Message
}

// sidecarProber probes a separate-pod sidecar's live MCP tool list. Extracted
// as an interface (runner.SidecarProber) so the refresher's probe+synthesize
// path is unit-testable without a real httptest MCP server. The production
// implementation (mcpProbeFunc) issues a tools/list over plain HTTP to the pod
// IP.

// mcpProbeFunc is the production runner.SidecarProber: it probes the sidecar's MCP
// endpoint with a plain (non-SSRF-guarded) HTTP client. Both reach paths
// (loopback in-pod, pod IP separate-pod) are operator-controlled, not
// LLM/user-supplied URLs, so the SSRF guard must not apply — mirrors the boot
// synthesis path in run().
func mcpProbeFunc(ctx context.Context, url string) ([]mcpprobe.Tool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return (&mcpprobe.Client{HTTP: &http.Client{}, URL: url}).ListTools(probeCtx, "", "")
}

// newSidecarToolRefresher builds the runner.Loop.ToolRefresher closure that
// re-synthesizes newly-ready or pod-replaced secret-gated separate-pod
// sidecars: re-Get the session, compute newlyReadySidecars, probe + synthesize,
// record the pod IP, stamp image provenance, and hand the tools back. Returned
// tools that share a name with live ones replace them in place.
//
// `synthed` (name → SidecarPodIP) seeds the synthesized set so boot-synthesized
// sidecars are skipped until their pod IP changes. The closure mutates it in
// place and is NOT safe for concurrent use; the loop calls the refresher
// sequentially at the top of each turn.
//
// `provenance` may be nil, in which case recording is skipped. When set, each
// synthesized tool's image ObservedPin (by rt.Ref + imagepin.KindName) is
// stamped onto the shared state so recordAuthzDecision can audit it.
//
// A probe or synthesize failure is returned as an error and short-circuits the
// call, but the sidecar's recorded IP is deliberately LEFT UNCHANGED so the next
// turn retries: a momentarily-unreachable pod must not be permanently dropped,
// and a replaced sidecar keeps its stale tools until the new pod answers, so the
// consumer is never left tool-less mid-replace.
//
// `sessionCache` matters most here: a secret-gated sidecar is AwaitingSecret at
// boot, so its tools exist only via this path — without the cache it would open
// a fresh MCP session per call, resetting any server-side state keyed by
// session id.
//
// `notify` delivers the user-visible half of a terminal-failure report. Nil for
// callers with no channel surface, leaving only the agent-facing advisory.
func newSidecarToolRefresher(
	c client.Client,
	sessKey client.ObjectKey,
	probe runner.SidecarProber,
	synthed map[string]string,
	provenance *runner.ToolProvenance,
	sp *runner.StatusPatcher,
	sessionCache *mcpprobe.SessionCache,
	notify func(ctx context.Context, n *notice.Notice),
) func(ctx context.Context) (runner.ToolRefreshResult, error) {
	if synthed == nil {
		synthed = map[string]string{}
	}
	// notified records the last failure fingerprint REPORTED per sidecar, so one
	// crash is reported once rather than on every turn. Kept separate from
	// `synthed` because they answer different questions and a sidecar can move
	// between them (fail → recover → fail again). Seeded from the session on the
	// first pass (see sidecarFailureReportedAnnotation) so the "once" holds
	// across a runner restart, not just across the turns of one process.
	notified := map[string]string{}
	seeded := false

	return func(ctx context.Context) (runner.ToolRefreshResult, error) {
		var sess spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, sessKey, &sess); err != nil {
			return runner.ToolRefreshResult{}, fmt.Errorf("sidecar refresh: get AgentSession: %w", err)
		}
		resolved := sess.Status.ResolvedSidecarToolboxes

		var res runner.ToolRefreshResult

		// Seed the reported set from the session on the first pass of THIS
		// process. The runner pod restarts in place (RestartPolicy=OnFailure)
		// and is re-created on every idle-sleep → wake, while the operator
		// keeps writing the identical PodFailure for a sidecar that will never
		// start — so an in-memory-only record re-reports the same failure into
		// the thread on every restart. Seeded here rather than at construction
		// because this is where the session is read.
		if !seeded {
			for name, fp := range decodeNotifiedFailures(sess.Annotations[sidecarFailureReportedAnnotation]) {
				if _, ok := notified[name]; !ok {
					notified[name] = fp
				}
			}
			seeded = true
		}
		before := len(notified)

		// Terminal failures first: a sidecar whose pod will never become Ready
		// never gets a PodIP, so it can never appear in newlyReadySidecars. This
		// is the only place it is ever noticed. Reporting it BEFORE the probe
		// loop means an unrelated probe error below cannot swallow it.
		changed := false
		for _, rt := range terminallyFailedSidecars(notified, resolved) {
			notified[rt.Name] = podFailureFingerprint(rt.PodFailure)
			changed = true
			if notify != nil {
				notify(ctx, sidecarFailureNotice(rt))
			}
			res.Advisories = append(res.Advisories, sidecarFailureAdvisory(rt))
		}
		// Drop the record for any sidecar that is no longer failing, so a LATER
		// failure — even one with an identical cause — is reported again rather
		// than being suppressed by the fingerprint of the healed one.
		for _, rt := range resolved {
			if rt.PodFailure == nil {
				delete(notified, rt.Name)
			}
		}
		if changed || len(notified) != before {
			// Best-effort: the in-memory map already suppresses repeats for this
			// process, so a patch failure costs one duplicate notice after the
			// next restart — never a swallowed report. Logged, never silent.
			if err := persistNotifiedFailures(ctx, c, sessKey, notified); err != nil {
				log.FromContext(ctx).Info("sidecar refresh: persist reported-failure annotation",
					"session", sessKey.String(), "err", err.Error())
			}
		}

		ready := newlyReadySidecars(synthed, resolved)
		if len(ready) == 0 {
			return res, nil
		}
		for _, rt := range ready {
			url := fmt.Sprintf("http://%s:%d%s", rt.SidecarPodIP, rt.Port, sidecartoolboxsynth.EndpointPath(rt))
			// Same shared core as the boot pass: probe + synthesize + provenance
			// + record per-session reachability (so a mid-session sidecar that is
			// up but unreachable/drifted lands in status, not just a swallowed
			// log). On failure leave rt's recorded IP unchanged so the next
			// refresh retries it; the loop keeps the prior (possibly stale) tools
			// meanwhile — the consumer is never left tool-less mid-replace.
			built, err := runner.ProbeSynthSidecar(ctx, url, rt, probe, sess.Status.ObservedPins, provenance, sp, sessionCache)
			if err != nil {
				return res, fmt.Errorf("sidecar refresh: %w", err)
			}
			synthed[rt.Name] = rt.SidecarPodIP
			res.Added = append(res.Added, built...)
		}
		return res, nil
	}
}

// sidecarFailureNotice is the user-facing half of a terminal sidecar failure:
// a deterministic channel message that does not depend on the model choosing
// to relay it.
//
// Degraded rather than critical: the toolset is gone for the session, but the
// agent keeps working with whatever else it has. The failure detail is
// pod/controller text, so it travels in the Excerpt where every surface
// renders it inert.
func sidecarFailureNotice(rt spiceboxv1alpha1.ResolvedSidecarToolbox) *notice.Notice {
	return notice.New(categories.ToolboxUnavailable, notice.Args{
		Lead: fmt.Sprintf("Tools from %q are unavailable", rt.Name),
		Body: "They failed to start and will not come back by retrying, so this session runs without them.",
		NextStep: "Ask for something that doesn't need those tools, " +
			"or ask an operator to check the toolbox.",
		Excerpt:  &channelevents.InteractionExcerpt{Label: "Cause", Content: sidecarFailureDetail(rt)},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// sidecarFailureAdvisory is the agent-facing half: injected into the turn's
// context so the model knows a toolset it was told about is gone, can explain
// the cause in its own words, and stops instead of reporting success on work it
// could not actually do.
// The DETAIL is enveloped and the imperative is not, and the split is
// load-bearing. A SidecarToolbox is a user-supplied MCP server image, and the
// detail carries that container's OWN LOG TAIL: the pod spec sets
// TerminationMessagePolicy: FallbackToLogsOnError, so the kubelet copies the log
// into LastTerminationState.Terminated.Message, the reconciler mirrors it onto
// status.resolvedSidecarToolboxes[].podFailure.message, and it arrives here
// verbatim. Spliced bare into a platform-authored sentence, a payload written to
// stdout before a deliberate non-zero exit reads to the model as more platform
// speech — while every tool RESULT on the same turn is enveloped. The identical
// bytes are already labelled untrusted on the human surface, in the notice's
// Excerpt; this makes the two surfaces agree.
func sidecarFailureAdvisory(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string {
	return fmt.Sprintf(
		"[system] The tools from the %q toolbox are UNAVAILABLE for this session. "+
			"They will not become available by retrying. Do not claim work that needs them succeeded; "+
			"tell the user which capability is missing and why, then stop or continue only with the tools "+
			"you do have. The cause reported by the toolbox itself follows:\n%s",
		rt.Name, toolenvelope.Wrap(sidecarFailureDetail(rt), toolenvelope.NewNonce()))
}

// sidecarFailureDetail renders the operator's failure record as one line,
// leading with the actionable message and keeping the kubelet reason as
// context.
func sidecarFailureDetail(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string {
	if rt.PodFailure == nil {
		return "unavailable"
	}
	if msg := rt.PodFailure.Message; msg != "" {
		return fmt.Sprintf("%s (%s)", msg, rt.PodFailure.Reason)
	}
	return rt.PodFailure.Reason
}
