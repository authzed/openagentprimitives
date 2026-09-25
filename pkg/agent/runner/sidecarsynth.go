package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// SidecarProber probes a sidecar's live MCP tool list. Abstracted so every
// caller — the production runner boot pass, its mid-session refresher, and the
// in-process e2e harness — injects its own HTTP client (plain vs SSRF-guarded)
// without the synth core depending on either.
type SidecarProber func(ctx context.Context, url string) ([]mcpprobe.Tool, error)

// RetryingProber wraps base so a probe against a not-yet-bound sidecar is retried
// until it succeeds or timeout elapses, polling every poll interval.
//
// In-pod sidecars are regular pod containers that start in PARALLEL with the
// runner (no ordering guarantee — they are appended to Pod.Spec.Containers, not
// gated as native/init sidecars), so the runner can dial the sidecar's loopback
// MCP endpoint a beat before its server binds, getting a transient "connection
// refused". The sidecar's own startupProbe budgets up to
// healthcheck.timeoutSeconds for it to come up; the boot probe must tolerate the
// same window rather than failing the whole session on the first dial. A sidecar
// that never binds still fails — with the real underlying error — once the
// deadline passes. Cancellation abandons the loop at once.
//
// Only the boot pass wraps its prober: the mid-session refresher probes
// separate-pod sidecars whose pod is already Ready (its startupProbe passed)
// before it is picked up, so its endpoint is bound and a one-shot probe is right.
func RetryingProber(base SidecarProber, timeout, poll time.Duration) SidecarProber {
	return func(ctx context.Context, url string) ([]mcpprobe.Tool, error) {
		deadline := time.Now().Add(timeout)
		for {
			tools, err := base(ctx, url)
			if err == nil {
				return tools, nil
			}
			if !time.Now().Before(deadline) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(poll):
			}
		}
	}
}

// ProbeSynthSidecar is the SINGLE shared core behind the runner boot pass, the
// mid-session refresher, AND the in-process e2e factory: probe one sidecar's
// live MCP endpoint at url, synthesize its allowlisted tools, stamp image
// provenance, and record the per-session reachability result on status — so that
// logic lives in exactly ONE place instead of being copied per call site.
//
// On probe or synthesize failure it records Reachable=false + the reason on
// status (so a sidecar that is up but unreachable/drifted is never silent) and
// returns the wrapped error; the caller decides whether that is fatal (boot:
// WriteFailed) or best-effort (refresh: retry next turn). On success it records
// Reachable=true + the observed tool names and returns the synthesized tools.
// The reachability write is best-effort: a failed status write is logged, never
// propagated, so it can neither fail a successful synth nor mask the real
// probe/synth error. sp and provenance may be nil (recording / provenance
// stamping are then skipped).
//
// sessionCache is the caller's per-AgentSession MCP session cache, handed to
// every tool this synthesizes so all of a sidecar's calls ride ONE MCP session
// and its server-side per-session state (dedicated-mcp's PermissionSystem
// selection) survives across calls. REQUIRED rather than an option because
// forgetting it is invisible until an agent notices its selection resetting
// mid-conversation. Production callers pass the runner's cache; only tests with
// no stateful server to talk to pass nil (fresh session per call).
func ProbeSynthSidecar(
	ctx context.Context,
	url string,
	rt spiceboxv1alpha1.ResolvedSidecarToolbox,
	prober SidecarProber,
	observedPins []spiceboxv1alpha1.ObservedPin,
	provenance *ToolProvenance,
	sp *StatusPatcher,
	sessionCache *mcpprobe.SessionCache,
) ([]agenttool.Tool, error) {
	live, perr := prober(ctx, url)
	if perr != nil {
		recordReachability(ctx, sp, rt.Name, false, nil, fmt.Sprintf("probe %s: %v", url, perr))
		return nil, fmt.Errorf("probe %q at %s: %w", rt.Name, url, perr)
	}
	observed := probeToolNames(live)
	built, serr := sidecartoolboxsynth.Synthesize(rt, live, sessionCache)
	if serr != nil {
		// tools/list succeeded but the allowlist is unsatisfied (drift): record
		// what the server DID offer so the mismatch is diagnosable from status.
		recordReachability(ctx, sp, rt.Name, false, observed, serr.Error())
		return nil, fmt.Errorf("synthesize %q: %w", rt.Name, serr)
	}
	stampSidecarProvenance(rt, built, observedPins, provenance)
	recordReachability(ctx, sp, rt.Name, true, observed, "")
	return built, nil
}

// recordReachability writes the per-session reachability result best-effort: a
// status-write error is logged, not returned, so observability never changes
// the caller's control flow.
func recordReachability(ctx context.Context, sp *StatusPatcher, name string, reachable bool, observed []string, unreachable string) {
	if sp == nil {
		return
	}
	if err := sp.RecordSidecarReachability(ctx, name, reachable, observed, unreachable); err != nil {
		slog.Default().Info("record sidecar reachability failed",
			"sidecar", name, "reachable", reachable, "err", err.Error())
	}
}

// stampSidecarProvenance mirrors the boot/refresh provenance logic in one place:
// for each synthesized tool it looks up the sidecar's image ObservedPin (written
// by the agentsession controller at session start, keyed by rt.Ref) and stamps
// it on the shared provenance state so the authz audit entry carries the pin.
// No-op when provenance is nil or no image pin was observed.
func stampSidecarProvenance(
	rt spiceboxv1alpha1.ResolvedSidecarToolbox,
	built []agenttool.Tool,
	observedPins []spiceboxv1alpha1.ObservedPin,
	provenance *ToolProvenance,
) {
	if provenance == nil {
		return
	}
	var pin spiceboxv1alpha1.PinRecord
	for _, op := range observedPins {
		if op.Name == rt.Ref && op.Pin.Kind == imagepin.KindName {
			pin = op.Pin
			break
		}
	}
	if pin.Kind == "" {
		return
	}
	for _, t := range built {
		provenance.Set(t.Name(), ProvenanceRecord{Pin: pin, Name: rt.Ref})
	}
}

// probeToolNames returns the sorted tool names from a live tools/list result —
// the runtime-observed tool surface recorded on status.sidecarReachability.
func probeToolNames(live []mcpprobe.Tool) []string {
	names := make([]string, 0, len(live))
	for _, t := range live {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
