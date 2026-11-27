//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
)

// setDemand is one session's pod set as DemandFor prices it: the runner pod
// plus one sandbox pod per tool bundle.
func setDemand(session string, bundles ...string) admission.Demand {
	d := admission.Demand{Pods: []admission.PodDemand{{Name: session + "-runner", CPUMilli: 100, MemBytes: 1 << 20}}}
	for _, b := range bundles {
		d.Pods = append(d.Pods, admission.PodDemand{
			Name: session + "-" + b + "-pod", CPUMilli: 100, MemBytes: 1 << 20, SandboxSession: session + "-" + b,
		})
	}
	return d
}

// TestFixedSetCapacityCountsWholeSets pins that the harness model charges one
// set per SESSION, however Decide splits that session's pods across Reserve
// calls. Decide reserves a holder's unplaced pods one at a time; a model that
// charged a set per call let one three-pod holder fill a two-set cluster, so
// the second arrival waited for the first to leave.
func TestFixedSetCapacityCountsWholeSets(t *testing.T) {
	holder := setDemand("first", "gitlike", "scratch")
	model, err := FixedSetCapacity{Sets: 2}.Snapshot(context.Background())
	require.NoError(t, err)

	key := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: "default", Name: n} }
	dec := admission.Decide(admission.Input{
		Holders: []admission.Holder{{Key: key("first"), Line: "default/reviewer", Demand: holder, Unplaced: holder}},
		Waiting: []admission.Candidate{
			{Key: key("second"), Line: "default/reviewer", Demand: setDemand("second", "gitlike", "scratch")},
			{Key: key("third"), Line: "default/reviewer", Demand: setDemand("third", "gitlike", "scratch")},
		},
		Model: model,
	})

	assert.Equal(t, []types.NamespacedName{key("second")}, dec.Admit,
		"a two-set cluster with one holder has room for exactly one more session")
	assert.Equal(t, admission.Position{Index: 1, LineLength: 1}, dec.Positions[key("third")])
}
