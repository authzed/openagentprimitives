//go:build darwin && arm64

package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/health"
)

func TestClassifyHealth(t *testing.T) {
	results := []namedResult{
		{Name: "spicebox-operator", Res: health.Result{Status: health.OK, Detail: "1/1 ready"}},
		{Name: "spicebox-webd", Res: health.Result{Status: health.Failed, Detail: "0/1 ready (CrashLoopBackOff)"}},
		{Name: "spicebox-postgres", Res: health.Result{Status: health.Failed, Detail: "deployment not found: x", NotFound: true}},
		{Name: "spicebox-graphiti", Res: health.Result{Status: health.Optional, Detail: "0/1 ready"}},
	}
	lines, unhealthy := classifyHealth(results)

	// NotFound (postgres) is dropped; the other three remain, in order.
	assert.Equal(t, []componentHealth{
		{Name: "spicebox-operator", Healthy: true, Optional: false, Detail: "1/1 ready"},
		{Name: "spicebox-webd", Healthy: false, Optional: false, Detail: "0/1 ready (CrashLoopBackOff)"},
		{Name: "spicebox-graphiti", Healthy: false, Optional: true, Detail: "0/1 ready"},
	}, lines)

	// Only the required-unhealthy component drives the alarm set.
	assert.Equal(t, map[string]string{"spicebox-webd": "0/1 ready (CrashLoopBackOff)"}, unhealthy)
}

func TestHealthDiff(t *testing.T) {
	cases := []struct {
		name          string
		prev, cur     map[string]string
		wantNewlyBad  []string
		wantRecovered bool
	}{
		{name: "newly bad", prev: map[string]string{}, cur: map[string]string{"webd": "x"}, wantNewlyBad: []string{"webd"}, wantRecovered: false},
		{name: "stays bad: no repeat", prev: map[string]string{"webd": "x"}, cur: map[string]string{"webd": "y"}, wantNewlyBad: nil, wantRecovered: false},
		{name: "recovered", prev: map[string]string{"webd": "x"}, cur: map[string]string{}, wantNewlyBad: nil, wantRecovered: true},
		{name: "no change, all healthy", prev: map[string]string{}, cur: map[string]string{}, wantNewlyBad: nil, wantRecovered: false},
		{name: "two newly bad sorted", prev: map[string]string{}, cur: map[string]string{"nats": "x", "webd": "y"}, wantNewlyBad: []string{"nats", "webd"}, wantRecovered: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newlyBad, recovered := healthDiff(tc.prev, tc.cur)
			assert.Equal(t, tc.wantNewlyBad, newlyBad)
			assert.Equal(t, tc.wantRecovered, recovered)
		})
	}
}

func TestHealthTitle(t *testing.T) {
	assert.Equal(t, "Running", healthTitle(map[string]string{}))
	assert.Equal(t, "1 component unhealthy", healthTitle(map[string]string{"webd": "x"}))
	assert.Equal(t, "2 components unhealthy", healthTitle(map[string]string{"webd": "x", "nats": "y"}))
}

func TestHealthLineLabel(t *testing.T) {
	assert.Equal(t, "✓ spicebox-operator — 1/1 ready",
		healthLineLabel(componentHealth{Name: "spicebox-operator", Healthy: true, Detail: "1/1 ready"}))
	assert.Equal(t, "✗ spicebox-webd — 0/1 ready (CrashLoopBackOff)",
		healthLineLabel(componentHealth{Name: "spicebox-webd", Healthy: false, Detail: "0/1 ready (CrashLoopBackOff)"}))
	assert.Equal(t, "⚠ spicebox-graphiti — 0/1 ready (optional)",
		healthLineLabel(componentHealth{Name: "spicebox-graphiti", Healthy: false, Optional: true, Detail: "0/1 ready"}))
}

func TestMaxHealthMenuItems_CoversRegistry(t *testing.T) {
	// The bounded submenu pool must fit every registered component, or the tail
	// of the registry would silently never render. Add a slot (or trim the
	// registry) if this fails.
	assert.GreaterOrEqual(t, maxHealthMenuItems, len(health.All()),
		"maxHealthMenuItems must be >= number of registered components")
}

func TestHealthNotifications(t *testing.T) {
	// disabled: never notify
	msgs := healthNotifications(map[string]string{}, map[string]string{"spicebox-webd": "x"}, false)
	assert.Empty(t, msgs)

	// enabled + newly bad: one "unhealthy" message naming the component
	msgs = healthNotifications(map[string]string{}, map[string]string{"spicebox-webd": "x"}, true)
	assert.Equal(t, []notification{{Title: "OAP Desktop — unhealthy", Message: "spicebox-webd not ready"}}, msgs)

	// enabled + recovered: one "recovered" message
	msgs = healthNotifications(map[string]string{"spicebox-webd": "x"}, map[string]string{}, true)
	assert.Equal(t, []notification{{Title: "OAP Desktop — recovered", Message: "all components healthy"}}, msgs)

	// enabled + stays bad: no message
	msgs = healthNotifications(map[string]string{"spicebox-webd": "x"}, map[string]string{"spicebox-webd": "y"}, true)
	assert.Empty(t, msgs)

	// two newly bad: sorted, comma-joined in one message
	msgs = healthNotifications(map[string]string{}, map[string]string{"spicebox-nats": "x", "spicebox-webd": "y"}, true)
	assert.Equal(t, []notification{{Title: "OAP Desktop — unhealthy", Message: "spicebox-nats, spicebox-webd not ready"}}, msgs)
}
