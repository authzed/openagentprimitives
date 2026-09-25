package settings

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func metav1Duration(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }

func int32p(v int32) *int32 { return &v }

// The authz windows compose like every other tier field: Defaults.Authz
// inherits downward and the class may override it, while Limits.Authz is a
// ceiling the class can only tighten. These tests pin the ceiling half and the
// provenance/violation reporting that goes with it; the plain default fold is
// covered by TestResolve_Authz in resolve_test.go.

func authzCeiling(maxApproval, maxLeakTTL time.Duration) *v1.SettingsLimits {
	c := &v1.SettingsAuthzCeiling{}
	if maxApproval > 0 {
		c.MaxApprovalTimeout = metav1Duration(maxApproval)
	}
	if maxLeakTTL > 0 {
		c.MaxInformationLeakageApprovalTTL = metav1Duration(maxLeakTTL)
	}
	return &v1.SettingsLimits{Authz: c}
}

func classAuthz(approval, leakTTL time.Duration) *v1.AuthzBlock {
	b := &v1.AuthzBlock{}
	if approval > 0 {
		d := metav1Duration(approval)
		b.ApprovalTimeout = &d
	}
	if leakTTL > 0 {
		d := metav1Duration(leakTTL)
		b.InformationLeakage = &v1.InformationLeakagePolicy{Mode: "enforcing", ApprovalTTL: &d}
	}
	return b
}

func TestResolve_AuthzCeiling(t *testing.T) {
	t.Run("class asks 30m over a 5m cluster ceiling: clamped to 5m, non-fatal AuthzClamped", func(t *testing.T) {
		got, vs := Resolve(Inputs{
			ClassAuthz: classAuthz(30*time.Minute, 0),
			Cluster:    &v1.SettingsSpec{Limits: authzCeiling(5*time.Minute, 0)},
		})

		assert.Equal(t, 5*time.Minute, got.Authz.ApprovalTimeout)
		assert.Equal(t, "clamped", got.Provenance["authz.approvalTimeout"])
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonAuthzClamped, vs[0].Reason)
		assert.False(t, vs[0].Fatal, "a clamp tightens; it must not block the session")
		assert.Contains(t, vs[0].Message, "approvalTimeout")
	})

	t.Run("class asks 30m leakage TTL over a 2m namespace ceiling: clamped to 2m", func(t *testing.T) {
		got, vs := Resolve(Inputs{
			ClassAuthz: classAuthz(0, 30*time.Minute),
			Namespace:  &v1.SettingsSpec{Limits: authzCeiling(0, 2*time.Minute)},
		})

		assert.Equal(t, 2*time.Minute, got.Authz.InformationLeakageApprovalTTL)
		assert.Equal(t, "clamped", got.Provenance["authz.informationLeakageApprovalTTL"])
		require.Len(t, vs, 1)
		assert.Contains(t, vs[0].Message, "informationLeakageApprovalTTL")
	})

	t.Run("two tiers set a ceiling: the shortest wins", func(t *testing.T) {
		got, _ := Resolve(Inputs{
			ClassAuthz: classAuthz(30*time.Minute, 0),
			Cluster:    &v1.SettingsSpec{Limits: authzCeiling(20*time.Minute, 0)},
			Namespace:  &v1.SettingsSpec{Limits: authzCeiling(6*time.Minute, 0)},
		})

		assert.Equal(t, 6*time.Minute, got.Authz.ApprovalTimeout)
	})

	t.Run("a namespace cannot widen the cluster ceiling", func(t *testing.T) {
		got, _ := Resolve(Inputs{
			ClassAuthz: classAuthz(30*time.Minute, 0),
			Cluster:    &v1.SettingsSpec{Limits: authzCeiling(5*time.Minute, 0)},
			Namespace:  &v1.SettingsSpec{Limits: authzCeiling(25*time.Minute, 0)},
		})

		assert.Equal(t, 5*time.Minute, got.Authz.ApprovalTimeout)
	})

	t.Run("class asks less than the ceiling: kept as asked, no violation", func(t *testing.T) {
		got, vs := Resolve(Inputs{
			ClassAuthz: classAuthz(90*time.Second, 0),
			Cluster:    &v1.SettingsSpec{Limits: authzCeiling(5*time.Minute, 0)},
		})

		assert.Equal(t, 90*time.Second, got.Authz.ApprovalTimeout)
		assert.Equal(t, "class", got.Provenance["authz.approvalTimeout"])
		assert.Empty(t, vs)
	})

	t.Run("ceiling below the built-in default with nothing requested: caps silently", func(t *testing.T) {
		// Nothing was reduced from a request, so there is no warning — but the
		// built-in 10m must not survive a 3m ceiling.
		got, vs := Resolve(Inputs{
			Cluster: &v1.SettingsSpec{Limits: authzCeiling(3*time.Minute, 3*time.Minute)},
		})

		assert.Equal(t, 3*time.Minute, got.Authz.ApprovalTimeout)
		assert.Equal(t, 3*time.Minute, got.Authz.InformationLeakageApprovalTTL)
		assert.Empty(t, vs)
	})

	t.Run("ceiling ABOVE the built-in default never widens it", func(t *testing.T) {
		// The trap resolveDim would spring: with nothing requested, a ceiling
		// must not become the value when a built-in default already exists.
		got, _ := Resolve(Inputs{
			Cluster: &v1.SettingsSpec{Limits: authzCeiling(30*time.Minute, 30*time.Minute)},
		})

		assert.Equal(t, 10*time.Minute, got.Authz.ApprovalTimeout)
		assert.Equal(t, 10*time.Minute, got.Authz.InformationLeakageApprovalTTL)
	})

	t.Run("an inherited tier default over the ceiling is clamped silently", func(t *testing.T) {
		got, vs := Resolve(Inputs{
			Cluster: &v1.SettingsSpec{
				Defaults: &v1.SettingsDefaults{Authz: &v1.DefaultAuthz{ApprovalTimeout: durp("20m")}},
				Limits:   authzCeiling(4*time.Minute, 0),
			},
		})

		assert.Equal(t, 4*time.Minute, got.Authz.ApprovalTimeout)
		assert.Equal(t, "clamped", got.Provenance["authz.approvalTimeout"])
		assert.Empty(t, vs, "the class asked for nothing, so nothing was denied it")
	})
}

func TestResolve_AuthzProvenance(t *testing.T) {
	got, _ := Resolve(Inputs{
		ClassAuthz: classAuthz(7*time.Minute, 0),
		Namespace: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Authz: &v1.DefaultAuthz{
			InformationLeakageApprovalTTL: durp("4m"),
			ScopeMaxLLMLatencyMs:          int32p(2500),
		}}},
	})

	assert.Equal(t, "class", got.Provenance["authz.approvalTimeout"])
	assert.Equal(t, "namespace", got.Provenance["authz.informationLeakageApprovalTTL"])
	assert.Equal(t, "namespace", got.Provenance["authz.scopeMaxLlmLatencyMs"])
	assert.Equal(t, int32(2500), got.Authz.ScopeMaxLLMLatencyMs)
}
