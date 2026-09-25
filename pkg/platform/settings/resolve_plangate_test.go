package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// planGateCase describes one resolution: a default and a floor at each of the
// two settings tiers, plus what the AgentClass asked for.
type planGateCase struct {
	name                         string
	clusterDefault, clusterFloor string
	nsDefault, nsFloor           string
	class                        string
	want                         string
}

func planGateInputs(t *testing.T, tc planGateCase) Inputs {
	t.Helper()

	tier := func(def, floor string) *v1.SettingsSpec {
		if def == "" && floor == "" {
			return nil
		}
		s := &v1.SettingsSpec{}
		if def != "" {
			s.Defaults = &v1.SettingsDefaults{
				Authz: &v1.DefaultAuthz{PlanGate: &v1.PlanGateConfig{Mode: def}},
			}
		}
		if floor != "" {
			s.Limits = &v1.SettingsLimits{MinPlanGateMode: floor}
		}
		return s
	}

	in := Inputs{
		Cluster:   tier(tc.clusterDefault, tc.clusterFloor),
		Namespace: tier(tc.nsDefault, tc.nsFloor),
	}
	if tc.class != "" {
		in.ClassAuthz = &v1.AuthzBlock{PlanGate: &v1.PlanGateConfig{Mode: tc.class}}
	}
	return in
}

// The plan gate resolves in two steps: pick the mode (class, else the nearest
// tier default, else disabled), then clamp it UP to the strictest floor any
// tier declares. Default and floor are different fields with different intents
// and neither substitutes for the other.
func TestResolveAuthz_planGateDefaultThenClampedToFloor(t *testing.T) {
	cases := []planGateCase{
		{name: "nothing set anywhere: disabled",
			want: "disabled"},

		{name: "cluster default only: class inherits it",
			clusterDefault: "logging", want: "logging"},

		{name: "namespace default overrides the cluster default",
			clusterDefault: "disabled", nsDefault: "logging", want: "logging"},

		{name: "class overrides the default upward",
			clusterDefault: "logging", class: "enforcing", want: "enforcing"},

		// The row that must NOT be "fixed" into a clamp: a default is what you
		// get when you say nothing, so a class may still say something quieter.
		// The floor is the admin's tool for forbidding that, and it is a
		// separate, deliberate act.
		{name: "class overrides the default DOWNWARD with no floor: allowed",
			clusterDefault: "logging", class: "disabled", want: "disabled"},

		{name: "floor set: class cannot go below it",
			clusterDefault: "logging", clusterFloor: "logging", class: "disabled", want: "logging"},

		{name: "floor enforcing: a disabled class is clamped all the way up",
			clusterFloor: "enforcing", class: "disabled", want: "enforcing"},

		{name: "class stricter than the floor: class wins",
			clusterFloor: "logging", class: "enforcing", want: "enforcing"},

		{name: "namespace floor tightens the cluster floor",
			clusterFloor: "disabled", nsFloor: "logging", class: "disabled", want: "logging"},

		// A lower tier may only tighten. A namespace declaring a laxer floor
		// than the cluster's must not loosen it.
		{name: "namespace floor cannot loosen the cluster floor",
			clusterFloor: "enforcing", nsFloor: "disabled", class: "disabled", want: "enforcing"},

		{name: "floor with no default and no class: the floor is the answer",
			clusterFloor: "logging", want: "logging"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := resolveAuthz(planGateInputs(t, tc), map[string]string{})
			require.NotNil(t, got.PlanGate, "PlanGate must always resolve to a value")
			assert.Equal(t, tc.want, got.PlanGate.Mode)
		})
	}
}

func TestStrictestMode(t *testing.T) {
	cases := []struct{ name, a, b, want string }{
		{"disabled vs logging: logging", "disabled", "logging", "logging"},
		{"logging vs enforcing: enforcing", "logging", "enforcing", "enforcing"},
		{"enforcing vs disabled: enforcing", "enforcing", "disabled", "enforcing"},
		{"equal values: unchanged", "logging", "logging", "logging"},
		{"empty is treated as unset, not as disabled", "", "logging", "logging"},
		{"both empty: disabled", "", "", "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, strictestMode(tc.a, tc.b))
			assert.Equal(t, tc.want, strictestMode(tc.b, tc.a), "must be commutative")
		})
	}
}

// An unrecognized mode string is a config error. Reading it as "disabled" would
// turn a typo into a silent security downgrade, so it ranks at the STRICTEST
// end — the operator gets a gate that is too tight, which they will notice,
// rather than one that is silently off, which they will not.
func TestStrictestMode_unknownValueFailsClosed(t *testing.T) {
	for _, unknown := range []string{"Enforcing", "enabled", "on", "logging ", "off"} {
		t.Run("unknown "+unknown+" outranks disabled", func(t *testing.T) {
			assert.NotEqual(t, "disabled", strictestMode(unknown, "disabled"),
				"an unparseable mode must never resolve to disabled")
		})
	}
}

// ToStatus is where a newly-added resolved field silently disappears: the
// resolver computes it, the struct carries it, and the conversion to the CRD
// status quietly drops it — with every resolver unit test still green, because
// they assert on the pre-conversion value.
//
// That is exactly what happened to PlanGate, and only the e2e suite caught it.
// This test closes the gap at unit speed.
func TestToStatus_carriesPlanGateThrough(t *testing.T) {
	e, _ := Resolve(Inputs{
		ClassAuthz: &v1.AuthzBlock{PlanGate: &v1.PlanGateConfig{Mode: "logging"}},
	})
	require.NotNil(t, e.Authz.PlanGate, "the resolver must produce a plan-gate config")

	st := e.ToStatus()
	require.NotNil(t, st.Authz.PlanGate,
		"ToStatus must carry PlanGate into the status snapshot — the runner reads it from there")
	assert.Equal(t, "logging", st.Authz.PlanGate.Mode)
}

// The clamped value, not the class's request, is what reaches the runner.
func TestToStatus_carriesTheClampedModeNotTheClassRequest(t *testing.T) {
	e, _ := Resolve(Inputs{
		Cluster:    &v1.SettingsSpec{Limits: &v1.SettingsLimits{MinPlanGateMode: "enforcing"}},
		ClassAuthz: &v1.AuthzBlock{PlanGate: &v1.PlanGateConfig{Mode: "disabled"}},
	})

	st := e.ToStatus()
	require.NotNil(t, st.Authz.PlanGate)
	assert.Equal(t, "enforcing", st.Authz.PlanGate.Mode,
		"the floor must survive the conversion, or a class could opt out past it")
}
