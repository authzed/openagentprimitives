package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The authz windows the runner gates on are resolved by pkg/platform/settings across all
// four tiers and stamped onto AgentSession.status.effectiveSettings.authz by the
// AgentSession reconciler before the pod is created. The runner MUST read that
// resolved snapshot: reading the AgentClass directly makes a cluster/namespace
// admin's tightened window inert while status keeps reporting it as effective.
//
// These tests pin the read side. The tier fold itself is pinned in
// pkg/platform/settings/resolve_authz_test.go.

// sessionWithEffectiveAuthz builds a session whose status carries the given
// resolved authz stanza — the shape the operator stamps.
func sessionWithEffectiveAuthz(t *testing.T, a spiceboxv1alpha1.EffectiveAuthz) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{Authz: a},
		},
	}
}

// classWithAuthz builds an AgentClass declaring its own (looser) windows.
func classWithAuthz(t *testing.T, approval, leakTTL time.Duration, latencyMs int32) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				ApprovalTimeout: &metav1.Duration{Duration: approval},
				InformationLeakage: &spiceboxv1alpha1.InformationLeakagePolicy{
					Mode:        "enforcing",
					ApprovalTTL: &metav1.Duration{Duration: leakTTL},
				},
				Scope: &spiceboxv1alpha1.ScopeSpec{Enabled: true, MaxLLMLatencyMs: latencyMs},
			},
		},
	}
}

// TestLoop_authzWindows_preferResolvedEffectiveSettings is the regression test
// for the inert-control defect: a tier that tightened a window must be what the
// runner gates on, not the AgentClass value (nor the built-in fallback).
func TestLoop_authzWindows_preferResolvedEffectiveSettings(t *testing.T) {
	t.Run("class omits every window; a tighter cluster default is what gates", func(t *testing.T) {
		// The AgentClass declares no authz block at all, so its own resolution
		// is the built-in 10m/10m/5000. A cluster admin set 2m/90s/1500, which
		// the resolver folded into status.effectiveSettings.authz.
		l := &Loop{
			AgentClass: &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent"},
			},
			AgentSession: sessionWithEffectiveAuthz(t, spiceboxv1alpha1.EffectiveAuthz{
				ApprovalTimeout:               metav1.Duration{Duration: 2 * time.Minute},
				InformationLeakageApprovalTTL: metav1.Duration{Duration: 90 * time.Second},
				ScopeMaxLLMLatencyMs:          1500,
			}),
		}

		assert.Equal(t, 2*time.Minute, l.resolvedApprovalTimeout(),
			"approval wait must come from the resolved snapshot, not the built-in 10m")
		assert.Equal(t, 90*time.Second, l.resolvedLeakageApprovalTTL(),
			"leakage approval reuse must come from the resolved snapshot")
		assert.Equal(t, int32(1500), l.resolvedScopeLLMLatency(),
			"cold-start extractor latency must come from the resolved snapshot")
	})

	t.Run("resolved snapshot beats a looser AgentClass value", func(t *testing.T) {
		// The resolver clamps a class value that exceeds a tier ceiling; the
		// runner must gate on the clamped result, not the raw class spec.
		l := &Loop{
			AgentClass:    classWithAuthz(t, 30*time.Minute, 30*time.Minute, 30000),
			LeakageConfig: classWithAuthz(t, 30*time.Minute, 30*time.Minute, 30000).Spec.Authz.InformationLeakage,
			AgentSession: sessionWithEffectiveAuthz(t, spiceboxv1alpha1.EffectiveAuthz{
				ApprovalTimeout:               metav1.Duration{Duration: 5 * time.Minute},
				InformationLeakageApprovalTTL: metav1.Duration{Duration: 5 * time.Minute},
				ScopeMaxLLMLatencyMs:          5000,
			}),
		}

		assert.Equal(t, 5*time.Minute, l.resolvedApprovalTimeout())
		assert.Equal(t, 5*time.Minute, l.resolvedLeakageApprovalTTL())
		assert.Equal(t, int32(5000), l.resolvedScopeLLMLatency())
	})

	t.Run("no stamped status: falls back to the AgentClass", func(t *testing.T) {
		// kubectl-driven / in-process Loops carry no resolved snapshot.
		// internal/cmd/runner refuses to start without one, so this path is tests and
		// embedded callers only — the class remains the best available source.
		class := classWithAuthz(t, 7*time.Minute, 3*time.Minute, 250)
		l := &Loop{AgentClass: class, LeakageConfig: class.Spec.Authz.InformationLeakage}

		assert.Equal(t, 7*time.Minute, l.resolvedApprovalTimeout())
		assert.Equal(t, 3*time.Minute, l.resolvedLeakageApprovalTTL())
		assert.Equal(t, int32(250), l.resolvedScopeLLMLatency())
	})

	t.Run("nothing set anywhere: built-in defaults", func(t *testing.T) {
		l := &Loop{}

		assert.Equal(t, 10*time.Minute, l.resolvedApprovalTimeout())
		assert.Equal(t, 10*time.Minute, l.resolvedLeakageApprovalTTL())
		assert.Equal(t, int32(5000), l.resolvedScopeLLMLatency())
	})
}

// TestRunnerHost_contentInspectionTTL_usesResolvedApprovalTimeout pins the
// content-inspection approval window onto the same resolved value as every
// other approval ask — it used to re-read the AgentClass independently.
func TestRunnerHost_contentInspectionTTL_usesResolvedApprovalTimeout(t *testing.T) {
	h := &runnerHost{l: &Loop{
		AgentClass: classWithAuthz(t, 30*time.Minute, 30*time.Minute, 30000),
		AgentSession: sessionWithEffectiveAuthz(t, spiceboxv1alpha1.EffectiveAuthz{
			ApprovalTimeout: metav1.Duration{Duration: 4 * time.Minute},
		}),
	}}

	assert.Equal(t, 4*time.Minute, h.contentInspectionTTL())
}
